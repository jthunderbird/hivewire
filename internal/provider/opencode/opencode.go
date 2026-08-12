// Package opencode adapts OpenCode child sessions stored in its SQLite database.
//
// OpenCode keeps every session, message and message part in
// ~/.local/share/opencode/opencode.db; it writes no per-subagent transcript, so
// unlike the Claude Code and Codex providers this one reads rows rather than
// tailing a file. Rows mutate in place — a tool part moves through pending,
// running and completed — but every mutation advances time_updated, so a
// per-session cursor over that column plus a fingerprint check at the cursor's
// own millisecond is enough to see every change exactly once.
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
	db       *database
	idleDone time.Duration
	since    time.Time
	agents   map[string]*agentTail
	now      func() time.Time

	// quietSweep bounds how many polls a settled session may be skipped before
	// it is read again. OpenCode touches session.time_updated slightly before
	// it writes the rows of a turn, so the watermark alone could miss a row
	// written after a session went quiet; re-reading on a slow cadence closes
	// that window without paying for every session on every poll.
	quietSweep int
}

// agentTail is everything we remember about one child session between polls.
type agentTail struct {
	agent     model.Agent
	state     map[string]emittedPart // row id -> phases already emitted
	roles     map[string]string      // message id -> role, for parts whose message did not change
	authority messageAuthority       // newest message, which owns lifecycle
	messages  rowCursor
	parts     rowCursor

	// quiet records that the previous poll read no rows. Combined with an
	// unchanged session.time_updated it lets the next poll skip the content
	// queries entirely, which is how a database full of finished sessions stays
	// cheap to watch. quietPolls counts how long it has been skipped.
	quiet          bool
	quietPolls     int
	skipped        bool
	sessionUpdated int64
}

// messageAuthority is the newest message seen for a session; its role, finish
// reason and error decide the agent's status.
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
		db:         newDatabase(path),
		idleDone:   idleDone,
		since:      since,
		agents:     make(map[string]*agentTail),
		now:        time.Now,
		quietSweep: defaultQuietSweep,
	}
}

// defaultQuietSweep is roughly five seconds at the default poll interval.
const defaultQuietSweep = 20

func (p *Provider) Name() string { return Name }

// Poll reads one consistent database snapshot and emits changed child sessions.
func (p *Provider) Poll() ([]provider.Update, error) {
	snapshot, err := p.db.pollSnapshot(context.Background(), func(session sessionRow) pollRequest {
		at, exists := p.agents[Name+":"+session.id]
		if !exists || p.db.replaced {
			// First sight of a session: read it whole, once.
			return pollRequest{}
		}
		if at.quiet && at.quietPolls < p.quietSweep && session.timeUpdated <= at.sessionUpdated {
			at.quietPolls++
			at.skipped = true
			return pollRequest{skip: true}
		}
		at.skipped = false
		return pollRequest{messages: at.messages, parts: at.parts}
	})
	if err != nil {
		return nil, err
	}
	if p.db.replaced {
		p.agents = make(map[string]*agentTail)
	}
	if snapshot.present {
		p.forgetDeletedSessions(snapshot.sessions)
	}

	var updates []provider.Update
	parents := parentIndex(snapshot.sessions)
	for _, session := range snapshot.sessions {
		update, err := p.applySession(session, snapshot, parents)
		if err != nil {
			return nil, err
		}
		if update != nil {
			updates = append(updates, *update)
		}
	}
	return updates, nil
}

