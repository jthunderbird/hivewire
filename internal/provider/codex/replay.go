package codex

import (
	"errors"
	"path/filepath"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/tailer"
)

// Replay reads a finished rollout from disk and rebuilds its full event stream.
func Replay(path string) (model.Agent, []model.Event, error) {
	meta, ok := readSessionMeta(path)
	if !ok {
		return model.Agent{}, nil, errors.New("codex: unreadable session_meta")
	}
	spawn := meta.Source.Subagent.ThreadSpawn

	at := &agentTail{
		agent: model.Agent{
			ID:         Name + ":" + meta.ID,
			NativeID:   meta.ID,
			Provider:   Name,
			Name:       filepath.Base(spawn.AgentPath),
			Nickname:   firstNonEmpty(spawn.AgentNickname, meta.AgentNickname),
			Depth:      spawn.Depth,
			Parent:     firstNonEmpty(spawn.ParentThreadID, meta.ParentThreadID),
			Cwd:        meta.Cwd,
			CLIVersion: meta.CLIVersion,
			Source:     path,
			Status:     model.StatusDone,
		},
		tail: tailer.New(path),
	}
	if t, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
		at.agent.Started = t
	}

	var events []model.Event
	var seq uint64
	for {
		lines, err := at.tail.Poll()
		if err != nil {
			return at.agent, events, err
		}
		if len(lines) == 0 {
			break
		}
		for _, line := range lines {
			for _, e := range at.parse(line) {
				seq++
				e.Seq = seq
				e.AgentID = at.agent.ID
				if e.TS.IsZero() {
					e.TS = time.Now()
				}
				events = append(events, e)
			}
		}
	}
	at.agent.EventCount = len(events)
	return at.agent, events, nil
}
