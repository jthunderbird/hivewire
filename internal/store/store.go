// Package store keeps a browsable index of every agent hivewire has seen.
//
// It deliberately stores no transcript content: every supported agent CLI keeps
// its own history indefinitely, so this is an index over records that already
// exist and replay reads the original transcript or database rows back.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
)

// Record is one indexed agent run.
type Record struct {
	ID       string       `json:"id"`
	NativeID string       `json:"nativeId,omitempty"`
	Provider string       `json:"provider"`
	Model    string       `json:"model"`
	Name     string       `json:"name"`
	Nickname string       `json:"nickname,omitempty"`
	Title    string       `json:"title"`
	Prompt   string       `json:"prompt,omitempty"`
	Cwd      string       `json:"cwd,omitempty"`
	Source   string       `json:"source"`
	Started  time.Time    `json:"started"`
	Updated  time.Time    `json:"updated"`
	Status   model.Status `json:"status"`
	Tokens   model.Tokens `json:"tokens"`
	Tools    int          `json:"tools"`
}

// Store is a JSON-backed index, flushed lazily.
type Store struct {
	path string

	mu    sync.Mutex
	recs  map[string]Record
	dirty bool
}

// Open loads the index at path, creating an empty one if absent.
func Open(path string) (*Store, error) {
	s := &Store{path: path, recs: map[string]Record{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil
		}
		return s, err
	}
	var list []Record
	if err := json.Unmarshal(b, &list); err != nil {
		// A corrupt index is rebuildable, so start clean rather than fail.
		return s, nil
	}
	for _, r := range list {
		s.recs[r.ID] = r
	}
	return s, nil
}

// Upsert records or refreshes one agent.
func (s *Store) Upsert(a model.Agent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.recs[a.ID]
	rec := Record{
		ID: a.ID, NativeID: a.NativeID, Provider: a.Provider, Model: a.Model, Name: a.Name,
		Nickname: a.Nickname, Title: a.Title, Prompt: a.Prompt, Cwd: a.Cwd, Source: a.Source,
		Started: a.Started, Updated: a.Updated, Status: a.Status,
		Tokens: a.Tokens, Tools: a.ToolCount,
	}
	// A backlog agent is indexed without being streamed, so it has no prompt to
	// contribute; never let that erase one recorded earlier.
	if rec.Prompt == "" {
		rec.Prompt = prev.Prompt
	}
	s.recs[a.ID] = rec
	s.dirty = true
}

// List returns every record, newest first.
func (s *Store) List() []Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// Search returns records matching every whitespace-separated term in q, newest
// first, along with the total number of matches before paging. Terms are matched
// case-insensitively against the title, prompt, agent name, model, provider and
// working directory. An empty q matches everything.
func (s *Store) Search(q string, offset, limit int) (matches []Record, total int) {
	terms := strings.Fields(strings.ToLower(q))
	all := s.List()

	for _, r := range all {
		if matchesAll(r, terms) {
			matches = append(matches, r)
		}
	}
	total = len(matches)

	if offset >= len(matches) {
		return nil, total
	}
	matches = matches[offset:]
	if limit > 0 && limit < len(matches) {
		matches = matches[:limit]
	}
	return matches, total
}

func matchesAll(r Record, terms []string) bool {
	if len(terms) == 0 {
		return true
	}
	hay := strings.ToLower(strings.Join([]string{
		r.Title, r.Prompt, r.Name, r.Nickname, r.Model, r.Provider, r.Cwd, string(r.Status),
	}, "\x00"))
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// Get returns one record by id.
func (s *Store) Get(id string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	return r, ok
}

// Flush writes the index to disk if anything changed.
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	list := make([]Record, 0, len(s.recs))
	for _, r := range s.recs {
		list = append(list, r)
	}
	s.dirty = false
	s.mu.Unlock()

	sort.Slice(list, func(i, j int) bool { return list[i].Started.After(list[j].Started) })
	b, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// RunFlusher periodically persists the index until ctx is done.
func (s *Store) RunFlusher(done <-chan struct{}, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-done:
			_ = s.Flush()
			return
		case <-t.C:
			_ = s.Flush()
		}
	}
}
