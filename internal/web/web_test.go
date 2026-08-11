package web

import (
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
