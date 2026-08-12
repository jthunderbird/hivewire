package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/config"
)

func TestProvidersForCoversEverySupportedCLI(t *testing.T) {
	cfg := config.Config{ClaudeRoot: "/c/projects", CodexRoot: "/x/sessions", OpenCodeDB: "/o/opencode.db", OmpRoot: "/m/sessions"}
	var names []string
	for _, p := range providersFor(cfg, time.Minute, time.Now()) {
		names = append(names, p.Name())
	}
	want := []string{"claude", "codex", "opencode", "omp"}
	if len(names) != len(want) {
		t.Fatalf("provider names = %v, want %v", names, want)
	}
	for i, name := range want {
		if names[i] != name {
			t.Fatalf("provider names = %v, want %v", names, want)
		}
	}
}

func TestOverflowRootsExposeToolOutputButNotTheOpenCodeDataDirectory(t *testing.T) {
	cfg := config.Config{
		ClaudeRoot: filepath.Join("/home/u", ".claude", "projects"),
		CodexRoot:  filepath.Join("/home/u", ".codex", "sessions"),
		OpenCodeDB: filepath.Join("/srv/state/opencode", "opencode.db"),
		OmpRoot:    filepath.Join("/home/u", ".omp", "agent", "sessions"),
	}
	roots := overflowRoots(cfg)
	want := []string{
		filepath.Join("/home/u", ".claude"),
		filepath.Join("/home/u", ".codex"),
		filepath.Join("/srv/state/opencode", "tool-output"),
	}
	if len(roots) != len(want) {
		t.Fatalf("roots = %v, want %v", roots, want)
	}
	for i := range want {
		if roots[i] != want[i] {
			t.Fatalf("roots = %v, want %v", roots, want)
		}
	}
	for _, root := range roots {
		if root == filepath.Dir(cfg.OpenCodeDB) {
			t.Fatalf("OpenCode data directory %q is exposed; it also holds auth.json", root)
		}
		// omp keeps an auth database beside its sessions, and truncates no tool
		// output to disk, so none of its directories belong here.
		if strings.Contains(root, ".omp") {
			t.Fatalf("omp directory %q is exposed", root)
		}
	}
}
