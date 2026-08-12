// Package opencode adapts OpenCode child sessions stored in its SQLite database.
package opencode

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

// Name is the provider identifier used in agent IDs and the UI.
const Name = "opencode"

// Provider polls one OpenCode database for child-session changes.
type Provider struct {
	db             *database
	idleDone       time.Duration
	since          time.Time
	agents         map[string]*agentTail
	lastNormalized pollStats
	now            func() time.Time
}

type agentTail struct {
	agent                 model.Agent
	state                 map[string]emittedPart
	roles                 map[string]string
	authority             messageAuthority
	messageWatches        map[string]string
	partWatches           map[string]string
	partMessages          map[string]string
	message               rowCursor
	part                  rowCursor
	backlogTools          int
	dormant               bool
	dormantSince          int64
	dormantSessionUpdated int64
}

type messageAuthority struct {
	id        string
	created   int64
	updated   int64
	data      messageData
	malformed bool
}

// New returns a provider for path. Persisted activity strictly before since is
// indexed as backlog; activity at the cutoff is live.
func New(path string, idleDone time.Duration, since time.Time) *Provider {
	return &Provider{
		db:       newDatabase(path),
		idleDone: idleDone,
		since:    since,
		agents:   make(map[string]*agentTail),
		now:      time.Now,
	}
}

func (p *Provider) Name() string { return Name }

