package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultOpenCodeDatabase(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("XDG_DATA_HOME", "")

	cfg := Default()
	want := filepath.Join("/home/tester", ".local", "share", "opencode", "opencode.db")
	if cfg.OpenCodeDB != want {
		t.Fatalf("OpenCodeDB = %q, want %q", cfg.OpenCodeDB, want)
	}
}

func TestDefaultOpenCodeDatabaseUsesXDGDataHome(t *testing.T) {
	t.Setenv("HOME", "/home/tester")
	t.Setenv("XDG_DATA_HOME", "/var/data")

	cfg := Default()
	want := filepath.Join("/var/data", "opencode", "opencode.db")
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
