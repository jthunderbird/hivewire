// Package web serves the browser UI and the SSE event stream.
//
// The server binds LAN-wide by default and is intentionally unauthenticated:
// anyone who can reach the port can read the agent transcripts it streams.
// That is a deliberate dev-box choice — do not expose it beyond a trusted LAN.
package web

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/hub"
	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider/claudecode"
	"github.com/jtaylor/hivewire/internal/provider/codex"
	"github.com/jtaylor/hivewire/internal/provider/omp"
	"github.com/jtaylor/hivewire/internal/provider/opencode"
	"github.com/jtaylor/hivewire/internal/store"
)

//go:embed assets
var assets embed.FS

// Server wires the hub and history store to HTTP.
type Server struct {
	Hub   *hub.Hub
	Store *store.Store
	// Roots bounds which directories overflow files may be read from.
	Roots []string
}

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/stream", s.stream)
	mux.HandleFunc("/api/agents", s.agents)
	mux.HandleFunc("/api/agent", s.agent)
	mux.HandleFunc("/api/history", s.history)
	mux.HandleFunc("/api/replay", s.replay)
	mux.HandleFunc("/api/overflow", s.overflow)
	mux.HandleFunc("/api/focus", s.focus)
	return cors(mux)
}

// cors allows any origin: the UI is meant to be opened from other machines on
// the LAN, so locking it to localhost would defeat the purpose.
func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// stream pushes hub frames as Server-Sent Events. SSE is one-way and reconnects
// on its own, which is all this UI needs.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	frames, cancel := s.Hub.Subscribe(256)
	defer cancel()

	keepalive := time.NewTicker(20 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		case f, ok := <-frames:
			if !ok {
				return
			}
			b, err := json.Marshal(f)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
	}
}

func (s *Server) agents(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.Hub.Agents())
}

func (s *Server) agent(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	a, ok := s.Hub.Agent(id)
	if !ok {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	writeJSON(w, struct {
		Agent  model.Agent   `json:"agent"`
		Events []model.Event `json:"events"`
	}{a, s.Hub.Events(id)})
}

// history returns a page of indexed runs. ?q= filters across every record (not
// just the current page), ?offset=/?limit= page the result.
func (s *Server) history(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		limit = 50
	}
	records, total := s.Store.Search(q.Get("q"), offset, limit)
	if records == nil {
		records = []store.Record{}
	}
	writeJSON(w, struct {
		Records []store.Record `json:"records"`
		Total   int            `json:"total"`
		Offset  int            `json:"offset"`
	}{records, total, offset})
}

// replay rebuilds a past agent's stream by re-reading its original transcript.
func (s *Server) replay(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	rec, ok := s.Store.Get(id)
	if !ok {
		http.Error(w, "unknown agent", http.StatusNotFound)
		return
	}
	var (
		a      model.Agent
		events []model.Event
		err    error
	)
	switch rec.Provider {
	case claudecode.Name:
		a, events, err = claudecode.Replay(rec.Source)
	case codex.Name:
		a, events, err = codex.Replay(rec.Source)
	case omp.Name:
		a, events, err = omp.Replay(rec.Source)
	case opencode.Name:
		// OpenCode transcripts are database rows, so replay needs the session ID
		// the index kept rather than a file path.
		if rec.NativeID == "" {
			http.Error(w, "missing session id", http.StatusBadRequest)
			return
		}
		a, events, err = opencode.Replay(rec.Source, rec.NativeID)
	default:
		http.Error(w, "unknown provider", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, struct {
		Agent  model.Agent   `json:"agent"`
		Events []model.Event `json:"events"`
	}{a, events})
}

// overflow serves output the agent harness truncated before writing the
// transcript. Paths are confined to the configured transcript roots.
func (s *Server) overflow(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.Query().Get("path")
	path, err := s.resolve(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if _, err := io.Copy(w, f); err != nil {
		log.Printf("overflow copy: %v", err)
	}
}

// resolve confines a requested path to the allowed roots, resolving symlinks
// first so a link cannot walk out of them.
func (s *Server) resolve(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("missing path")
	}
	clean := filepath.Clean(raw)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("path must be absolute")
	}
	real, err := filepath.EvalSymlinks(clean)
	if err != nil {
		return "", fmt.Errorf("unresolvable path")
	}
	for _, root := range s.Roots {
		rr, err := filepath.EvalSymlinks(root)
		if err != nil {
			rr = root
		}
		if real == rr || strings.HasPrefix(real, strings.TrimRight(rr, string(os.PathSeparator))+string(os.PathSeparator)) {
			return real, nil
		}
	}
	return "", fmt.Errorf("path outside allowed roots")
}

func (s *Server) focus(w http.ResponseWriter, r *http.Request) {
	slot, err := strconv.Atoi(r.URL.Query().Get("slot"))
	if err != nil {
		http.Error(w, "bad slot", http.StatusBadRequest)
		return
	}
	s.Hub.Focus(slot, r.URL.Query().Get("id"))
	w.WriteHeader(http.StatusNoContent)
}
