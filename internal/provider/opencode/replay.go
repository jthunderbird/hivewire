package opencode

import (
	"context"
	"fmt"

	"github.com/jtaylor/hivewire/internal/model"
)

// Replay rebuilds one indexed child session's full stream from the database,
// using the same normalizer live polling uses so history matches what the pane
// showed while the agent ran.
func Replay(path, sessionID string) (model.Agent, []model.Event, error) {
	if sessionID == "" {
		return model.Agent{}, nil, fmt.Errorf("opencode: empty session id")
	}
	snapshot, err := newDatabase(path).snapshot(context.Background())
	if err != nil {
		return model.Agent{}, nil, err
	}
	if !snapshot.present {
		return model.Agent{}, nil, fmt.Errorf("opencode: database %s is missing", path)
	}

	var session sessionRow
	found := false
	for _, row := range snapshot.sessions {
		if row.id == sessionID {
			session, found = row, true
			break
		}
	}
	if !found {
		return model.Agent{}, nil, fmt.Errorf("opencode: no child session %s", sessionID)
	}

	normalized, err := normalizeSession(session, snapshot.sessions, snapshot.messages[sessionID], snapshot.parts[sessionID], path, nil)
	if err != nil {
		return model.Agent{}, nil, err
	}

	agent := normalized.agent
	agent.Status = normalized.status
	events := normalized.events
	if normalized.status == model.StatusDone || normalized.status == model.StatusError {
		events = append(events, statusEvent(agent.ID, normalized.status, normalized.statusTime))
	}
	for i := range events {
		events[i].Seq = uint64(i + 1)
		events[i].AgentID = agent.ID
	}
	agent.EventCount = len(events)
	agent.DurationMS = agentDuration(agent)
	return agent, events, nil
}
