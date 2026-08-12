// Command hivewire streams live Claude Code, Codex and OpenCode subagent
// activity into a terminal UI and a LAN-reachable web page, four panes at a
// time.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jtaylor/hivewire/internal/config"
	"github.com/jtaylor/hivewire/internal/hub"
	"github.com/jtaylor/hivewire/internal/provider"
	"github.com/jtaylor/hivewire/internal/provider/claudecode"
	"github.com/jtaylor/hivewire/internal/provider/codex"
	"github.com/jtaylor/hivewire/internal/provider/opencode"
	"github.com/jtaylor/hivewire/internal/store"
	"github.com/jtaylor/hivewire/internal/tui"
	"github.com/jtaylor/hivewire/internal/web"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "hivewire:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.Args[1:])
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StateDir, 0o755); err != nil {
		return err
	}

	// Stderr belongs to the TUI once it starts, so logs go to a file instead.
	logPath := cfg.LogFile
	if logPath == "" && cfg.TUI {
		logPath = filepath.Join(cfg.StateDir, "hivewire.log")
	}
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open log: %w", err)
		}
		defer f.Close()
		log.SetOutput(f)
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("hivewire ")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := hub.New(cfg.Slots, cfg.BufferBytes)
	idx, err := store.Open(cfg.IndexPath())
	if err != nil {
		log.Printf("history index: %v (starting empty)", err)
	}

	// Transcripts already on disk at launch are indexed as history; only agents
	// that appear from now on take a live pane.
	since := time.Now()
	providers := providersFor(cfg, time.Duration(cfg.IdleDoneSec)*time.Second, since)

	done := make(chan struct{})
	go idx.RunFlusher(done, 5*time.Second)
	go poll(ctx, providers, h, idx, time.Duration(cfg.PollMS)*time.Millisecond)

	webURL := ""
	if cfg.Web {
		srv := &web.Server{
			Hub:   h,
			Store: idx,
			Roots: overflowRoots(cfg),
		}
		addr := net.JoinHostPort(cfg.Addr, fmt.Sprint(cfg.Port))
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return fmt.Errorf("listen %s: %w", addr, err)
		}
		httpSrv := &http.Server{Handler: srv.Handler()}
		go func() {
			if err := httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				log.Printf("web server: %v", err)
			}
		}()
		defer httpSrv.Close()

		webURL = fmt.Sprintf("http://%s/", hostForURL(cfg.Addr, cfg.Port))
		banner := fmt.Sprintf("web UI on http://%s/  (unauthenticated, reachable from the LAN)", addr)
		log.Print(banner)
		if !cfg.TUI {
			fmt.Println("hivewire:", banner)
		}
	}

	if cfg.TUI {
		m := tui.New(h, cfg.LayoutPath(), webURL)
		defer m.Close()
		p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithContext(ctx))
		if _, err := p.Run(); err != nil {
			close(done)
			return err
		}
		close(done)
		return nil
	}

	if !cfg.Web {
		return fmt.Errorf("both --tui and --web are disabled; nothing to do")
	}
	<-ctx.Done()
	close(done)
	return nil
}

// hostForURL turns a bind address into something a human can paste. A wildcard
// bind is reported as this machine's LAN address where one can be found.
func hostForURL(addr string, port int) string {
	if addr != "0.0.0.0" && addr != "" && addr != "::" {
		return net.JoinHostPort(addr, fmt.Sprint(port))
	}
	host := "localhost"
	if ifaces, err := net.InterfaceAddrs(); err == nil {
		for _, a := range ifaces {
			ipnet, ok := a.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() || ipnet.IP.To4() == nil {
				continue
			}
			host = ipnet.IP.String()
			break
		}
	}
	return net.JoinHostPort(host, fmt.Sprint(port))
}

// providersFor builds the provider set hivewire polls.
func providersFor(cfg config.Config, idle time.Duration, since time.Time) []provider.Provider {
	return []provider.Provider{
		claudecode.New(cfg.ClaudeRoot, idle, since),
		codex.New(cfg.CodexRoot, idle, since),
		opencode.New(cfg.OpenCodeDB, idle, since),
	}
}

// overflowRoots lists the directories the web server may read truncated tool
// output from. Claude Code and Codex keep those files beside their transcripts,
// while OpenCode keeps them in a tool-output directory beside its database. Only
// that directory is allowed, because the database's own directory also holds
// credentials.
func overflowRoots(cfg config.Config) []string {
	return []string{
		filepath.Dir(cfg.ClaudeRoot),
		filepath.Dir(cfg.CodexRoot),
		filepath.Join(filepath.Dir(cfg.OpenCodeDB), "tool-output"),
	}
}

// poll drives every provider on a ticker and feeds the hub.
func poll(ctx context.Context, providers []provider.Provider, h *hub.Hub, idx *store.Store, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, p := range providers {
				updates, err := p.Poll()
				if err != nil {
					log.Printf("%s poll: %v", p.Name(), err)
					continue
				}
				for _, u := range updates {
					h.Apply(u)
					idx.Upsert(u.Agent)
				}
			}
		}
	}
}
