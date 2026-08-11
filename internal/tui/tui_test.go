package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jtaylor/hivewire/internal/hub"
	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

const (
	testW = 120
	testH = 30
)

func newTestModel(t *testing.T) (*Model, *hub.Hub) {
	t.Helper()
	h := hub.New(4, 8<<20)
	m := New(h, t.TempDir()+"/layout.json", "http://192.168.1.10:8787/")
	m.Update(tea.WindowSizeMsg{Width: testW, Height: testH})
	return m, h
}

func drain(m *Model, h *hub.Hub) {
	for {
		select {
		case f := <-m.frames:
			m.applyFrame(f)
		default:
			return
		}
	}
}

func liveAgent() provider.Update {
	return provider.Update{
		Agent: model.Agent{
			ID: "claude:abc", NativeID: "abc", Provider: "claude",
			Model: "claude-opus-5", Name: "Explore", Title: "audit the tailer",
			Depth: 1, Status: model.StatusLive, Started: time.Now().Add(-8 * time.Second),
			Updated: time.Now(), ToolCount: 2,
			Tokens: model.Tokens{Total: 9939},
		},
		Events: []model.Event{
			{Kind: model.EvToolUse, Tool: "Bash", Header: "Bash  wc -l tui.go", Body: `{"command":"wc -l tui.go"}`, Lines: 1, TS: time.Now()},
			{Kind: model.EvToolResult, Header: "612 tui.go", Body: "612 tui.go", Lines: 1, TS: time.Now()},
		},
	}
}

func TestViewRendersExactlyTerminalHeight(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(liveAgent())
	drain(m, h)

	lines := strings.Split(strings.TrimRight(m.View(), "\n"), "\n")
	if len(lines) != testH {
		t.Fatalf("View rendered %d lines, want %d", len(lines), testH)
	}
}

func TestViewShowsAgentAndEvents(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(liveAgent())
	drain(m, h)

	out := m.View()
	for _, want := range []string{"claude", "opus-5", "Explore", "audit the tailer", "wc -l tui.go", "612 tui.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("View missing %q", want)
		}
	}
	if !strings.Contains(out, "slot 2 — waiting") {
		t.Error("View should label empty slots as waiting")
	}
	if !strings.Contains(out, "LIVE") {
		t.Error("a live pane must spell its status out, not rely on a glyph")
	}
}

func TestToolUseBodyFoldsUntilExpanded(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(liveAgent())
	drain(m, h)

	if strings.Contains(m.View(), `{"command"`) {
		t.Fatal("tool_use body should start folded")
	}
	m.expandAll[0] = true
	if !strings.Contains(m.View(), `{"command"`) {
		t.Fatal("tool_use body should appear once expanded")
	}
}

