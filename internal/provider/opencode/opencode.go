// Package opencode adapts OpenCode child sessions stored in its SQLite database.
package opencode

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

// Name is the provider identifier used in agent IDs and the UI.
const Name = "opencode"

// Provider polls one OpenCode database for child-session changes.
type Provider struct {
	db       *database
	idleDone time.Duration
	since    time.Time
	agents   map[string]*agentTail
}

type agentTail struct {
	agent        model.Agent
	state        map[string]emittedPart
	messages     map[string]messageRow
	parts        map[string]partRow
	message      rowCursor
	part         rowCursor
	backlogTools int
}

// New returns a provider for path. Persisted activity strictly before since is
// indexed as backlog; activity at the cutoff is live.
func New(path string, idleDone time.Duration, since time.Time) *Provider {
	return &Provider{
		db:       newDatabase(path),
		idleDone: idleDone,
		since:    since,
		agents:   make(map[string]*agentTail),
	}
}

func (p *Provider) Name() string { return Name }

// Poll reads one consistent database snapshot and emits changed child sessions.
func (p *Provider) Poll() ([]provider.Update, error) {
	snapshot, err := p.db.pollSnapshot(context.Background(), func(session sessionRow) pollRequest {
		if p.db.replaced {
			return pollRequest{}
		}
		at, exists := p.agents[Name+":"+session.id]
		if !exists {
			return pollRequest{}
		}
		if at.agent.Backlog {
			if session.timeUpdated < p.since.UnixMilli() {
				return pollRequest{skip: true}
			}
			return pollRequest{updatedSince: p.since.UnixMilli()}
		}
		return pollRequest{messages: at.message, parts: at.part}
	})
	if err != nil {
		return nil, err
	}
	if p.db.replaced {
		p.agents = make(map[string]*agentTail)
	}

	var updates []provider.Update
	for _, session := range snapshot.sessions {
		id := Name + ":" + session.id
		at, exists := p.agents[id]
		changedMessages := snapshot.messages[session.id]
		changedParts := snapshot.parts[session.id]
		if exists && at.agent.Backlog && len(changedMessages) == 0 && len(changedParts) == 0 {
			continue
		}
		if exists && !at.agent.Backlog && len(changedMessages) == 0 && len(changedParts) == 0 {
			updated := agentFromSession(session, snapshot.sessions, p.db.path, at.agent)
			status, statusTime := p.settle(updated.Status, updated.Updated, updated.Updated)
			var events []model.Event
			if status != at.agent.Status {
				updated.Status = status
				updated.EventCount++
				events = append(events, statusEvent(id, status, statusTime))
			}
			updated.DurationMS = agentDuration(updated)
			if updated != at.agent || len(events) > 0 {
				at.agent = updated
				updates = append(updates, provider.Update{Agent: updated, Events: events})
			}
			continue
		}
		var prior map[string]emittedPart
		if exists {
			prior = at.state
		}
		messages, parts := changedMessages, changedParts
		if exists && at.agent.Backlog {
			messages = messagesCreatedSince(changedMessages, p.since.UnixMilli())
			parts = partsCreatedSince(changedParts, p.since.UnixMilli())
		}
		if exists && !at.agent.Backlog {
			mergeMessages(at.messages, changedMessages)
			mergeParts(at.parts, changedParts)
			messages = messageValues(at.messages)
			parts = partValues(at.parts)
		}
		activity := sessionActivity(session, messages, parts)
		backlog := !exists && activity.Before(p.since)
		if exists && at.agent.Backlog {
			backlog = activity.Before(p.since)
		}

		normalized, err := normalizeSessionMode(
			session,
			snapshot.sessions,
			messages,
			parts,
			p.db.path,
			prior,
			!backlog,
			nil,
		)
		if err != nil {
			return nil, err
		}

		events := normalized.events
		if backlog {
			normalized.agent.Backlog = true
			normalized.agent.Status = model.StatusDone
			if exists {
				normalized.agent.EventCount = at.agent.EventCount
			}
			normalized.agent.EventCount += normalized.eventCount
			events = nil
		} else {
			previousStatus := model.StatusLive
			if exists {
				previousStatus = at.agent.Status
				normalized.agent.EventCount = at.agent.EventCount
				normalized.agent.ToolCount += at.backlogTools
				if at.agent.Prompt != "" {
					normalized.agent.Prompt = at.agent.Prompt
				}
			}
			normalized.agent.Backlog = false
			status, statusTime := p.settle(normalized.status, normalized.statusTime, activity)
			normalized.agent.Status = status
			if status != previousStatus {
				events = append(events, statusEvent(id, status, statusTime))
			}
			normalized.agent.EventCount += len(events)
		}

		normalized.agent.DurationMS = agentDuration(normalized.agent)
		changed := !exists || normalized.agent != at.agent || len(events) > 0
		if !exists {
			at = &agentTail{messages: make(map[string]messageRow), parts: make(map[string]partRow)}
			p.agents[id] = at
		}
		at.agent = normalized.agent
		if normalized.agent.Backlog {
			at.backlogTools = normalized.agent.ToolCount
			at.state = nil
			at.messages = nil
			at.parts = nil
			at.message = rowCursor{}
			at.part = rowCursor{}
		} else {
			at.state = normalized.state
			if at.messages == nil {
				at.messages = make(map[string]messageRow)
				at.parts = make(map[string]partRow)
			}
			mergeMessages(at.messages, changedMessages)
			mergeParts(at.parts, changedParts)
			at.message = advanceMessageCursor(at.message, changedMessages)
			at.part = advancePartCursor(at.part, changedParts)
			at.message.watched = messageWatches(at.messages)
			at.part.watched = partWatches(at.messages, at.parts, at.state)
		}
		if changed {
			updates = append(updates, provider.Update{Agent: at.agent, Events: events})
		}
	}
	return updates, nil
}

