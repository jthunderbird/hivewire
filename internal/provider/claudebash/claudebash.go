// Package claudebash adapts Claude Code's background Bash tasks — commands
// launched with the Bash tool's run_in_background flag (or Ctrl+B). Unlike
// the other four providers, a background task has no model and no structured
// event stream: it is a shell command whose raw stdout/stderr Claude Code
// appends to a plain text file outside the transcript, entirely separate from
// the async Task subagents internal/provider/claudecode already handles.
//
// Nothing about a task is self-describing on its own — there is no sidecar
// meta file the way a subagent has one. Discovery instead reads the *host*
// transcript (the top-level session, or a subagent's own transcript, since a
// subagent can background a command too) for three things that always appear
// together there:
//
//  1. A Bash tool_use with input.run_in_background == true: the command and
//     description.
//  2. Its tool_result, a launch receipt whose structured toolUseResult carries
//     backgroundTaskId, and whose text names the output file: "Command
//     running in background with ID: <id>. Output is being written to:
//     <path>."
//  3. Later, a <task-notification> block — delivered as a queue-operation and
//     again once the model sees it, either as an "attachment" or as a plain
//     user message — naming the task id and its final status.
//
// The output file itself is then tailed line by line like any other
// transcript, just without JSON to parse.
package claudebash

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
	"github.com/jtaylor/hivewire/internal/tailer"
)

// Name is the provider identifier used in agent IDs and the UI.
const Name = "claude-background"

// Provider discovers Claude Code background Bash tasks from session and
// subagent transcripts, then tails each task's raw output file.
type Provider struct {
	root     string        // same projects directory internal/provider/claudecode reads
	idleDone time.Duration // fallback completion timeout when no notification appears
	since    time.Time     // launches older than this are history, not live

	hosts    map[string]*hostState // transcript path -> scan state
	tasks    map[string]*taskTail  // native task id -> task
	notified map[string]finish     // native task id -> completion, until claimed by settle
}

// New returns a provider rooted at the given Claude Code projects directory.
// Tasks launched before since are indexed as backlog rather than streamed.
func New(root string, idleDone time.Duration, since time.Time) *Provider {
	return &Provider{
		root:     root,
		idleDone: idleDone,
		since:    since,
		hosts:    map[string]*hostState{},
		tasks:    map[string]*taskTail{},
		notified: map[string]finish{},
	}
}

func (p *Provider) Name() string { return Name }

// hostState follows one transcript — a top-level session or a subagent — for
// Bash launches and task-notifications. parentID is what a task discovered
// here reports as its Parent: the session's or subagent's native id.
type hostState struct {
	tail     *tailer.Tailer
	parentID string
	pending  map[string]launch // tool_use id -> launch, until its receipt arrives
}

// launch is a background Bash invocation seen in a host transcript, held
// until its receipt names the task id it was assigned.
type launch struct {
	command     string
	description string
	cwd         string
	gitBranch   string
	version     string
	ts          time.Time
}

// finish is a completion parsed from a <task-notification> block.
type finish struct {
	errored bool
	summary string
}

// taskTail is one background task: its normalized agent plus a tailer over
// its raw output file.
type taskTail struct {
	agent      model.Agent
	tail       *tailer.Tailer
	initEvents []model.Event // the synthesized launch event, emitted once
	lastSeen   time.Time
	finished   bool
	dirty      bool
}

var (
	taskIDRe     = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	statusRe     = regexp.MustCompile(`<status>([^<]+)</status>`)
	summaryRe    = regexp.MustCompile(`<summary>([^<]*)</summary>`)
	outputFileRe = regexp.MustCompile(`Output is being written to: (\S+\.output)\b`)
)

