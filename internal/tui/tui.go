// Package tui renders the live agent panes in the terminal.
//
// Layout mirrors the web UI: two columns split by a draggable vertical gutter,
// each column split by its own draggable horizontal gutter. Mouse drag, click
// to expand, and wheel scrolling all work; every action also has a key.
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/jtaylor/hivewire/internal/hub"
	"github.com/jtaylor/hivewire/internal/model"
)

// Layout holds the pane proportions, persisted between runs.
type Layout struct {
	ColLeft  float64 `json:"col_left"`
	RowLeft  float64 `json:"row_left"`
	RowRight float64 `json:"row_right"`
}

func defaultLayout() Layout { return Layout{ColLeft: 0.5, RowLeft: 0.5, RowRight: 0.5} }

const (
	collapseLines  = 40   // fold bodies longer than this
	maxInlineBytes = 4096 // …or bigger than this, however few lines it is
	maxBodyLines   = 500  // cap one expanded body's contribution to a pane
	maxPaneLines   = 4000 // cap a pane's whole rendered buffer
	maxDrain       = 512  // frames coalesced into one render
)

var (
	cLive   = lipgloss.Color("#3fb950")
	cDone   = lipgloss.Color("#8b949e")
	cIdle   = lipgloss.Color("#3b444f")
	cError  = lipgloss.Color("#f85149")
	cAccent = lipgloss.Color("#58a6ff")
	cMuted  = lipgloss.Color("#8492a6")
	cTool   = lipgloss.Color("#d2a8ff")
	cResult = lipgloss.Color("#7ee787")
	cReason = lipgloss.Color("#f0b72f")

	stMuted  = lipgloss.NewStyle().Foreground(cMuted)
	stAccent = lipgloss.NewStyle().Foreground(cAccent)
	stGutter = lipgloss.NewStyle().Foreground(lipgloss.Color("#2a323d"))
	stFocus  = lipgloss.NewStyle().Foreground(cAccent)
)

type frameMsg hub.Frame
type tickMsg time.Time

// Model is the bubbletea model for the pane grid.
type Model struct {
	hub    *hub.Hub
	frames <-chan hub.Frame
	cancel func()

	w, h int

	agents  map[string]model.Agent
	buffers map[string][]model.Event
	slots   []string
	pending []string

	expanded  map[uint64]bool
	expandVer int // bumped on every expand/collapse, invalidates the cache
	expandAll [4]bool
	scroll    [4]int // lines above the bottom; 0 follows the tail
	hits      [4]map[int]uint64
	cache     [4]*paneCache

	focus  int
	zoom   bool
	drag   string // "", "main", "left", "right"
	layout Layout
	path   string
	url    string // shown in the status bar so the web UI is discoverable
}

// paneCache holds one pane's rendered lines until something changes.
type paneCache struct {
	key    string
	lines  []string
	owners []uint64
}

// New builds the model, subscribing to the hub and loading any saved layout.
// url, when set, is shown in the status bar so the web UI is discoverable.
func New(h *hub.Hub, layoutPath, url string) *Model {
	frames, cancel := h.Subscribe(1024)
	m := &Model{
		hub:      h,
		frames:   frames,
		cancel:   cancel,
		agents:   map[string]model.Agent{},
		buffers:  map[string][]model.Event{},
		expanded: map[uint64]bool{},
		layout:   defaultLayout(),
		path:     layoutPath,
		url:      url,
	}
	if b, err := os.ReadFile(layoutPath); err == nil {
		var l Layout
		if json.Unmarshal(b, &l) == nil && l.ColLeft > 0 {
			m.layout = l
		}
	}
	return m
}

// Close releases the hub subscription.
func (m *Model) Close() { m.cancel() }

func (m *Model) Init() tea.Cmd { return tea.Batch(m.waitFrame(), tick()) }

func (m *Model) waitFrame() tea.Cmd {
	return func() tea.Msg {
		f, ok := <-m.frames
		if !ok {
			return nil
		}
		return frameMsg(f)
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		return m, tick()

	case frameMsg:
		// Coalesce every frame already queued into this one render. One render
		// per frame cannot keep up with a busy agent, and a backed-up
		// subscriber gets its frames dropped by the hub — which is how a pane
		// ends up stuck showing "live" after its agent finished.
		m.applyFrame(hub.Frame(msg))
		for i := 0; i < maxDrain; i++ {
			select {
			case f, ok := <-m.frames:
				if !ok {
					return m, nil
				}
				m.applyFrame(f)
			default:
				return m, m.waitFrame()
			}
		}
		return m, m.waitFrame()

	case tea.KeyMsg:
		return m, m.key(msg)

	case tea.MouseMsg:
		m.mouse(msg)
		return m, nil
	}
	return m, nil
}