func messagesCreatedSince(rows []messageRow, since int64) []messageRow {
	result := make([]messageRow, 0, len(rows))
	for _, row := range rows {
		if row.timeCreated >= since {
			result = append(result, row)
		}
	}
	return result
}

func partsCreatedSince(rows []partRow, since int64) []partRow {
	result := make([]partRow, 0, len(rows))
	for _, row := range rows {
		if row.timeCreated >= since {
			result = append(result, row)
		}
	}
	return result
}

func agentFromSession(session sessionRow, sessions []sessionRow, source string, prior model.Agent) model.Agent {
	depth, err := sessionDepth(session, sessions)
	if err != nil {
		depth = prior.Depth
	}
	prior.NativeID = session.id
	prior.Provider = Name
	prior.Parent = session.parentID
	prior.Depth = depth
	prior.Cwd = session.directory
	prior.CLIVersion = session.version
	prior.Source = source
	if session.agent != "" {
		prior.Name = session.agent
	}
	prior.Title = session.title
	if prior.Name != "" {
		prior.Title = strings.TrimSuffix(prior.Title, " (@"+prior.Name+" subagent)")
	}
	if session.model != "" {
		prior.Model = session.model
	}
	prior.Tokens = model.Tokens{
		In: int(session.tokensInput), Out: int(session.tokensOutput), Reasoning: int(session.tokensReasoning),
		CacheRead: int(session.tokensCacheRead), CacheWrite: int(session.tokensCacheWrite),
		Total: int(session.tokensInput + session.tokensOutput),
	}
	prior.Updated = laterTime(prior.Updated, unixMillis(session.timeUpdated))
	return prior
}

func mergeMessages(dst map[string]messageRow, rows []messageRow) {
	for _, row := range rows {
		dst[row.id] = row
	}
}

func mergeParts(dst map[string]partRow, rows []partRow) {
	for _, row := range rows {
		dst[row.id] = row
	}
}

