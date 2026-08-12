package store

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
)

func seeded(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	for i, a := range []model.Agent{
		{ID: "claude:1", Provider: "claude", Name: "Explore", Model: "claude-opus-5", Title: "audit the tailer", Prompt: "look at internal/tailer and report races"},
		{ID: "codex:2", Provider: "codex", Name: "count_markdown", Model: "gpt-5.6-sol", Title: "count files", Prompt: "count the markdown files under docs"},
		{ID: "claude:3", Provider: "claude", Name: "general-purpose", Model: "claude-opus-5", Title: "fix the web server", Prompt: "the SSE stream drops frames under load"},
	} {
		a.Started = base.Add(time.Duration(i) * time.Hour)
		a.Status = model.StatusDone
		s.Upsert(a)
	}
	return s
}

func ids(recs []Record) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}

func TestSearchMatchesTitlePromptAndName(t *testing.T) {
	s := seeded(t)
	for _, tc := range []struct {
		q    string
		want string
	}{
		{"tailer", "claude:1"},        // title
		{"races", "claude:1"},         // prompt
		{"count_markdown", "codex:2"}, // agent name
		{"gpt-5.6-sol", "codex:2"},    // model
		{"SSE", "claude:3"},           // prompt, case-insensitive
	} {
		got, total := s.Search(tc.q, 0, 50)
		if total != 1 || len(got) != 1 || got[0].ID != tc.want {
			t.Errorf("Search(%q) = %v (total %d), want [%s]", tc.q, ids(got), total, tc.want)
		}
	}
}

func TestSearchRequiresEveryTerm(t *testing.T) {
	s := seeded(t)
	if _, total := s.Search("count markdown", 0, 50); total != 1 {
		t.Errorf("both terms present should match 1, got %d", total)
	}
	if _, total := s.Search("count tailer", 0, 50); total != 0 {
		t.Errorf("terms from different records should match nothing, got %d", total)
	}
}

func TestSearchSpansEveryRecordNotJustAPage(t *testing.T) {
	s := seeded(t)
	// Page size of 1 must not stop the oldest record from being found.
	got, total := s.Search("tailer", 0, 1)
	if total != 1 || len(got) != 1 || got[0].ID != "claude:1" {
		t.Fatalf("got %v total %d", ids(got), total)
	}
}

func TestEmptyQueryPagesNewestFirst(t *testing.T) {
	s := seeded(t)
	first, total := s.Search("", 0, 2)
	if total != 3 {
		t.Fatalf("total = %d, want 3", total)
	}
	if got := ids(first); len(got) != 2 || got[0] != "claude:3" {
		t.Fatalf("first page = %v, want newest first", got)
	}

	second, _ := s.Search("", 2, 2)
	if got := ids(second); len(got) != 1 || got[0] != "claude:1" {
		t.Fatalf("second page = %v", got)
	}

	if got, _ := s.Search("", 99, 2); len(got) != 0 {
		t.Fatalf("offset past the end = %v, want empty", ids(got))
	}
}

func TestBacklogUpsertKeepsAPreviouslyRecordedPrompt(t *testing.T) {
	s := seeded(t)
	// A later run re-indexes the same transcript as backlog, which carries no
	// prompt because it is never streamed.
	s.Upsert(model.Agent{ID: "claude:1", Provider: "claude", Name: "Explore", Backlog: true})

	if r, _ := s.Get("claude:1"); r.Prompt == "" {
		t.Fatal("re-indexing as backlog must not erase the recorded prompt")
	}
	if _, total := s.Search("races", 0, 50); total != 1 {
		t.Fatal("prompt search should still work after a backlog re-index")
	}
}

func TestIndexSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	s, _ := Open(path)
	s.Upsert(model.Agent{ID: "claude:1", Name: "Explore", Prompt: "hello world", Started: time.Now()})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, total := reloaded.Search("hello", 0, 50); total != 1 {
		t.Fatal("prompts should be searchable after a restart")
	}
}

func TestOpenCodeNativeIDSurvivesAReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	s.Upsert(model.Agent{ID: "opencode:ses_123", NativeID: "ses_123", Provider: "opencode"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record, ok := reloaded.Get("opencode:ses_123")
	if !ok {
		t.Fatal("reloaded store does not contain OpenCode record")
	}
	if record.NativeID != "ses_123" {
		t.Fatalf("NativeID = %q, want %q", record.NativeID, "ses_123")
	}
}

func TestOpenLoadsLegacyIndexWithoutNativeID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.json")
	if err := os.WriteFile(path, []byte(`[{"id":"claude:1","provider":"claude"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	record, ok := s.Get("claude:1")
	if !ok {
		t.Fatal("legacy record was not loaded")
	}
	if record.NativeID != "" {
		t.Fatalf("NativeID = %q, want empty", record.NativeID)
	}
}

func TestIndexKeepsNestingAndResolvesItAcrossRecords(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The nested agent is indexed before the agent that spawned it.
	s.Upsert(model.Agent{
		ID: "omp:ses-helper", NativeID: "ses-helper", Provider: "omp", Name: "scout",
		Nickname: "Helper", Depth: 2, Parent: "ses-lead", Started: time.Now(),
	})
	s.Upsert(model.Agent{
		ID: "omp:ses-lead", NativeID: "ses-lead", Provider: "omp", Name: "task",
		Nickname: "Lead", Depth: 1, Parent: "ses-session", Started: time.Now().Add(-time.Minute),
	})

	byID := map[string]Record{}
	for _, r := range s.List() {
		byID[r.ID] = r
	}
	helper := byID["omp:ses-helper"]
	if helper.Depth != 2 || helper.Parent != "ses-lead" {
		t.Fatalf("nesting not persisted: %+v", helper)
	}
	if helper.ParentLabel != "task (Lead)" {
		t.Fatalf("parent label = %q, want the spawning agent", helper.ParentLabel)
	}
	if lead := byID["omp:ses-lead"]; lead.ParentLabel != "" {
		t.Fatalf("top-level parent labelled %q", lead.ParentLabel)
	}
}
