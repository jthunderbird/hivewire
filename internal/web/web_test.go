package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/hub"
	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
	"github.com/jtaylor/hivewire/internal/store"

	_ "modernc.org/sqlite"
)

func newServer(t *testing.T, roots ...string) (*Server, *hub.Hub) {
	t.Helper()
	h := hub.New(4, 1<<20)
	idx, err := store.Open(filepath.Join(t.TempDir(), "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{Hub: h, Store: idx, Roots: roots}, h
}

func TestOverflowServesFilesInsideTheAllowedRoots(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "big.txt")
	if err := os.WriteFile(path, []byte("full output"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(t, root)

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/overflow?path="+path, nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "full output" {
		t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestOverflowRefusesPathsOutsideTheRoots(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(t, root)

	for _, target := range []string{outside, "/etc/passwd", root + "/../etc/passwd", "relative.txt", ""} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/overflow?path="+target, nil))
		if rr.Code == http.StatusOK {
			t.Errorf("%q was served but should have been refused", target)
		}
	}
}

func TestOverflowRefusesSymlinksEscapingTheRoots(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(secret, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	s, _ := newServer(t, root)

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/overflow?path="+link, nil))
	if rr.Code == http.StatusOK {
		t.Fatal("a symlink pointing outside the roots must not be served")
	}
}

func TestAgentEndpointReturnsMetadataAndEvents(t *testing.T) {
	s, h := newServer(t)
	h.Apply(provider.Update{
		Agent:  model.Agent{ID: "claude:x", Provider: "claude", Name: "Explore", Status: model.StatusLive, Updated: time.Now()},
		Events: []model.Event{{Kind: model.EvText, Header: "hello", Body: "hello"}},
	})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/agent?id=claude:x", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code = %d", rr.Code)
	}
	var got struct {
		Agent  model.Agent   `json:"agent"`
		Events []model.Event `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent.Name != "Explore" || len(got.Events) != 1 {
		t.Fatalf("agent=%+v events=%d", got.Agent, len(got.Events))
	}
}

func TestUnknownAgentIs404(t *testing.T) {
	s, _ := newServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/agent?id=nope", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rr.Code)
	}
}

func TestFocusSwapsSlots(t *testing.T) {
	s, h := newServer(t)
	now := time.Now()
	for _, id := range []string{"a", "b"} {
		h.Apply(provider.Update{Agent: model.Agent{ID: id, Provider: "claude", Status: model.StatusLive, Updated: now}})
	}

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/focus?slot=0&id=b", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("code = %d", rr.Code)
	}
	if got := h.Snapshot().Slots; got[0] != "b" || got[1] != "a" {
		t.Fatalf("slots = %v", got)
	}
}

func TestUIIsServedAndCORSIsOpen(t *testing.T) {
	s, _ := newServer(t)
	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("index not served: %d", rr.Code)
	}
	if rr.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Error("LAN use requires open CORS")
	}
}

// openCodeFixture writes a minimal OpenCode database holding one finished child
// session, returning its path.
func openCodeFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	schema := []string{
		`CREATE TABLE session (id TEXT PRIMARY KEY, project_id TEXT NOT NULL, parent_id TEXT, directory TEXT NOT NULL,
			title TEXT NOT NULL, version TEXT NOT NULL, time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL,
			agent TEXT, model TEXT, tokens_input INTEGER NOT NULL DEFAULT 0, tokens_output INTEGER NOT NULL DEFAULT 0,
			tokens_reasoning INTEGER NOT NULL DEFAULT 0, tokens_cache_read INTEGER NOT NULL DEFAULT 0,
			tokens_cache_write INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE message (id TEXT PRIMARY KEY, session_id TEXT NOT NULL, time_created INTEGER NOT NULL,
			time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`CREATE TABLE part (id TEXT PRIMARY KEY, message_id TEXT NOT NULL, session_id TEXT NOT NULL,
			time_created INTEGER NOT NULL, time_updated INTEGER NOT NULL, data TEXT NOT NULL)`,
		`INSERT INTO session VALUES ('ses_child','project','ses_parent','/work','Audit parser','1.15.7',1000,2000,'general','gpt-5.6-sol',10,5,0,0,0)`,
		`INSERT INTO message VALUES ('msg_user','ses_child',1100,1100,'{"role":"user"}')`,
		`INSERT INTO message VALUES ('msg_assistant','ses_child',1200,2000,'{"role":"assistant","time":{"completed":2000},"finish":"stop"}')`,
		`INSERT INTO part VALUES ('part_prompt','msg_user','ses_child',1100,1100,'{"type":"text","text":"audit the parser"}')`,
		`INSERT INTO part VALUES ('part_answer','msg_assistant','ses_child',1300,1400,'{"type":"text","text":"done","time":{"start":1300,"end":1400}}')`,
	}
	for _, stmt := range schema {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func TestReplayDispatchesOpenCodeUsingIndexedNativeID(t *testing.T) {
	dir := t.TempDir()
	path := openCodeFixture(t, dir)
	indexPath := filepath.Join(t.TempDir(), "index.json")
	idx, err := store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	idx.Upsert(model.Agent{
		ID: "opencode:ses_child", NativeID: "ses_child", Provider: "opencode",
		Source: path, Status: model.StatusDone,
	})
	if err := idx.Flush(); err != nil {
		t.Fatal(err)
	}
	// Reopen so replay uses the persisted record, not in-memory state.
	reopened, err := store.Open(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Hub: hub.New(4, 1<<20), Store: reopened}

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/replay?id=opencode:ses_child", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
	}
	var got struct {
		Agent  model.Agent   `json:"agent"`
		Events []model.Event `json:"events"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Agent.ID != "opencode:ses_child" || got.Agent.NativeID != "ses_child" || got.Agent.Title != "Audit parser" {
		t.Fatalf("replayed agent = %+v", got.Agent)
	}
	if len(got.Events) == 0 {
		t.Fatal("replay returned no events")
	}
	for _, event := range got.Events {
		if event.AgentID != got.Agent.ID {
			t.Fatalf("event agent id = %q", event.AgentID)
		}
	}
}

func TestReplayRejectsOpenCodeRecordWithoutNativeID(t *testing.T) {
	dir := t.TempDir()
	path := openCodeFixture(t, dir)
	s, _ := newServer(t)
	s.Store.Upsert(model.Agent{ID: "opencode:ses_child", Provider: "opencode", Source: path})

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/replay?id=opencode:ses_child", nil))
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("code=%d body=%q", rr.Code, rr.Body.String())
	}
}

func TestOverflowAllowsOpenCodeToolOutputButNotItsDatabaseDirectory(t *testing.T) {
	dataDir := t.TempDir()
	toolOutput := filepath.Join(dataDir, "tool-output")
	if err := os.MkdirAll(toolOutput, 0o755); err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(toolOutput, "tool_1")
	if err := os.WriteFile(allowed, []byte("full tool output"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dataDir, "auth.json")
	if err := os.WriteFile(secret, []byte(`{"token":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := newServer(t, toolOutput)

	rr := httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/overflow?path="+allowed, nil))
	if rr.Code != http.StatusOK || rr.Body.String() != "full tool output" {
		t.Fatalf("tool output code=%d body=%q", rr.Code, rr.Body.String())
	}

	rr = httptest.NewRecorder()
	s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/overflow?path="+secret, nil))
	if rr.Code == http.StatusOK {
		t.Fatal("auth.json beside the OpenCode database was served")
	}
}

func TestStaticAssetsAreRevalidatedAfterAnUpgrade(t *testing.T) {
	s, _ := newServer(t)
	for _, path := range []string{"/", "/app.js", "/style.css"} {
		rr := httptest.NewRecorder()
		s.Handler().ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s code = %d", path, rr.Code)
		}
		// Embedded files carry no Last-Modified or ETag, so without this header
		// a page loaded before a rebuild keeps serving the old UI.
		if got := rr.Header().Get("Cache-Control"); got != "no-cache, must-revalidate" {
			t.Errorf("%s Cache-Control = %q, want revalidation", path, got)
		}
	}
}
