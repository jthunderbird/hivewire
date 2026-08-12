// Package config loads hivewire settings from defaults, an optional TOML file,
// and command-line flags (in that order of increasing precedence).
package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Config is the full runtime configuration.
type Config struct {
	Slots       int    `toml:"slots"`
	Web         bool   `toml:"web"`
	TUI         bool   `toml:"tui"`
	Addr        string `toml:"addr"`
	Port        int    `toml:"port"`
	BufferBytes int64  `toml:"buffer_bytes"`
	PollMS      int    `toml:"poll_ms"`
	IdleDoneSec int    `toml:"idle_done_sec"`
	ClaudeRoot  string `toml:"claude_root"`
	CodexRoot   string `toml:"codex_root"`
	OpenCodeDB  string `toml:"opencode_db"`
	OmpRoot     string `toml:"omp_root"`
	StateDir    string `toml:"state_dir"`
	LogFile     string `toml:"log_file"`
}

// Default returns the built-in configuration.
func Default() Config {
	home, _ := os.UserHomeDir()
	return Config{
		Slots:       4,
		Web:         true,
		TUI:         true,
		Addr:        "0.0.0.0",
		Port:        8787,
		BufferBytes: 8 << 20,
		PollMS:      250,
		IdleDoneSec: 300,
		ClaudeRoot:  filepath.Join(home, ".claude", "projects"),
		CodexRoot:   filepath.Join(home, ".codex", "sessions"),
		OpenCodeDB:  filepath.Join(openCodeDataDir(home), "opencode.db"),
		OmpRoot:     filepath.Join(home, ".omp", "agent", "sessions"),
		StateDir:    filepath.Join(home, ".local", "state", "hivewire"),
	}
}

func openCodeDataDir(home string) string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "opencode")
	}
	return filepath.Join(home, ".local", "share", "opencode")
}

// Path returns the default config file location.
func Path() string {
	if v := os.Getenv("HIVEWIRE_CONFIG"); v != "" {
		return v
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "hivewire", "config.toml")
}

// Load resolves configuration from the config file then the given arguments.
func Load(args []string) (Config, error) {
	cfg := Default()

	cfgPath := Path()
	for i, a := range args {
		if a == "--config" && i+1 < len(args) {
			cfgPath = args[i+1]
		}
	}
	if b, err := os.ReadFile(cfgPath); err == nil {
		if err := toml.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", cfgPath, err)
		}
	} else if !os.IsNotExist(err) {
		return cfg, fmt.Errorf("read %s: %w", cfgPath, err)
	}

	fs := flag.NewFlagSet("hivewire", flag.ContinueOnError)
	fs.IntVar(&cfg.Slots, "slots", cfg.Slots, "number of display slots")
	fs.BoolVar(&cfg.Web, "web", cfg.Web, "serve the web UI")
	fs.BoolVar(&cfg.TUI, "tui", cfg.TUI, "run the terminal UI")
	fs.StringVar(&cfg.Addr, "addr", cfg.Addr, "web bind address")
	fs.IntVar(&cfg.Port, "port", cfg.Port, "web port")
	fs.Int64Var(&cfg.BufferBytes, "buffer-bytes", cfg.BufferBytes, "per-agent ring buffer budget")
	fs.IntVar(&cfg.PollMS, "poll-ms", cfg.PollMS, "transcript poll interval in milliseconds")
	fs.IntVar(&cfg.IdleDoneSec, "idle-done-sec", cfg.IdleDoneSec, "seconds of silence before an agent is assumed finished")
	fs.StringVar(&cfg.ClaudeRoot, "claude-root", cfg.ClaudeRoot, "Claude Code projects directory")
	fs.StringVar(&cfg.CodexRoot, "codex-root", cfg.CodexRoot, "Codex sessions directory")
	fs.StringVar(&cfg.OpenCodeDB, "opencode-db", cfg.OpenCodeDB, "OpenCode SQLite database")
	fs.StringVar(&cfg.OmpRoot, "omp-root", cfg.OmpRoot, "omp sessions directory")
	fs.StringVar(&cfg.StateDir, "state-dir", cfg.StateDir, "directory for history index and layout")
	fs.StringVar(&cfg.LogFile, "log-file", cfg.LogFile, "write logs here instead of stderr (required with --tui)")
	fs.String("config", cfgPath, "path to config.toml")
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if cfg.Slots < 1 {
		cfg.Slots = 1
	}
	return cfg, nil
}

// LayoutPath is where TUI pane proportions persist between runs.
func (c Config) LayoutPath() string { return filepath.Join(c.StateDir, "layout.json") }

// IndexPath is where the browsable history index lives.
func (c Config) IndexPath() string { return filepath.Join(c.StateDir, "index.json") }