func messageValues(rows map[string]messageRow) []messageRow {
	result := make([]messageRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].timeCreated < result[j].timeCreated || result[i].timeCreated == result[j].timeCreated && result[i].id < result[j].id
	})
	return result
}

func partValues(rows map[string]partRow) []partRow {
	result := make([]partRow, 0, len(rows))
	for _, row := range rows {
		result = append(result, row)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].timeCreated < result[j].timeCreated || result[i].timeCreated == result[j].timeCreated && result[i].id < result[j].id
	})
	return result
}

func advanceMessageCursor(cursor rowCursor, rows []messageRow) rowCursor {
	for _, row := range rows {
		cursor = advanceCursor(cursor, row.id, row.timeUpdated, row.data)
	}
	return cursor
}

func advancePartCursor(cursor rowCursor, rows []partRow) rowCursor {
	for _, row := range rows {
		cursor = advanceCursor(cursor, row.id, row.timeUpdated, row.data)
	}
	return cursor
}

func advanceCursor(cursor rowCursor, id string, updated int64, data string) rowCursor {
	if updated > cursor.updated {
		cursor.updated = updated
		cursor.boundary = make(map[string]string)
	}
	if updated == cursor.updated {
		if cursor.boundary == nil {
			cursor.boundary = make(map[string]string)
		}
		cursor.boundary[id] = data
	}
	return cursor
}

func messageWatches(rows map[string]messageRow) map[string]string {
	result := make(map[string]string, len(rows))
	for id, row := range rows {
		result[id] = row.data
	}
	return result
}

func partWatches(messages map[string]messageRow, parts map[string]partRow, state map[string]emittedPart) map[string]string {
	roles := make(map[string]string, len(messages))
	for id, row := range messages {
		var data messageData
		if json.Unmarshal([]byte(row.data), &data) == nil {
			roles[id] = data.Role
		}
	}
	result := make(map[string]string)
	for id, row := range parts {
		emitted := state[id]
		if emitted.malformed != "" {
			result[id] = row.data
			continue
		}
		var header partHeader
		if json.Unmarshal([]byte(row.data), &header) != nil {
			result[id] = row.data
			continue
		}
		switch header.Type {
		case "text":
			if roles[row.messageID] == "user" && !emitted.user || roles[row.messageID] == "assistant" && !emitted.textDone {
				result[id] = row.data
			}
		case "reasoning":
			if !emitted.reasoningDone {
				result[id] = row.data
			}
		case "tool":
			if !emitted.toolResult {
				result[id] = row.data
			}
		}
	}
	return result
}

func (p *Provider) settle(status model.Status, statusTime, activity time.Time) (model.Status, time.Time) {
	if status != model.StatusLive || p.idleDone <= 0 || activity.IsZero() {
		return status, statusTime
	}
	deadline := activity.Add(p.idleDone)
	if !time.Now().Before(deadline) {
		return model.StatusDone, deadline
	}
	return status, statusTime
}

func sessionActivity(session sessionRow, messages []messageRow, parts []partRow) time.Time {
	activity := unixMillis(maxMillis(session.timeCreated, session.timeUpdated))
	for _, row := range messages {
		activity = laterTime(activity, unixMillis(maxMillis(row.timeCreated, row.timeUpdated)))
	}
	for _, row := range parts {
		activity = laterTime(activity, unixMillis(maxMillis(row.timeCreated, row.timeUpdated)))
	}
	return activity
}

func statusEvent(agentID string, status model.Status, ts time.Time) model.Event {
	return model.Event{
		AgentID: agentID,
		TS:      ts,
		Kind:    model.EvStatus,
		Header:  "agent " + string(status),
		Err:     status == model.StatusError,
	}
}

func agentDuration(agent model.Agent) int64 {
	if agent.Started.IsZero() || agent.Updated.Before(agent.Started) {
		return 0
	}
	return agent.Updated.Sub(agent.Started).Milliseconds()
}

func maxMillis(a, b int64) int64 {
	if b > a {
		return b
	}
	return a
}
