package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/tailer"
)

// Replay reads a finished subagent transcript from disk and rebuilds its full
// event stream. Used by the history browser, which never copies transcripts —
// it re-reads the originals.
func Replay(path string) (model.Agent, []model.Event, error) {
	native := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl")
	var meta metaFile
	if b, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json"); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	sessionDir := filepath.Dir(filepath.Dir(path))

	at := &agentTail{
		agent: model.Agent{
			ID:       Name + ":" + native,
			NativeID: native,
			Provider: Name,
			Name:     meta.AgentType,
			Title:    meta.Description,
			Depth:    meta.SpawnDepth,
			Parent:   filepath.Base(sessionDir),
			Source:   path,
			Status:   model.StatusDone,
		},
		tail: tailer.New(path),
		meta: meta,
	}
	if fi, err := os.Stat(path); err == nil {
		at.agent.Started = fi.ModTime()
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
