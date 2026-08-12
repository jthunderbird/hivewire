package omp

import (
	"errors"
	"path/filepath"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/tailer"
)

// Replay reads a finished subagent transcript from disk and rebuilds its full
// event stream.
func Replay(path string) (model.Agent, []model.Event, error) {
	head, ok := readHeader(path)
	if !ok {
		return model.Agent{}, nil, errors.New("omp: unreadable session header")
	}

	jobID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	native := head.sessionID
	if native == "" {
		native = jobID
	}
	depth := replayDepth(path)
	at := &agentTail{
		agent: model.Agent{
			ID:       Name + ":" + native,
			NativeID: native,
			Provider: Name,
			Name:     head.agent,
			Nickname: jobID,
			Cwd:      head.cwd,
			Depth:    depth,
			Parent:   parentNativeID(path, depth),
			Source:   path,
			Started:  head.started,
			Status:   model.StatusDone,
		},
		tail:  tailer.New(path),
		jobID: jobID,
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
	at.agent.DurationMS = duration(at.agent)
	return at.agent, events, nil
}

// replayDepth counts how deep inside its session directory a transcript sits. A
// direct subagent sits in the session's own artifacts directory; a nested one
// sits in another agent's, which is not named like a session file.
func replayDepth(path string) int {
	if sessionDirRe.MatchString(filepath.Base(filepath.Dir(path))) {
		return 1
	}
	return 2
}
