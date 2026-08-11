package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCodeDataDirFallback(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "")

	home := filepath.Join("test", "home")
	want := filepath.Join(home, ".local", "share", "opencode")
	if got := openCodeDataDir(home); got != want {
		t.Fatalf("openCodeDataDir(%q) = %q, want %q", home, got, want)
	}
}

func TestOpenCodeDataDirUsesXDGDataHome(t *testing.T) {
	dataHome := filepath.Join("test", "data")
	t.Setenv("XDG_DATA_HOME", dataHome)

	want := filepath.Join(dataHome, "opencode")
	if got := openCodeDataDir(filepath.Join("unused", "home")); got != want {
		t.Fatalf("openCodeDataDir() = %q, want %q", got, want)
	}
}

func TestDefaultOpenCodeDatabase(t *testing.T) {
	dataHome := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dataHome)

	cfg := Default()
	want := filepath.Join(dataHome, "opencode", "opencode.db")
	if cfg.OpenCodeDB != want {
		t.Fatalf("OpenCodeDB = %q, want %q", cfg.OpenCodeDB, want)
	}
}

func TestOpenCodeDatabaseFlagOverridesDefault(t *testing.T) {
	t.Setenv("HIVEWIRE_CONFIG", filepath.Join(t.TempDir(), "missing.toml"))

	cfg, err := Load([]string{"--opencode-db", "/tmp/custom.db"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenCodeDB != "/tmp/custom.db" {
		t.Fatalf("OpenCodeDB = %q, want %q", cfg.OpenCodeDB, "/tmp/custom.db")
	}
}

func TestOpenCodeDatabaseFromTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("opencode_db = '/tmp/from-config.db'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HIVEWIRE_CONFIG", path)

	cfg, err := Load(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OpenCodeDB != "/tmp/from-config.db" {
		t.Fatalf("OpenCodeDB = %q, want %q", cfg.OpenCodeDB, "/tmp/from-config.db")
	}
}