// Poll discovers new background tasks and drains everything appended since
// the last call, both to the host transcripts and to each task's output file.
func (p *Provider) Poll() ([]provider.Update, error) {
	p.discover()

	// Hosts first, so a launch or completion recorded this tick is applied to
	// its task in the same tick.
	for _, h := range p.hosts {
		lines, err := h.tail.Poll()
		if err != nil {
			continue
		}
		for _, line := range lines {
			p.scanHost(h, line)
		}
	}

	var updates []provider.Update
	for id, tt := range p.tasks {
		var events []model.Event
		if len(tt.initEvents) > 0 {
			events = append(events, tt.initEvents...)
			tt.initEvents = nil
			tt.dirty = true
		}
		lines, err := tt.tail.Poll()
		if err != nil {
			continue
		}
		if len(lines) > 0 {
			body := joinLines(lines)
			events = append(events, model.Event{
				Kind:   model.EvToolResult,
				Header: provider.FirstLine(body, 160),
				Body:   body,
				Lines:  provider.CountLines(body),
			})
			tt.lastSeen = time.Now()
			tt.agent.Updated = tt.lastSeen
			tt.dirty = true
		}
		if st, changed := p.settle(tt); changed {
			events = append(events, model.Event{
				TS:     time.Now(),
				Kind:   model.EvStatus,
				Header: "agent " + string(st),
				Err:    st == model.StatusError,
			})
			tt.dirty = true
		}
		if !tt.dirty {
			continue
		}
		tt.dirty = false
		tt.agent.EventCount += len(events)
		for i := range events {
			events[i].AgentID = id
			if events[i].TS.IsZero() {
				events[i].TS = time.Now()
			}
		}
		updates = append(updates, provider.Update{Agent: tt.agent, Events: events})
	}
	return updates, nil
}

// discover globs for session and subagent transcripts, the two places a
// background Bash launch can appear.
func (p *Provider) discover() {
	if sessions, err := filepath.Glob(filepath.Join(p.root, "*", "*.jsonl")); err == nil {
		for _, path := range sessions {
			p.ensureHost(path)
		}
	}
	if subs, err := filepath.Glob(filepath.Join(p.root, "*", "*", "subagents", "agent-*.jsonl")); err == nil {
		for _, path := range subs {
			p.ensureHost(path)
		}
	}
}

func (p *Provider) ensureHost(path string) {
	if _, ok := p.hosts[path]; ok {
		return
	}
	p.hosts[path] = &hostState{
		tail:     tailer.New(path),
		parentID: hostParentID(path),
		pending:  map[string]launch{},
	}
}

// hostParentID is the native id a task launched in this transcript should
// report as its parent: the subagent's id for a subagent transcript, or the
// session's id for a top-level one.
func hostParentID(path string) string {
	if filepath.Base(filepath.Dir(path)) == "subagents" {
		return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl")
	}
	return strings.TrimSuffix(filepath.Base(path), ".jsonl")
}

// scanHost records a background Bash launch, its receipt, or its eventual
// completion.
func (p *Provider) scanHost(h *hostState, line []byte) {
	var l transcriptLine
	if err := json.Unmarshal(line, &l); err != nil {
		return
	}

	if text := notificationText(l); text != "" {
		if m := taskIDRe.FindStringSubmatch(text); m != nil {
			f := finish{errored: true}
			if s := statusRe.FindStringSubmatch(text); s != nil {
				f.errored = s[1] != "completed"
			}
			if sm := summaryRe.FindStringSubmatch(text); sm != nil {
				f.summary = sm[1]
			}
			p.notified[m[1]] = f
		}
		return
	}

	if len(l.Message) == 0 {
		return
	}
	var msg message
	if err := json.Unmarshal(l.Message, &msg); err != nil {
		return
	}

	for _, b := range msg.blocks() {
		switch b.Type {
		case "tool_use":
			if !strings.EqualFold(b.Name, "Bash") {
				continue
			}
			// Record every Bash call, not just ones launched with
			// run_in_background: true — Claude Code also backgrounds a
			// synchronous command that hits its own timeout, or one the user
			// moves to the background mid-flight with Ctrl+B. In every case
			// the *receipt* is what actually says a task was created (it
			// carries backgroundTaskId); the launch's own flag does not.
			cmd, _ := b.Input["command"].(string)
			desc, _ := b.Input["description"].(string)
			h.pending[b.ID] = launch{
				command: cmd, description: desc,
				cwd: l.Cwd, gitBranch: l.GitBranch, version: l.Version,
				ts: parseTS(l.Timestamp),
			}
		case "tool_result":
			if b.ToolUseID == "" {
				continue
			}
			lc, ok := h.pending[b.ToolUseID]
			if !ok {
				continue
			}
			delete(h.pending, b.ToolUseID)
			var res struct {
				BackgroundTaskID string `json:"backgroundTaskId"`
			}
			if len(l.ToolUseResult) > 0 {
				_ = json.Unmarshal(l.ToolUseResult, &res)
			}
			if res.BackgroundTaskID == "" {
				continue // launch itself failed; no task was actually started
			}
			outPath := ""
			if m := outputFileRe.FindStringSubmatch(flatten(b.Content)); m != nil {
				outPath = m[1]
			}
			p.foundTask(res.BackgroundTaskID, lc, outPath, h.parentID)
		}
	}
}