func (m *Model) applyFrame(f hub.Frame) {
	switch f.Type {
	case hub.FrameSnapshot:
		m.agents = map[string]model.Agent{}
		m.buffers = map[string][]model.Event{}
		for _, a := range f.Agents {
			m.agents[a.ID] = a
		}
		for id, evs := range f.Buffers {
			m.buffers[id] = evs
		}
		m.slots, m.pending = f.Slots, f.Pending
	case hub.FrameAgent:
		if f.Agent != nil {
			m.agents[f.Agent.ID] = *f.Agent
		}
	case hub.FrameEvents:
		for _, e := range f.Events {
			m.buffers[e.AgentID] = append(m.buffers[e.AgentID], e)
		}
	case hub.FrameSlots:
		m.slots, m.pending = f.Slots, f.Pending
	}
}

func (m *Model) key(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "q", "ctrl+c":
		m.save()
		return tea.Quit
	case "tab":
		m.focus = (m.focus + 1) % 4
	case "shift+tab":
		m.focus = (m.focus + 3) % 4
	case "1", "2", "3", "4":
		m.focus = int(k.String()[0] - '1')
	case "z":
		m.zoom = !m.zoom
	case "e":
		m.expandAll[m.focus] = !m.expandAll[m.focus]
		m.expandVer++
	case "r":
		m.layout = defaultLayout()
		m.save()
	case "up", "k":
		m.scroll[m.focus]++
	case "down", "j":
		if m.scroll[m.focus] > 0 {
			m.scroll[m.focus]--
		}
	case "pgup":
		m.scroll[m.focus] += 10
	case "pgdown":
		m.scroll[m.focus] = max(0, m.scroll[m.focus]-10)
	case "g":
		m.scroll[m.focus] += 10000
	case "G":
		m.scroll[m.focus] = 0
	case "ctrl+left":
		m.layout.ColLeft = clamp(m.layout.ColLeft - 0.02)
		m.save()
	case "ctrl+right":
		m.layout.ColLeft = clamp(m.layout.ColLeft + 0.02)
		m.save()
	case "ctrl+up", "ctrl+down":
		delta := -0.02
		if k.String() == "ctrl+down" {
			delta = 0.02
		}
		if m.focus < 2 {
			m.layout.RowLeft = clamp(m.layout.RowLeft + delta)
		} else {
			m.layout.RowRight = clamp(m.layout.RowRight + delta)
		}
		m.save()
	}
	return nil
}

// mouse handles gutter dragging, pane focus, wheel scroll and click-to-expand.
func (m *Model) mouse(e tea.MouseMsg) {
	g := m.geometry()

	switch e.Action {
	case tea.MouseActionRelease:
		if m.drag != "" {
			m.drag = ""
			m.save()
		}
		return

	case tea.MouseActionPress:
		switch e.Button {
		case tea.MouseButtonWheelUp:
			if s, _, ok := g.locate(e.X, e.Y); ok {
				m.scroll[s] += 3
			}
			return
		case tea.MouseButtonWheelDown:
			if s, _, ok := g.locate(e.X, e.Y); ok {
				m.scroll[s] = max(0, m.scroll[s]-3)
			}
			return
		}
		if which := g.gutterAt(e.X, e.Y); which != "" {
			m.drag = which
			return
		}
		if s, row, ok := g.locate(e.X, e.Y); ok {
			m.focus = s
			if seq, hit := m.hits[s][row]; hit {
				m.expanded[seq] = !m.expanded[seq]
				m.expandVer++
			}
		}
		return

	case tea.MouseActionMotion:
		if m.drag == "" {
			return
		}
		switch m.drag {
		case "main":
			m.layout.ColLeft = clamp(float64(e.X) / float64(max(1, m.w)))
		case "left":
			m.layout.RowLeft = clamp(float64(e.Y) / float64(max(1, g.bodyH)))
		case "right":
			m.layout.RowRight = clamp(float64(e.Y) / float64(max(1, g.bodyH)))
		}
	}
}