// applySession folds one session's changed rows into its agent, returning an
// update when anything the hub can see moved.
func (p *Provider) applySession(session sessionRow, snapshot dbSnapshot, parents map[string]string) (*provider.Update, error) {
	id := Name + ":" + session.id
	at, exists := p.agents[id]
	changedMessages := snapshot.messages[session.id]
	changedParts := snapshot.parts[session.id]

	if exists && len(changedMessages) == 0 && len(changedParts) == 0 {
		// Nothing new to parse: refresh what the session row owns and let the
		// idle fallback run. This is the common case once a database fills up
		// with finished sessions, so it stays free of normalization work.
		return p.applySessionMetadata(at, session, parents)
	}

	messages, parts := changedMessages, changedParts
	var prior map[string]emittedPart
	if exists {
		prior = at.state
		if at.agent.Backlog {
			// Resuming a pre-existing session streams only what happened after
			// hivewire started; history stays in the index, not in a live pane.
			messages = messagesCreatedSince(messages, p.since.UnixMilli())
			parts = partsCreatedSince(parts, p.since.UnixMilli())
		}
		// A part can arrive without its message when only the part changed;
		// replay the role we already know so the part still normalizes.
		messages = appendRoleContext(at.roles, messages, parts)
		if session.agent == "" {
			session.agent = at.agent.Name
		}
	}

	activity := sessionActivity(session, changedMessages, changedParts)
	// A session whose whole history predates startup is indexed, not streamed,
	// and stays that way until rows created after startup arrive.
	backlog := activity.Before(p.since)
	if exists {
		backlog = at.agent.Backlog && len(messages) == 0 && len(parts) == 0
	}

	normalized, err := normalizeSessionMode(session, parents, messages, parts, p.db.path, prior, !backlog, nil)
	if err != nil {
		return nil, err
	}

	authority := messageAuthority{}
	if exists {
		authority = at.authority
	}
	authority = updateAuthority(authority, changedMessages)
	if authority.id != "" {
		normalized.status, normalized.statusTime = authorityStatus(authority)
	}

	events := normalized.events
	if backlog {
		normalized.agent.Backlog = true
		normalized.agent.Status = model.StatusDone
		normalized.agent.EventCount = normalized.eventCount
		if exists {
			normalized.agent.EventCount += at.agent.EventCount
			normalized.agent.ToolCount += at.agent.ToolCount
			normalized.agent.Updated = laterTime(at.agent.Updated, normalized.agent.Updated)
			carryResolvedMetadata(&normalized.agent, at.agent)
		}
		events = nil
	} else {
		previousStatus := model.StatusLive
		if exists {
			previousStatus = at.agent.Status
			normalized.agent.Backlog = false
			normalized.agent.EventCount = at.agent.EventCount
			normalized.agent.ToolCount = at.agent.ToolCount + countNewToolUses(events)
			normalized.agent.Updated = laterTime(at.agent.Updated, normalized.agent.Updated)
			carryResolvedMetadata(&normalized.agent, at.agent)
		}
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
	at.authority = authority
	at.state = mergeEmissionState(at.state, normalized.state)
	updateRoles(at.roles, changedMessages)
	at.messages = advanceMessageCursor(at.messages, changedMessages)
	at.parts = advancePartCursor(at.parts, changedParts)
	at.quiet = len(changedMessages) == 0 && len(changedParts) == 0
	if !at.skipped {
		at.quietPolls = 0
	}
	at.sessionUpdated = session.timeUpdated

	if !changed {
		return nil, nil
	}
	return &provider.Update{Agent: at.agent, Events: events}, nil
}

// applySessionMetadata handles a poll that read no rows for this session.
func (p *Provider) applySessionMetadata(at *agentTail, session sessionRow, parents map[string]string) (*provider.Update, error) {
	depth, err := sessionDepth(session, parents)
	if err != nil {
		return nil, err
	}
	agent := refreshFromSession(at.agent, session, depth, p.db.path)
	var events []model.Event
	if !agent.Backlog {
		activity := sessionActivity(session, nil, nil)
		activity = laterTime(activity, agent.Updated)
		if status, statusTime := p.settle(agent.Status, agent.Updated, activity); status != agent.Status {
			agent.Status = status
			events = append(events, statusEvent(agent.ID, status, statusTime))
			agent.EventCount++
		}
	}
	agent.DurationMS = agentDuration(agent)

	changed := agent != at.agent || len(events) > 0
	at.agent = agent
	at.quiet = true
	if !at.skipped {
		at.quietPolls = 0
	}
	at.sessionUpdated = session.timeUpdated
	if !changed {
		return nil, nil
	}
	return &provider.Update{Agent: agent, Events: events}, nil
}

// refreshFromSession applies the fields the session row owns, leaving anything
// only messages and parts can resolve untouched.
func refreshFromSession(agent model.Agent, session sessionRow, depth int, source string) model.Agent {
	agent.NativeID, agent.Provider = session.id, Name
	agent.Parent, agent.Depth = session.parentID, depth
	agent.Cwd, agent.CLIVersion, agent.Source = session.directory, session.version, source
	agent.Title = session.title
	if session.agent != "" {
		agent.Name = session.agent
	}
	if agent.Name != "" {
		agent.Title = strings.TrimSuffix(agent.Title, " (@"+agent.Name+" subagent)")
	}
	if resolved := sessionModelID(session.model); resolved != "" {
		agent.Model = resolved
	}
	agent.Tokens = model.Tokens{
		In: int(session.tokensInput), Out: int(session.tokensOutput), Reasoning: int(session.tokensReasoning),
		CacheRead: int(session.tokensCacheRead), CacheWrite: int(session.tokensCacheWrite),
		Total: int(session.tokensInput + session.tokensOutput),
	}
	agent.Updated = laterTime(agent.Updated, unixMillis(session.timeUpdated))
	return agent
}

// forgetDeletedSessions drops state for children that no longer exist, so a
// pruned OpenCode database does not pin memory forever.
func (p *Provider) forgetDeletedSessions(sessions []sessionRow) {
	seen := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		seen[Name+":"+session.id] = true
	}
	for id := range p.agents {
		if !seen[id] {
			delete(p.agents, id)
		}
	}
}

// carryResolvedMetadata keeps fields an earlier poll resolved from message rows
// that the current poll's rows do not mention.
func carryResolvedMetadata(agent *model.Agent, prior model.Agent) {
	// The first prompt seen is the task the agent was handed; a later user turn
	// does not replace it.
	if prior.Prompt != "" {
		agent.Prompt = prior.Prompt
	}
	if agent.Name == "" {
		agent.Name = prior.Name
	}
	if agent.Model == "" {
		agent.Model = prior.Model
	}
	if agent.Name != "" {
		agent.Title = strings.TrimSuffix(agent.Title, " (@"+agent.Name+" subagent)")
	}
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

func countNewToolUses(events []model.Event) int {
	count := 0
	for _, event := range events {
		if event.Kind == model.EvToolUse {
			count++
		}
	}
	return count
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

// advanceCursor moves the high-water mark and keeps fingerprints for the rows
// sitting exactly on it, which is what lets a same-millisecond mutation be told
// apart from a row we have already consumed.
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

// appendRoleContext synthesizes the minimal message rows a part normalization
// needs when only the part changed this poll.
func appendRoleContext(roles map[string]string, messages []messageRow, parts []partRow) []messageRow {
	present := make(map[string]bool, len(messages))
	for _, row := range messages {
		present[row.id] = true
	}
	for _, part := range parts {
		if !present[part.messageID] && roles[part.messageID] != "" {
			messages = append(messages, messageRow{
				id:        part.messageID,
				sessionID: part.sessionID,
				data:      `{"role":"` + roles[part.messageID] + `"}`,
			})
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

// updateAuthority keeps the newest message by creation time, then ID.
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
		prior[id] = emitted
	}
	return prior
}

// settle applies the idle fallback: a session the database never marks terminal
// is assumed finished after idleDone of silence.
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