// foundTask creates the task the first time its launch receipt is seen.
func (p *Provider) foundTask(taskID string, l launch, outputPath, parentID string) {
	if _, ok := p.tasks[taskID]; ok || outputPath == "" {
		return
	}
	started := l.ts
	if started.IsZero() {
		started = time.Now()
	}
	title := l.description
	if title == "" {
		title = provider.FirstLine(l.command, 80)
	}

	tt := &taskTail{
		agent: model.Agent{
			ID:         Name + ":" + taskID,
			NativeID:   taskID,
			Provider:   Name,
			Name:       "bash",
			Nickname:   taskID,
			Title:      title,
			Prompt:     l.command,
			Parent:     parentID,
			Cwd:        l.cwd,
			GitBranch:  l.gitBranch,
			CLIVersion: l.version,
			Source:     outputPath,
			Started:    started,
			Updated:    started,
			Status:     model.StatusLive,
		},
		tail:     tailer.New(outputPath),
		lastSeen: time.Now(),
		dirty:    true,
	}

	if started.Before(p.since) {
		// Index it for history, but do not replay it into a live pane.
		tt.agent.Backlog = true
		tt.agent.Status = model.StatusDone
		tt.finished = true
		tt.tail.SeekEnd()
	} else {
		tt.initEvents = []model.Event{launchEvent(l)}
		tt.dirty = true
	}
	p.tasks[taskID] = tt
}

// launchEvent synthesizes the tool_use a background task never gets a
// dedicated transcript line for, so a pane shows the same "Bash  <command>"
// header a foreground invocation would.
func launchEvent(l launch) model.Event {
	return model.Event{
		TS:     l.ts,
		Kind:   model.EvToolUse,
		Tool:   "Bash",
		Header: provider.ToolHeader("Bash", map[string]any{"command": l.command, "description": l.description}),
		Body:   l.command,
		Lines:  provider.CountLines(l.command),
	}
}

// settle resolves a task's status from its completion notification, falling
// back to an idle timer only when no notification has arrived.
func (p *Provider) settle(tt *taskTail) (model.Status, bool) {
	if tt.finished {
		return tt.agent.Status, false
	}
	if f, done := p.notified[tt.agent.NativeID]; done {
		tt.finished = true
		tt.agent.Status = model.StatusDone
		if f.errored {
			tt.agent.Status = model.StatusError
		}
		tt.agent.DurationMS = duration(tt.agent)
		return tt.agent.Status, true
	}
	if p.idleDone > 0 && !tt.lastSeen.IsZero() && time.Since(tt.lastSeen) > p.idleDone {
		tt.finished = true
		tt.agent.Status = model.StatusDone
		tt.agent.DurationMS = duration(tt.agent)
		return tt.agent.Status, true
	}
	return tt.agent.Status, false
}