// Poll reads one consistent database snapshot and emits changed child sessions.
func (p *Provider) Poll() ([]provider.Update, error) {
	p.lastNormalized = pollStats{}
	snapshot, err := p.db.pollSnapshot(context.Background(), func(session sessionRow) pollRequest {
		if p.db.replaced {
			return pollRequest{}
		}
		at, exists := p.agents[Name+":"+session.id]
		if !exists {
			return pollRequest{}
		}
		if at.agent.Backlog {
			// OpenCode touches session.time_updated when a session resumes. Rely on
			// that indexed metadata signal so dormant backlog never scans content.
			if session.timeUpdated < p.since.UnixMilli() {
				return pollRequest{skip: true}
			}
			return pollRequest{updatedSince: p.since.UnixMilli()}
		}
		if at.dormant {
			if session.timeUpdated <= at.dormantSessionUpdated {
				if len(at.messageWatches) == 0 && len(at.partWatches) == 0 {
					return pollRequest{skip: true}
				}
				return pollRequest{skipRows: true, messageWatches: at.messageWatches, partWatches: at.partWatches}
			}
			return pollRequest{updatedSince: at.dormantSince + 1, messageWatches: at.messageWatches, partWatches: at.partWatches}
		}
		return pollRequest{messages: at.message, parts: at.part, messageWatches: at.messageWatches, partWatches: at.partWatches}
	})
	if err != nil {
		return nil, err
	}
	if p.db.replaced {
		p.agents = make(map[string]*agentTail)
	}
	for _, session := range snapshot.sessions {
		if _, err := sessionDepth(session, snapshot.sessions); err != nil {
			return nil, err
		}
	}
	if snapshot.present {
		seen := make(map[string]bool, len(snapshot.sessions))
		for _, session := range snapshot.sessions {
			seen[Name+":"+session.id] = true
		}
		for id := range p.agents {
			if !seen[id] {
				delete(p.agents, id)
			}
		}
	}

	var updates []provider.Update
	for _, session := range snapshot.sessions {
		id := Name + ":" + session.id
		at, exists := p.agents[id]
		if exists {
			pruneMissingRows(at, snapshot.missingMessages[session.id], snapshot.missingParts[session.id])
		}
		changedMessages := snapshot.messages[session.id]
		changedParts := snapshot.parts[session.id]
		if exists && at.agent.Backlog && len(changedMessages) == 0 && len(changedParts) == 0 {
			continue
		}
		if exists && at.dormant && len(changedMessages) == 0 && len(changedParts) == 0 {
			if session.timeUpdated <= at.dormantSessionUpdated {
				continue
			}
			updated, err := agentFromSession(session, snapshot.sessions, p.db.path, at.agent)
			if err != nil {
				return nil, err
			}
			updated.Status = at.agent.Status
			updated.DurationMS = agentDuration(updated)
			at.dormantSessionUpdated = session.timeUpdated
			if updated != at.agent {
				at.agent = updated
				updates = append(updates, provider.Update{Agent: updated})
			}
			continue
		}
		if exists && !at.agent.Backlog && len(changedMessages) == 0 && len(changedParts) == 0 {
			updated, err := agentFromSession(session, snapshot.sessions, p.db.path, at.agent)
			if err != nil {
				return nil, err
			}
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
				if updated.Status == model.StatusDone || updated.Status == model.StatusError {
					at.makeDormant(updated.Updated.UnixMilli(), session.timeUpdated, false)
				}
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
			messages = appendRoleContext(at.roles, messages, parts)
		}
		dormantSessionAdvanced := exists && at.dormant && session.timeUpdated > at.dormantSessionUpdated
		if exists && at.dormant {
			messages = messagesAfterCutoff(changedMessages, at.dormantSince, at.messageWatches)
			parts = partsAfterCutoff(changedParts, at.dormantSince, at.partWatches)
		}
		if exists && !at.agent.Backlog {
			messages = appendRoleContext(at.roles, messages, parts)
		}
		priorAgent := model.Agent{}
		if exists {
			priorAgent = at.agent
			if session.agent == "" {
				session.agent = at.agent.Name
			}
		}
		p.lastNormalized.messageRows += len(changedMessages)
		p.lastNormalized.partRows += len(changedParts)
		activity := sessionActivity(session, messages, parts)
		backlog := !exists && activity.Before(p.since)
		if exists && at.agent.Backlog {
			backlog = activity.Before(p.since)
		}
		if exists && at.dormant {
			backlog = false
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
		var currentAuthority messageAuthority
		if exists {
			currentAuthority = at.authority
		}
		nextAuthority := updateAuthority(currentAuthority, changedMessages)
		if nextAuthority.id != "" {
			normalized.status, normalized.statusTime = authorityStatus(nextAuthority)
		}
		if exists && at.dormant && !dormantSessionAdvanced {
			normalized.status = at.agent.Status
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
				normalized.agent.ToolCount = at.agent.ToolCount + countNewToolUses(events)
				if at.agent.Prompt != "" {
					normalized.agent.Prompt = at.agent.Prompt
				}
				if normalized.agent.Name == "" {
					normalized.agent.Name = at.agent.Name
				}
				if normalized.agent.Model == "" {
					normalized.agent.Model = at.agent.Model
				}
				normalized.agent.Updated = laterTime(at.agent.Updated, normalized.agent.Updated)
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
			at = &agentTail{roles: make(map[string]string)}
			p.agents[id] = at
		}
		at.agent = normalized.agent
		if normalized.agent.Backlog {
			at.backlogTools = normalized.agent.ToolCount
			at.state = nil
			at.roles = make(map[string]string)
			updateRoles(at.roles, changedMessages)
			if normalized.agent.Name == "" {
				normalized.agent.Name = priorAgent.Name
			}
			at.authority = messageAuthority{}
			at.message = rowCursor{}
			at.part = rowCursor{}
		} else {
			at.dormant = false
			if at.roles == nil {
				at.roles = make(map[string]string)
			}
			updateRoles(at.roles, changedMessages)
			at.authority = nextAuthority
			at.state = mergeEmissionState(at.state, normalized.state)
			at.message = advanceMessageCursor(at.message, changedMessages, nil)
			at.part = advancePartCursor(at.part, changedParts, at.state)
			at.messageWatches = malformedMessageWatches(at.messageWatches, changedMessages, at.state, at.authority.id)
			at.messageWatches = authorityWatches(at.messageWatches, at.authority, changedMessages)
			at.partWatches, at.partMessages = unresolvedPartWatches(at.partWatches, at.partMessages, changedParts, at.state, at.roles)
			if at.agent.Status == model.StatusDone || at.agent.Status == model.StatusError {
				at.makeDormant(at.agent.Updated.UnixMilli(), session.timeUpdated, true)
			}
		}
		if changed {
			updates = append(updates, provider.Update{Agent: at.agent, Events: events})
		}
	}
	return updates, nil
}

func (at *agentTail) makeDormant(cutoff, sessionUpdated int64, preserveWatches bool) {
	at.dormant = true
	at.dormantSince = cutoff
	at.dormantSessionUpdated = sessionUpdated
	at.messageWatches = malformedWatchesOnly(at.messageWatches, at.state)
	if preserveWatches && (len(at.messageWatches) > 0 || len(at.partWatches) > 0) {
		at.state = compactWatchedState(at.state, at.messageWatches, at.partWatches)
		at.roles = compactWatchedRoles(at.roles, at.partWatches, at.partMessages)
		at.partMessages = compactWatchedPartMessages(at.partWatches, at.partMessages)
		at.authority = messageAuthority{}
		at.message = rowCursor{}
		at.part = rowCursor{}
		return
	}
	at.state = nil
	at.roles = nil
	at.authority = messageAuthority{}
	at.messageWatches = nil
	at.partWatches = nil
	at.partMessages = nil
	at.message = rowCursor{}
	at.part = rowCursor{}
}

func malformedWatchesOnly(watches map[string]string, state map[string]emittedPart) map[string]string {
	result := make(map[string]string)
	for id, fingerprint := range watches {
		if state[id].malformed != "" {
			result[id] = fingerprint
		}
	}
	return result
}

func compactWatchedState(state map[string]emittedPart, messages, parts map[string]string) map[string]emittedPart {
	result := make(map[string]emittedPart, len(messages)+len(parts))
	for id := range messages {
		result[id] = state[id]
	}
	for id := range parts {
		result[id] = state[id]
	}
	return result
}

func compactWatchedRoles(roles map[string]string, parts, messages map[string]string) map[string]string {
	result := make(map[string]string)
	for partID := range parts {
		if messageID := messages[partID]; roles[messageID] != "" {
			result[messageID] = roles[messageID]
		}
	}
	return result
}

func compactWatchedPartMessages(parts, messages map[string]string) map[string]string {
	result := make(map[string]string, len(parts))
	for partID := range parts {
		result[partID] = messages[partID]
	}
	return result
}

func pruneMissingRows(at *agentTail, messages, parts []string) {
	for _, id := range messages {
		delete(at.messageWatches, id)
		delete(at.roles, id)
		delete(at.state, id)
		if at.authority.id == id {
			at.authority = messageAuthority{}
			at.agent.Status = model.StatusLive
		}
	}
	for _, id := range parts {
		delete(at.partWatches, id)
		delete(at.partMessages, id)
		delete(at.state, id)
	}
}

func messagesAfterCutoff(rows []messageRow, cutoff int64, watched map[string]string) []messageRow {
	result := make([]messageRow, 0, len(rows))
	for _, row := range rows {
		if row.timeCreated > cutoff || row.timeUpdated > cutoff || watched[row.id] != "" {
			result = append(result, row)
		}
	}
	return result
}

func partsAfterCutoff(rows []partRow, cutoff int64, watched map[string]string) []partRow {
	result := make([]partRow, 0, len(rows))
	for _, row := range rows {
		if row.timeCreated > cutoff || row.timeUpdated > cutoff || watched[row.id] != "" {
			result = append(result, row)
		}
	}
	return result
}

func countNewToolUses(events []model.Event) int {
	count := 0
	for _, event := range events {
		if event.Kind == model.EvToolUse {
			count++
		}
	}
	return count
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

func agentFromSession(session sessionRow, sessions []sessionRow, source string, prior model.Agent) (model.Agent, error) {
	depth, err := sessionDepth(session, sessions)
	if err != nil {
		return model.Agent{}, err
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
	return prior, nil
}

func advanceMessageCursor(cursor rowCursor, rows []messageRow, _ map[string]emittedPart) rowCursor {
	for _, row := range rows {
		cursor = advanceCursor(cursor, row.id, row.timeUpdated, row.data)
	}
	return cursor
}

func advancePartCursor(cursor rowCursor, rows []partRow, state map[string]emittedPart) rowCursor {
	for _, row := range rows {
		cursor = advanceCursor(cursor, row.id, row.timeUpdated, row.data)
	}
	return cursor
}

func advanceCursor(cursor rowCursor, id string, updated int64, data string) rowCursor {
	if updated > cursor.updated {
		cursor.updated = updated
		cursor.frontier = make(map[string]string)
	}
	if updated == cursor.updated {
		if cursor.frontier == nil {
			cursor.frontier = make(map[string]string)
		}
		cursor.frontier[id] = rowFingerprint(data)
	}
	return cursor
}

func appendRoleContext(roles map[string]string, messages []messageRow, parts []partRow) []messageRow {
	present := make(map[string]bool, len(messages))
	for _, row := range messages {
		present[row.id] = true
	}
	for _, part := range parts {
		if !present[part.messageID] && roles[part.messageID] != "" {
			messages = append(messages, messageRow{id: part.messageID, sessionID: part.sessionID, data: `{"role":"` + roles[part.messageID] + `"}`})
			present[part.messageID] = true
		}
	}
	return messages
}

func updateRoles(roles map[string]string, rows []messageRow) {
	for _, row := range rows {
		var data messageData
		if json.Unmarshal([]byte(row.data), &data) == nil && data.Role != "" {
			roles[row.id] = data.Role
		}
	}
}

func updateAuthority(current messageAuthority, rows []messageRow) messageAuthority {
	for _, row := range rows {
		if row.timeCreated < current.created || row.timeCreated == current.created && row.id < current.id {
			continue
		}
		current = messageAuthority{id: row.id, created: row.timeCreated, updated: row.timeUpdated}
		current.malformed = json.Unmarshal([]byte(row.data), &current.data) != nil
	}
	return current
}

func authorityWatches(watches map[string]string, authority messageAuthority, rows []messageRow) map[string]string {
	if watches == nil {
		watches = make(map[string]string)
	}
	if authority.id == "" {
		return watches
	}
	for _, row := range rows {
		if row.id == authority.id {
			watches[row.id] = rowFingerprint(row.data)
			return watches
		}
	}
	return watches
}

func unresolvedPartWatches(watches, messages map[string]string, rows []partRow, state map[string]emittedPart, roles map[string]string) (map[string]string, map[string]string) {
	if watches == nil {
		watches = make(map[string]string)
	}
	if messages == nil {
		messages = make(map[string]string)
	}
	for _, row := range rows {
		s := state[row.id]
		if partCanStillEmit(row, s, roles[row.messageID]) {
			watches[row.id] = rowFingerprint(row.data)
			messages[row.id] = row.messageID
		} else {
			delete(watches, row.id)
			delete(messages, row.id)
		}
	}
	return watches, messages
}

func malformedMessageWatches(watches map[string]string, rows []messageRow, state map[string]emittedPart, authorityID string) map[string]string {
	if watches == nil {
		watches = make(map[string]string)
	}
	for id := range watches {
		if id != authorityID && state[id].malformed == "" {
			delete(watches, id)
		}
	}
	for _, row := range rows {
		var data messageData
		if json.Unmarshal([]byte(row.data), &data) != nil && state[row.id].malformed != "" {
			watches[row.id] = rowFingerprint(row.data)
		} else {
			delete(watches, row.id)
		}
	}
	return watches
}

func partCanStillEmit(row partRow, state emittedPart, role string) bool {
	var header partHeader
	if json.Unmarshal([]byte(row.data), &header) != nil {
		return state.malformed != ""
	}
	var data partData
	if json.Unmarshal([]byte(row.data), &data) != nil {
		return state.malformed != ""
	}
	switch header.Type {
	case "text":
		if role == "user" {
			return !state.user
		}
		return role == "assistant" && !state.textDone
	case "reasoning":
		return !state.reasoningDone
	case "tool":
		terminal := data.State.Status == "completed" || data.State.Status == "error" || data.State.Status == "errored"
		return !state.toolUse || !terminal && !state.toolResult
	default:
		return false
	}
}

func authorityStatus(authority messageAuthority) (model.Status, time.Time) {
	if authority.malformed {
		return model.StatusLive, unixMillis(firstPositive(authority.updated, authority.created))
	}
	return messageStatus(messageRow{id: authority.id, timeCreated: authority.created, timeUpdated: authority.updated}, authority.data)
}

func mergeEmissionState(prior, changed map[string]emittedPart) map[string]emittedPart {
	if prior == nil {
		prior = make(map[string]emittedPart, len(changed))
	}
	for id, emitted := range changed {
		emitted.fingerprint = ""
		prior[id] = emitted
	}
	return prior
}

func (p *Provider) settle(status model.Status, statusTime, activity time.Time) (model.Status, time.Time) {
	if status != model.StatusLive || p.idleDone <= 0 || activity.IsZero() {
		return status, statusTime
	}
	deadline := activity.Add(p.idleDone)
	if !p.now().Before(deadline) {
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