func TestClickTogglesTheEventUnderTheCursor(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(liveAgent())
	drain(m, h)
	m.View() // populates the hit map

	var row int
	var seq uint64
	for r, s := range m.hits[0] {
		if s != 0 {
			row, seq = r, s
			break
		}
	}
	if seq == 0 {
		t.Fatal("no clickable rows recorded")
	}

	m.mouse(tea.MouseMsg{X: 2, Y: row + 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if !m.expanded[seq] {
		t.Fatalf("click on row %d should expand event %d", row, seq)
	}
	m.View()
	m.mouse(tea.MouseMsg{X: 2, Y: row + 1, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.expanded[seq] {
		t.Fatal("second click should collapse the event again")
	}
}

func TestDraggingGuttersResizesPanes(t *testing.T) {
	m, _ := newTestModel(t)
	before := m.geometry()

	// Grab the vertical gutter and drag it left.
	m.mouse(tea.MouseMsg{X: before.leftW, Y: 5, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.drag != "main" {
		t.Fatalf("press on the column gutter should start a drag, got %q", m.drag)
	}
	m.mouse(tea.MouseMsg{X: 30, Y: 5, Action: tea.MouseActionMotion})
	if got := m.geometry().leftW; got >= before.leftW {
		t.Fatalf("drag left should shrink the left column: %d → %d", before.leftW, got)
	}
	m.mouse(tea.MouseMsg{Action: tea.MouseActionRelease})
	if m.drag != "" {
		t.Fatal("release should end the drag")
	}

	// The horizontal gutter inside the left column moves independently.
	g := m.geometry()
	m.mouse(tea.MouseMsg{X: 2, Y: g.leftTopH, Action: tea.MouseActionPress, Button: tea.MouseButtonLeft})
	if m.drag != "left" {
		t.Fatalf("press on the row gutter should start a row drag, got %q", m.drag)
	}
	m.mouse(tea.MouseMsg{X: 2, Y: 20, Action: tea.MouseActionMotion})
	if m.geometry().leftTopH <= g.leftTopH {
		t.Fatal("dragging down should grow the top-left pane")
	}
}

func TestWheelScrollsTheHoveredPane(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(liveAgent())
	drain(m, h)

	m.mouse(tea.MouseMsg{X: 2, Y: 3, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelUp})
	if m.scroll[0] == 0 {
		t.Fatal("wheel up should scroll the pane under the cursor")
	}
	m.mouse(tea.MouseMsg{X: 2, Y: 3, Action: tea.MouseActionPress, Button: tea.MouseButtonWheelDown})
	if m.scroll[0] != 0 {
		t.Fatal("wheel down should scroll back")
	}
}

func TestZoomGivesTheFocusedPaneTheWholeGrid(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(liveAgent())
	drain(m, h)

	m.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'z'}})
	g := m.geometry()
	if g.zoomSlot != 0 || g.leftW != testW {
		t.Fatalf("zoom should hand the full width to the focused pane, got slot=%d w=%d", g.zoomSlot, g.leftW)
	}
	if lines := strings.Split(strings.TrimRight(m.View(), "\n"), "\n"); len(lines) != testH {
		t.Fatalf("zoomed View rendered %d lines, want %d", len(lines), testH)
	}
}

// hugeResult reproduces the shape that pegged the render thread in the field:
// a single-line tool result of ~140 KB, which has Lines == 1 and so escaped the
// line-count fold entirely.
func hugeResult() provider.Update {
	body := strings.Repeat("x", 140*1024)
	return provider.Update{
		Agent: model.Agent{
			ID: "claude:big", Provider: "codex", Model: "gpt-5.6-sol",
			Name: "ap_today", Status: model.StatusLive, Started: time.Now(), Updated: time.Now(),
		},
		Events: []model.Event{{Kind: model.EvToolResult, Header: "fetched ap.org", Body: body, Lines: 1}},
	}
}

func TestHugeSingleLineBodyRendersFast(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(hugeResult())
	drain(m, h)

	start := time.Now()
	for i := 0; i < 20; i++ {
		m.expandVer++ // defeat the cache: every frame does the real work
		m.View()
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("20 renders of a 140 KB result took %s — the render thread would starve", d)
	}
}

// BenchmarkViewHugeResult guards the render cost of the shape that starved the
// UI in the field. Run with -benchtime to compare after renderer changes.
func BenchmarkViewHugeResult(b *testing.B) {
	h := hub.New(4, 8<<20)
	m := New(h, b.TempDir()+"/layout.json", "")
	m.Update(tea.WindowSizeMsg{Width: testW, Height: testH})
	h.Apply(hugeResult())
	for {
		select {
		case f := <-m.frames:
			m.applyFrame(f)
			continue
		default:
		}
		break
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.expandVer++
		m.View()
	}
}

func TestHugeSingleLineBodyIsFoldedByByteSize(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(hugeResult())
	drain(m, h)

	out := m.View()
	if strings.Contains(out, strings.Repeat("x", 200)) {
		t.Fatal("a 140 KB single-line body must fold, not paste into the pane")
	}
	if !strings.Contains(out, "KB") {
		t.Error("the folded header should advertise the body size")
	}
}

func TestExpandedBodyIsCappedInView(t *testing.T) {
	m, h := newTestModel(t)
	body := strings.Repeat("line\n", 5000)
	h.Apply(provider.Update{
		Agent:  model.Agent{ID: "claude:x", Provider: "claude", Status: model.StatusLive, Updated: time.Now()},
		Events: []model.Event{{Kind: model.EvToolResult, Header: "big", Body: body, Lines: 5000}},
	})
	drain(m, h)
	m.expandAll[0] = true

	lines, _ := m.body("claude:x", 0, 80)
	if len(lines) > maxPaneLines {
		t.Fatalf("pane buffer = %d lines, cap is %d", len(lines), maxPaneLines)
	}
}

func TestCacheIsReusedUntilSomethingChanges(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(liveAgent())
	drain(m, h)

	first, _ := m.body("claude:abc", 0, 80)
	second, _ := m.body("claude:abc", 0, 80)
	if &first[0] != &second[0] {
		t.Fatal("an unchanged pane should reuse its cached lines")
	}
	m.expandVer++
	third, _ := m.body("claude:abc", 0, 80)
	if len(third) == 0 {
		t.Fatal("cache invalidation should re-render")
	}
}

func TestStatusBarShowsTheWebURL(t *testing.T) {
	m, _ := newTestModel(t)
	if !strings.Contains(m.View(), "192.168.1.10:8787") {
		t.Fatal("the status bar must show where the web UI is listening")
	}
}

func TestQueuedFramesAreCoalescedIntoOneRender(t *testing.T) {
	m, h := newTestModel(t)
	for i := 0; i < 50; i++ {
		h.Apply(provider.Update{
			Agent:  model.Agent{ID: "claude:abc", Provider: "claude", Status: model.StatusLive, Updated: time.Now()},
			Events: []model.Event{{Kind: model.EvText, Header: "tick", Body: "tick"}},
		})
	}
	// One frameMsg must drain the whole backlog, not one frame per render.
	f := <-m.frames
	m.Update(frameMsg(f))

	if got := len(m.buffers["claude:abc"]); got < 50 {
		t.Fatalf("only %d events applied; queued frames were not coalesced", got)
	}
}

func TestRingBufferWarningSurfacesInTitleBar(t *testing.T) {
	m, h := newTestModel(t)
	u := liveAgent()
	h.Apply(u)
	drain(m, h)

	a := m.agents["claude:abc"]
	a.Dropped = 12
	m.agents["claude:abc"] = a

	if !strings.Contains(m.View(), "12 dropped") {
		t.Fatal("a wrapped ring buffer must be visible in the pane title bar")
	}
}

func TestEveryStatusIsSpelledOutInTheTitleBar(t *testing.T) {
	for status, want := range map[model.Status]string{
		model.StatusLive:  "LIVE",
		model.StatusDone:  "DONE",
		model.StatusError: "ERROR",
	} {
		m, h := newTestModel(t)
		u := liveAgent()
		u.Agent.Status = status
		h.Apply(u)
		drain(m, h)

		if out := m.View(); !strings.Contains(out, want) {
			t.Errorf("status %q should render as %q in the title bar", status, want)
		}
	}
}

func TestNarrowPaneKeepsTheDroppedWarning(t *testing.T) {
	m, h := newTestModel(t)
	h.Apply(liveAgent())
	drain(m, h)
	a := m.agents["claude:abc"]
	a.Dropped = 7
	a.Title = strings.Repeat("a very long title that will not fit ", 6)
	m.agents["claude:abc"] = a

	// Squeeze the left column as far as it goes. At this width the long title
	// must be shed, but the warning may only shorten — never vanish.
	m.layout.ColLeft = 0.12
	out := m.View()
	if !strings.Contains(out, "7 dropped") && !strings.Contains(out, "⚠7") {
		t.Fatal("the dropped-events warning must survive truncation in a narrow pane")
	}
	if !strings.Contains(out, "LIVE") {
		t.Fatal("status must survive truncation too")
	}

	// With room, it spells the warning out in full.
	m.layout.ColLeft = 0.7
	if !strings.Contains(m.View(), "7 dropped") {
		t.Fatal("a wide pane should show the full warning")
	}
}