// Replay reads a finished background task's raw output file plus its host
// transcript, and rebuilds its full event stream. hostPath is the record's
// Source field — the transcript the task was launched from — and taskID is
// its NativeID, since one host transcript can launch many tasks.
func Replay(hostPath, taskID string) (model.Agent, []model.Event, error) {
	p := &Provider{
		hosts:    map[string]*hostState{},
		tasks:    map[string]*taskTail{},
		notified: map[string]finish{},
	}
	h := &hostState{tail: tailer.New(hostPath), parentID: hostParentID(hostPath), pending: map[string]launch{}}
	p.hosts[hostPath] = h

	for {
		lines, err := h.tail.Poll()
		if err != nil {
			return model.Agent{}, nil, err
		}
		if len(lines) == 0 {
			break
		}
		for _, line := range lines {
			p.scanHost(h, line)
		}
	}

	tt, ok := p.tasks[taskID]
	if !ok {
		return model.Agent{}, nil, fmt.Errorf("claude-background: task %s not found in %s", taskID, hostPath)
	}

	var events []model.Event
	var seq uint64
	appendEvent := func(e model.Event) {
		seq++
		e.Seq = seq
		e.AgentID = tt.agent.ID
		if e.TS.IsZero() {
			e.TS = time.Now()
		}
		events = append(events, e)
	}
	for _, e := range tt.initEvents {
		appendEvent(e)
	}
	for {
		lines, err := tt.tail.Poll()
		if err != nil {
			return tt.agent, events, err
		}
		if len(lines) == 0 {
			break
		}
		body := joinLines(lines)
		appendEvent(model.Event{
			Kind:   model.EvToolResult,
			Header: provider.FirstLine(body, 160),
			Body:   body,
			Lines:  provider.CountLines(body),
		})
	}

	tt.agent.Status = model.StatusDone
	if f, done := p.notified[taskID]; done && f.errored {
		tt.agent.Status = model.StatusError
	}
	if len(events) > 0 {
		tt.agent.Updated = events[len(events)-1].TS
	}
	tt.agent.DurationMS = duration(tt.agent)
	tt.agent.EventCount = len(events)
	return tt.agent, events, nil
}

// duration reports the task's wall time, never negative.
func duration(a model.Agent) int64 {
	if a.Started.IsZero() || a.Updated.Before(a.Started) {
		return 0
	}
	return a.Updated.Sub(a.Started).Milliseconds()
}

func parseTS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func joinLines(lines [][]byte) string {
	return string(bytes.Join(lines, []byte("\n")))
}

// notificationText returns the body of a task-notification carried by this
// line, or "" if the line is not one. Claude Code has delivered these three
// ways across versions: a queue-operation record (always present, and always
// first), an "attachment" wrapping a queued_command, or a plain user message
// whose content is the notification text.
func notificationText(l transcriptLine) string {
	if l.Type == "queue-operation" && strings.Contains(l.Content, "<task-notification>") {
		return l.Content
	}
	if l.Attachment != nil && l.Attachment.CommandMode == "task-notification" {
		return l.Attachment.Prompt
	}
	if l.Type == "user" && len(l.Message) > 0 {
		var m struct {
			Content string `json:"content"`
		}
		if json.Unmarshal(l.Message, &m) == nil && strings.Contains(m.Content, "<task-notification>") {
			return m.Content
		}
	}
	return ""
}

type transcriptLine struct {
	Type          string          `json:"type"`
	Timestamp     string          `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	GitBranch     string          `json:"gitBranch"`
	Version       string          `json:"version"`
	Content       string          `json:"content"` // queue-operation payload
	Message       json.RawMessage `json:"message"`
	ToolUseResult json.RawMessage `json:"toolUseResult"`
	Attachment    *struct {
		CommandMode string `json:"commandMode"`
		Prompt      string `json:"prompt"`
	} `json:"attachment"`
}

type message struct {
	Content json.RawMessage `json:"content"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// blocks normalizes message.content, which is either a bare string or a list
// of typed blocks. Only tool_use and tool_result carry anything this provider
// needs, so a bare string (plain prose turns) yields nothing.
func (m message) blocks() []block {
	if len(m.Content) == 0 {
		return nil
	}
	var list []block
	if err := json.Unmarshal(m.Content, &list); err == nil {
		return list
	}
	return nil
}

// flatten renders a tool_result's content, which mirrors the same
// string-or-blocks shape. Every launch receipt observed has been a bare
// string, but the block form is handled defensively since other providers'
// transcripts use both.
func flatten(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var list []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &list); err == nil {
		var sb strings.Builder
		for _, b := range list {
			sb.WriteString(b.Text)
		}
		return sb.String()
	}
	return string(raw)
}