func (m *Model) save() {
	b, err := json.MarshalIndent(m.layout, "", " ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(m.path), 0o755)
	_ = os.WriteFile(m.path, b, 0o644)
}

// geom describes the current pane rectangles in terminal cells.
type geom struct {
	leftW, rightW        int
	bodyH                int
	leftTopH, leftBotH   int
	rightTopH, rightBotH int
	zoomSlot             int // -1 unless zoomed
}

func (m *Model) geometry() geom {
	g := geom{zoomSlot: -1}
	g.bodyH = max(1, m.h-1) // reserve the status bar
	if m.zoom {
		g.zoomSlot = m.focus
		g.leftW, g.rightW = m.w, 0
		g.leftTopH = g.bodyH
		return g
	}
	g.leftW = clampInt(int(m.layout.ColLeft*float64(m.w)), 8, max(8, m.w-9))
	g.rightW = max(1, m.w-g.leftW-1)

	inner := max(1, g.bodyH-1) // one row for the horizontal gutter
	g.leftTopH = clampInt(int(m.layout.RowLeft*float64(g.bodyH)), 3, max(3, inner-3))
	g.leftBotH = max(1, inner-g.leftTopH)
	g.rightTopH = clampInt(int(m.layout.RowRight*float64(g.bodyH)), 3, max(3, inner-3))
	g.rightBotH = max(1, inner-g.rightTopH)
	return g
}

// gutterAt reports which gutter (if any) sits under the given cell.
func (g geom) gutterAt(x, y int) string {
	if g.zoomSlot >= 0 || y >= g.bodyH {
		return ""
	}
	if x == g.leftW {
		return "main"
	}
	if x < g.leftW && y == g.leftTopH {
		return "left"
	}
	if x > g.leftW && y == g.rightTopH {
		return "right"
	}
	return ""
}

// locate maps a cell to a slot index and the row within that pane's body.
func (g geom) locate(x, y int) (slot, row int, ok bool) {
	if y >= g.bodyH {
		return 0, 0, false
	}
	if g.zoomSlot >= 0 {
		return g.zoomSlot, y - 1, y >= 1
	}
	switch {
	case x < g.leftW && y < g.leftTopH:
		return 0, y - 1, y >= 1
	case x < g.leftW && y > g.leftTopH:
		return 1, y - g.leftTopH - 2, y >= g.leftTopH+2
	case x > g.leftW && y < g.rightTopH:
		return 2, y - 1, y >= 1
	case x > g.leftW && y > g.rightTopH:
		return 3, y - g.rightTopH - 2, y >= g.rightTopH+2
	}
	return 0, 0, false
}

func (m *Model) View() string {
	if m.w == 0 || m.h == 0 {
		return "starting hivewire…"
	}
	g := m.geometry()
	m.hits = [4]map[int]uint64{}

	if g.zoomSlot >= 0 {
		body := strings.Join(m.pane(g.zoomSlot, m.w, g.bodyH), "\n")
		return body + "\n" + m.status()
	}

	left := append(m.pane(0, g.leftW, g.leftTopH),
		stGutter.Render(strings.Repeat("─", g.leftW)))
	left = append(left, m.pane(1, g.leftW, g.leftBotH)...)

	right := append(m.pane(2, g.rightW, g.rightTopH),
		stGutter.Render(strings.Repeat("─", g.rightW)))
	right = append(right, m.pane(3, g.rightW, g.rightBotH)...)

	var sb strings.Builder
	rows := max(len(left), len(right))
	for i := 0; i < rows; i++ {
		l, r := "", ""
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		sb.WriteString(pad(l, g.leftW))
		sb.WriteString(stGutter.Render("│"))
		sb.WriteString(pad(r, g.rightW))
		sb.WriteString("\n")
	}
	sb.WriteString(m.status())
	return sb.String()
}

// pane renders one slot as exactly h lines of width w.
func (m *Model) pane(slot, w, h int) []string {
	if h < 1 {
		return nil
	}
	m.hits[slot] = map[int]uint64{}

	var id string
	if slot < len(m.slots) {
		id = m.slots[slot]
	}
	agent, live := m.agents[id]

	out := make([]string, 0, h)
	out = append(out, m.title(slot, agent, live, w))
	rail := m.edge(agent, live)
	blank := rail + strings.Repeat(" ", max(0, w-1))

	if !live {
		out = append(out, rail+stMuted.Render(pad(" waiting for a subagent…", max(0, w-1))))
		for len(out) < h {
			out = append(out, blank)
		}
		return out[:h]
	}

	// The rail occupies one column, so the body wraps one narrower.
	lines, owners := m.body(id, slot, max(1, w-1))
	visible := h - 1
	start := max(0, len(lines)-visible-m.scroll[slot])
	if start+visible > len(lines) {
		start = max(0, len(lines)-visible)
		m.scroll[slot] = 0
	}
	end := min(len(lines), start+visible)

	for i := start; i < end; i++ {
		m.hits[slot][i-start] = owners[i]
		out = append(out, rail+lines[i])
	}
	for len(out) < h {
		out = append(out, blank)
	}
	return out[:h]
}

// statusColors maps a pane's state to its title-bar fill and edge colour. The
// bar is filled rather than merely tinted: across four panes a coloured glyph is
// too easy to miss, a solid bar is not.
func statusColors(a model.Agent, live bool) (fill lipgloss.Color, word string) {
	switch {
	case !live:
		return cIdle, "IDLE"
	case a.Status == model.StatusError:
		return cError, "ERROR"
	case a.Status == model.StatusDone:
		return cDone, "DONE"
	}
	return cLive, "LIVE"
}

// title renders the filled, colour-coded pane header.
func (m *Model) title(slot int, a model.Agent, live bool, w int) string {
	fill, word := statusColors(a, live)
	bar := lipgloss.NewStyle().Background(fill).Foreground(lipgloss.Color("#0d1117")).Bold(true)

	marker := " "
	if m.focus == slot {
		marker = "▌"
	}

	var line string
	if !live {
		line = fmt.Sprintf("%s %-5s  slot %d — waiting for a subagent", marker, word, slot+1)
		return bar.Render(pad(line, w))
	}

	// Assemble in priority order and stop when the pane runs out of room, so a
	// narrow pane sheds the title before it sheds the status or the
	// dropped-events warning — the two things that must never go silent.
	head := fmt.Sprintf("%s %s", marker, word)
	if a.Dropped > 0 {
		head += compactFit(w-lipgloss.Width(head),
			fmt.Sprintf("  ⚠ %d dropped", a.Dropped),
			fmt.Sprintf(" ⚠%d", a.Dropped))
	}
	for _, seg := range []string{
		"  " + a.Provider + " · " + shortModel(a.Model),
		" · " + a.Label(),
		" · " + strconv.Quote(a.Title),
	} {
		if a.Title == "" && strings.HasPrefix(seg, ` · "`) {
			continue
		}
		if lipgloss.Width(head)+lipgloss.Width(seg) > w {
			break
		}
		head += seg
	}

	meta := metaLine(a)
	line = head
	if lipgloss.Width(head)+lipgloss.Width(meta)+2 < w {
		line = head + strings.Repeat(" ", max(1, w-lipgloss.Width(head)-lipgloss.Width(meta)-1)) + meta
	}
	return bar.Render(pad(truncate(line, w), w))
}

// compactFit returns full when it fits in w cells, otherwise the short form
// (and the short form even when that overflows — losing the warning entirely is
// worse than a clipped one).
func compactFit(w int, full, short string) string {
	if lipgloss.Width(full) <= w {
		return full
	}
	return short
}

// edge is the coloured rail drawn down the left of a pane body, so state stays
// visible even when the title bar has scrolled out of a glance.
func (m *Model) edge(a model.Agent, live bool) string {
	fill, _ := statusColors(a, live)
	return lipgloss.NewStyle().Foreground(fill).Render("▏")
}

func truncate(s string, w int) string {
	if lipgloss.Width(s) <= w {
		return s
	}
	out, width := make([]rune, 0, w), 0
	for _, r := range s {
		rw := runewidth.RuneWidth(r)
		if width+rw > w {
			break
		}
		out = append(out, r)
		width += rw
	}
	return string(out)
}

func metaLine(a model.Agent) string {
	var parts []string
	if a.Depth > 0 {
		depth := fmt.Sprintf("d%d", a.Depth)
		// Depth says an agent was nested; the parent's name says by whom.
		if a.ParentLabel != "" {
			depth += " · in " + a.ParentLabel
		}
		parts = append(parts, depth)
	}
	if a.Tokens.Total > 0 {
		tok := fmt.Sprintf("%.1fk tok", float64(a.Tokens.Total)/1000)
		// Only Codex reports the model's context window; an unknown one is
		// shown as unknown rather than as a percentage of nothing.
		if a.Tokens.ContextWindow > 0 {
			tok += fmt.Sprintf(" (%d%% ctx)", a.Tokens.Total*100/a.Tokens.ContextWindow)
		} else {
			tok += " (ctx --)"
		}
		parts = append(parts, tok)
	}
	if a.ToolCount > 0 {
		parts = append(parts, fmt.Sprintf("%d tools", a.ToolCount))
	}
	if d := elapsed(a); d != "" {
		parts = append(parts, d)
	}
	if a.Sandbox != "" {
		parts = append(parts, a.Sandbox)
	}
	if a.Effort != "" {
		parts = append(parts, "effort:"+a.Effort)
	}
	if a.GitBranch != "" {
		parts = append(parts, a.GitBranch)
	}
	return strings.Join(parts, " · ") + " "
}

// body renders an agent's events into wrapped lines, returning the event seq
// that owns each line so clicks can be routed back to it.
//
// The result is cached: bubbletea re-renders after every message (including
// every mouse-motion event during a drag), and re-wrapping a full buffer each
// time is what makes the UI feel stuck.
func (m *Model) body(id string, slot, w int) ([]string, []uint64) {
	key := fmt.Sprintf("%s|%d|%d|%t|%d", id, len(m.buffers[id]), w, m.expandAll[slot], m.expandVer)
	if c := m.cache[slot]; c != nil && c.key == key {
		return c.lines, c.owners
	}

	var lines []string
	var owners []uint64

	add := func(text string, st lipgloss.Style, seq uint64) {
		// Tool output frequently carries its own ANSI colour codes (make,
		// kubectl, colored test runners, …). Trust those over our own
		// kind-based colour rather than treating the escape bytes as text:
		// plain rune-width wrapping cannot skip them, so they would otherwise
		// come out as literal "␛[32m" garbage and mis-measure the line.
		if hasANSI(text) {
			for _, chunk := range ansiWrap(text, w) {
				lines = append(lines, chunk)
				owners = append(owners, seq)
			}
			return
		}
		for _, chunk := range wrap(text, w) {
			lines = append(lines, st.Render(chunk))
			owners = append(owners, seq)
		}
	}

	for _, e := range m.buffers[id] {
		// Fold on bytes as well as lines: a single-line 140 KB tool result has
		// Lines == 1 but must not be pasted into the pane verbatim.
		collapsible := e.Lines > collapseLines || len(e.Body) > maxInlineBytes || e.Kind == model.EvToolUse
		open := m.expanded[e.Seq] || m.expandAll[slot] || (!collapsible && e.Body != "")

		chev := " "
		if collapsible {
			chev = "▸"
			if open {
				chev = "▾"
			}
		}
		head := fmt.Sprintf("%s %s%s %s", e.TS.Format("15:04:05"), chev, kindTag(e), e.Header)
		switch {
		case e.Lines > 1:
			head += fmt.Sprintf("  (%d lines)", e.Lines)
		case len(e.Body) > maxInlineBytes:
			head += fmt.Sprintf("  (%s)", humanBytes(len(e.Body)))
		}
		add(head, styleFor(e), e.Seq)

		if e.Overflow != nil {
			add("    ⚠ "+e.Overflow.Note+" — full output: "+e.Overflow.Path,
				lipgloss.NewStyle().Foreground(cReason), e.Seq)
		}
		if open && e.Body != "" {
			shown := 0
			for _, bl := range strings.Split(e.Body, "\n") {
				if shown >= maxBodyLines {
					add(fmt.Sprintf("    … body truncated in view (%s total) — see %s",
						humanBytes(len(e.Body)), "the web UI or the transcript"),
						lipgloss.NewStyle().Foreground(cReason), e.Seq)
					break
				}
				before := len(lines)
				add("    "+bl, stMuted, e.Seq)
				shown += len(lines) - before
			}
		}
	}

	// Keep the buffer bounded so a long-running agent cannot make each frame
	// linearly more expensive.
	if len(lines) > maxPaneLines {
		cut := len(lines) - maxPaneLines
		lines, owners = lines[cut:], owners[cut:]
	}

	m.cache[slot] = &paneCache{key: key, lines: lines, owners: owners}
	return lines, owners
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

func styleFor(e model.Event) lipgloss.Style {
	switch {
	case e.Err:
		return lipgloss.NewStyle().Foreground(cError)
	case e.Kind == model.EvToolUse:
		return lipgloss.NewStyle().Foreground(cTool)
	case e.Kind == model.EvToolResult:
		return lipgloss.NewStyle().Foreground(cResult)
	case e.Kind == model.EvReasoning:
		return lipgloss.NewStyle().Foreground(cReason)
	case e.Kind == model.EvUser:
		return stAccent
	case e.Kind == model.EvStatus, e.Kind == model.EvNotice:
		return stMuted
	}
	return lipgloss.NewStyle()
}

func kindTag(e model.Event) string {
	switch e.Kind {
	case model.EvToolUse:
		return "⚙"
	case model.EvToolResult:
		return "⮑"
	case model.EvReasoning:
		return "✻"
	case model.EvUser:
		return "›"
	case model.EvStatus:
		return "◆"
	case model.EvNotice:
		return "!"
	}
	return "·"
}

func (m *Model) status() string {
	live := 0
	for _, a := range m.agents {
		if a.Status == model.StatusLive {
			live++
		}
	}
	s := fmt.Sprintf(" hivewire · %d live · %d seen", live, len(m.agents))
	if len(m.pending) > 0 {
		s += fmt.Sprintf(" · %d waiting", len(m.pending))
	}
	if m.url != "" {
		s += " · " + m.url
	}
	s += fmt.Sprintf(" · pane %d", m.focus+1)
	if m.zoom {
		s += " · zoom"
	}
	hint := "drag gutters · click to expand · 1-4 focus · e expand · z zoom · ctrl+←→↑↓ resize · q quit "
	if len(s)+len(hint) < m.w {
		s += strings.Repeat(" ", m.w-len(s)-len(hint)) + hint
	}
	return lipgloss.NewStyle().Foreground(cMuted).Render(pad(s, m.w))
}

// ---- small helpers -------------------------------------------------------

func elapsed(a model.Agent) string {
	if a.Started.IsZero() {
		return ""
	}
	end := a.Updated
	if a.Status == model.StatusLive {
		end = time.Now()
	}
	d := end.Sub(a.Started)
	if d <= 0 {
		return ""
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
}

func shortModel(s string) string {
	if s == "" {
		return "?"
	}
	return strings.TrimPrefix(s, "claude-")
}

// hasANSI reports whether s carries a raw ANSI escape byte, the signal used to
// decide whether to trust the source's own colouring instead of our own.
func hasANSI(s string) bool { return strings.IndexByte(s, 0x1b) >= 0 }

// ansiWrap hard-wraps text that already carries ANSI SGR codes, preserving
// them (ansi.Hardwrap skips escape sequences when measuring width, unlike the
// plain rune loop in wrap). Every resulting line gets its own reset appended,
// so a colour a source left open cannot bleed into the rail character, the
// padding, or an unrelated line rendered after it — lipgloss re-renders the
// whole screen as one string each frame, so nothing else would stop it.
func ansiWrap(text string, w int) []string {
	if w <= 0 {
		return []string{""}
	}
	lines := strings.Split(ansi.Hardwrap(text, w, true), "\n")
	for i := range lines {
		lines[i] += ansi.ResetStyle
	}
	return lines
}

// wrap splits plain (unstyled) text into chunks of at most w cells.
//
// It walks the string once, accumulating rune widths, because the obvious
// implementation — shrink a slice until lipgloss.Width fits — is quadratic and
// a single 140 KB tool result was enough to peg the render thread. Styling is
// applied to the resulting chunks, never before wrapping.
func wrap(s string, w int) []string {
	if w <= 0 {
		return []string{""}
	}
	var (
		out   []string
		start int
		width int
	)
	for i, r := range s {
		rw := runewidth.RuneWidth(r)
		if width+rw > w && i > start {
			out = append(out, s[start:i])
			start, width = i, 0
		}
		width += rw
	}
	out = append(out, s[start:])
	return out
}

func pad(s string, w int) string {
	d := w - lipgloss.Width(s)
	if d <= 0 {
		return s
	}
	return s + strings.Repeat(" ", d)
}

func clamp(v float64) float64 {
	if v < 0.12 {
		return 0.12
	}
	if v > 0.88 {
		return 0.88
	}
	return v
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
