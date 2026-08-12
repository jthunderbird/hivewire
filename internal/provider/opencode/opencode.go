// Package opencode adapts OpenCode child sessions stored in its SQLite database.
package opencode

import (
	"context"
	"encoding/json"
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
	agent model.Agent
	state map[string]emittedPart
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
	snapshot, err := p.db.snapshot(context.Background())
	if err != nil {
		return nil, err
	}

	var updates []provider.Update
	for _, session := range snapshot.sessions {
		id := Name + ":" + session.id
		at, exists := p.agents[id]
		var prior map[string]emittedPart
		if exists {
			prior = at.state
		}
		normalized, err := normalizeSession(
			session,
			snapshot.sessions,
			snapshot.messages[session.id],
			snapshot.parts[session.id],
			p.db.path,
			prior,
		)
		if err != nil {
			return nil, err
		}

		activity := sessionActivity(session, snapshot.messages[session.id], snapshot.parts[session.id])
		backlog := !exists && activity.Before(p.since)
		if exists && at.agent.Backlog {
			backlog = activity.Before(p.since)
		}

		events := normalized.events
		if backlog {
			normalized.agent.Backlog = true
			normalized.agent.Status = model.StatusDone
			if exists {
				normalized.agent.EventCount = at.agent.EventCount
			}
			normalized.agent.EventCount += len(events)
			normalized.state = suppressExistingRows(
				normalized.state,
				snapshot.messages[session.id],
				snapshot.parts[session.id],
			)
			events = nil
		} else {
			previousStatus := model.StatusLive
			if exists {
				previousStatus = at.agent.Status
				normalized.agent.EventCount = at.agent.EventCount
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
			at = &agentTail{}
			p.agents[id] = at
		}
		at.agent = normalized.agent
		at.state = normalized.state
		if changed {
			updates = append(updates, provider.Update{Agent: at.agent, Events: events})
		}
	}
	return updates, nil
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

func suppressExistingRows(state map[string]emittedPart, messages []messageRow, parts []partRow) map[string]emittedPart {
	for _, row := range messages {
		emitted := state[row.id]
		if emitted.malformed == "" {
			emitted.malformed = emitted.fingerprint
		}
		state[row.id] = emitted
	}
	for _, row := range parts {
		emitted := state[row.id]
		var header partHeader
		if err := json.Unmarshal([]byte(row.data), &header); err == nil {
			switch header.Type {
			case "text":
				emitted.user = true
				emitted.textDone = true
			case "reasoning":
				emitted.reasoningDone = true
			case "tool":
				emitted.toolUse = true
				emitted.toolResult = true
			}
		}
		if emitted.malformed == "" {
			emitted.malformed = emitted.fingerprint
		}
		state[row.id] = emitted
	}
	return state
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
