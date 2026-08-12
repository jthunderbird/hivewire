// Package claudecode adapts Claude Code's on-disk transcripts.
//
// Claude Code writes one dedicated JSONL per subagent:
//
//	~/.claude/projects/<slug>/<session>/subagents/agent-<id>.jsonl
//	~/.claude/projects/<slug>/<session>/subagents/agent-<id>.meta.json
//
// The sidecar meta file names the agent type and the Task tool_use that spawned
// it. Completion is read from the *parent* transcript at
// ~/.claude/projects/<slug>/<session>.jsonl — see parentTail for why a
// background agent's tool_result is a launch receipt rather than a completion,
// and its real ending arrives in a <task-notification> block.
package claudecode

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
	"github.com/jtaylor/hivewire/internal/tailer"
)

// Name is the provider identifier used in agent IDs and the UI.
const Name = "claude"

// Provider discovers and tails Claude Code subagent transcripts.
type Provider struct {
	root     string        // ~/.claude/projects
	idleDone time.Duration // fallback completion timeout when no parent result appears
	since    time.Time     // transcripts older than this are history, not live

	agents  map[string]*agentTail
	parents map[string]*parentTail

	// spawns maps a Task tool_use id to the agent that issued it, which is how
	// a nested subagent learns which agent spawned it rather than only which
	// session it belongs to.
	spawns map[string]string
}

// New returns a provider rooted at the given projects directory. Transcripts
// last modified before since are indexed as backlog rather than streamed.
func New(root string, idleDone time.Duration, since time.Time) *Provider {
	return &Provider{
		root:     root,
		idleDone: idleDone,
		since:    since,
		agents:   map[string]*agentTail{},
		parents:  map[string]*parentTail{},
		spawns:   map[string]string{},
	}
}

func (p *Provider) Name() string { return Name }

type metaFile struct {
	AgentType   string `json:"agentType"`
	Description string `json:"description"`
	ToolUseID   string `json:"toolUseId"`
	SpawnDepth  int    `json:"spawnDepth"`
}

type agentTail struct {
	agent    model.Agent
	tail     *tailer.Tailer
	meta     metaFile
	parent   string   // parent transcript path
	spawned  []string // Task tool_use ids this agent issued
	lastSeen time.Time
	finished bool
	dirty    bool // has state the hub has not seen yet
}

// parentTail follows a top-level session transcript to learn how the subagents
// it spawned ended.
//
// Two shapes exist. A synchronous Task produces a tool_result carrying the
// agent's answer, so that result *is* the completion. A background ("async")
// Task produces a tool_result immediately with status "async_launched" — a
// launch receipt, not a completion — and the real ending arrives later as a
// <task-notification> block naming the agent id. Treating the launch receipt as
// a completion would mark every background agent finished the instant it
// started, so the two are tracked separately.
type parentTail struct {
	tail    *tailer.Tailer
	results map[string]bool   // tool_use id -> errored (synchronous tasks)
	async   map[string]bool   // tool_use id -> launched asynchronously
	models  map[string]string // agent id -> model resolved at launch
	done    map[string]finish // agent id -> completion (async tasks)
}

// finish is a completion parsed from a <task-notification> block.
type finish struct {
	errored    bool
	durationMS int64
}

var (
	taskIDRe   = regexp.MustCompile(`<task-id>([^<]+)</task-id>`)
	statusRe   = regexp.MustCompile(`<status>([^<]+)</status>`)
	durationRe = regexp.MustCompile(`<duration_ms>(\d+)</duration_ms>`)
)

// Poll discovers new subagent transcripts and drains everything appended since
// the last call.
func (p *Provider) Poll() ([]provider.Update, error) {
	p.discover()

	// Parent transcripts first, so a completion recorded this tick is applied
	// to the agent in the same tick.
	for _, pt := range p.parents {
		lines, err := pt.tail.Poll()
		if err != nil {
			continue
		}
		for _, line := range lines {
			pt.scan(line)
		}
	}

	var updates []provider.Update
	for id, at := range p.agents {
		lines, err := at.tail.Poll()
		if err != nil {
			continue
		}
		var events []model.Event
		for _, line := range lines {
			events = append(events, at.parse(line)...)
		}
		if len(lines) > 0 {
			at.lastSeen = time.Now()
			at.dirty = true
		}
		for _, useID := range at.spawned {
			p.spawns[useID] = at.agent.NativeID
		}
		at.spawned = at.spawned[:0]
		if parent := p.spawns[at.meta.ToolUseID]; parent != "" && parent != at.agent.NativeID && at.agent.Parent != parent {
			// Spawned by another subagent, not by the session itself.
			at.agent.Parent = parent
			at.dirty = true
		}
		if st, changed := p.settle(at); changed {
			events = append(events, model.Event{
				AgentID: id,
				TS:      time.Now(),
				Kind:    model.EvStatus,
				Header:  "agent " + string(st),
				Err:     st == model.StatusError,
			})
			at.dirty = true
		}
		if !at.dirty {
			continue
		}
		at.dirty = false
		at.agent.EventCount += len(events)
		for i := range events {
			events[i].AgentID = id
		}
		updates = append(updates, provider.Update{Agent: at.agent, Events: events})
	}
	return updates, nil
}

// discover globs for subagent transcripts that appeared since the last tick.
func (p *Provider) discover() {
	matches, err := filepath.Glob(filepath.Join(p.root, "*", "*", "subagents", "agent-*.jsonl"))
	if err != nil {
		return
	}
	for _, path := range matches {
		native := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "agent-"), ".jsonl")
		id := Name + ":" + native
		if _, ok := p.agents[id]; ok {
			continue
		}

		var meta metaFile
		if b, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + ".meta.json"); err == nil {
			_ = json.Unmarshal(b, &meta)
		}

		// .../projects/<slug>/<session>/subagents/agent-x.jsonl → .../<session>.jsonl
		sessionDir := filepath.Dir(filepath.Dir(path))
		parent := sessionDir + ".jsonl"
		if _, ok := p.parents[parent]; !ok {
			p.parents[parent] = &parentTail{
				tail:    tailer.New(parent),
				results: map[string]bool{},
				async:   map[string]bool{},
				models:  map[string]string{},
				done:    map[string]finish{},
			}
		}

		started, backlog := time.Now(), false
		if fi, err := os.Stat(path); err == nil {
			started = fi.ModTime()
			backlog = fi.ModTime().Before(p.since)
		}
		at := &agentTail{
			agent: model.Agent{
				ID:       id,
				NativeID: native,
				Provider: Name,
				Name:     meta.AgentType,
				Title:    meta.Description,
				Depth:    meta.SpawnDepth,
				Parent:   filepath.Base(sessionDir),
				Source:   path,
				Started:  started,
				Status:   model.StatusLive,
			},
			tail:     tailer.New(path),
			meta:     meta,
			parent:   parent,
			lastSeen: time.Now(),
			dirty:    true,
		}
		if backlog {
			// Index it for history, but do not replay it into a live pane.
			// Sniff the head of the transcript first so history search can still
			// match its prompt and model.
			at.sniff(sniffLines)
			at.tail.SeekEnd()
			at.agent.Backlog = true
			at.agent.Status = model.StatusDone
			at.agent.Updated = started
			at.finished = true
		}
		p.agents[id] = at
	}
}

// settle resolves the agent's status from the parent transcript, falling back
// to an idle timer only when the parent tells us nothing.
func (p *Provider) settle(at *agentTail) (model.Status, bool) {
	if at.finished {
		return at.agent.Status, false
	}
	if pt, ok := p.parents[at.parent]; ok {
		if m := pt.models[at.agent.NativeID]; m != "" && at.agent.Model == "" {
			at.agent.Model = m
		}
		// Background agent: the task-notification is the completion.
		if f, done := pt.done[at.agent.NativeID]; done {
			at.finished = true
			at.agent.Status = model.StatusDone
			if f.errored {
				at.agent.Status = model.StatusError
			}
			at.agent.DurationMS = f.durationMS
			if at.agent.DurationMS == 0 {
				at.agent.DurationMS = duration(at.agent)
			}
			return at.agent.Status, true
		}
		// Synchronous agent: its tool_result is the completion. A launch
		// receipt for an async task is explicitly not one.
		if at.meta.ToolUseID != "" && !pt.async[at.meta.ToolUseID] {
			if errored, done := pt.results[at.meta.ToolUseID]; done {
				at.finished = true
				at.agent.Status = model.StatusDone
				if errored {
					at.agent.Status = model.StatusError
				}
				at.agent.DurationMS = duration(at.agent)
				return at.agent.Status, true
			}
		}
	}
	// Fallback: no parent record (parent file rotated, or agent launched by a
	// harness we cannot see). Treat prolonged silence as completion.
	if p.idleDone > 0 && !at.lastSeen.IsZero() && time.Since(at.lastSeen) > p.idleDone {
		at.finished = true
		at.agent.Status = model.StatusDone
		at.agent.DurationMS = duration(at.agent)
		return at.agent.Status, true
	}
	return at.agent.Status, false
}

// sniffLines bounds how far into a pre-existing transcript we read purely to
// recover searchable metadata.
const sniffLines = 200

// sniff parses the head of a transcript for its prompt and model, discarding
// the events. Without it, history search could only match runs hivewire watched
// live — every transcript that predates it would be unsearchable by prompt.
func (at *agentTail) sniff(maxLines int) {
	f, err := os.Open(at.tail.Path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 0; n < maxLines && sc.Scan(); n++ {
		at.parse(sc.Bytes())
		if at.agent.Prompt != "" && at.agent.Model != "" {
			break
		}
	}
	at.agent.Tokens = model.Tokens{} // sniffed counts are partial; drop them
	at.agent.ToolCount = 0
}

// duration reports the agent's wall time, never negative.
func duration(a model.Agent) int64 {
	if a.Started.IsZero() || a.Updated.Before(a.Started) {
		return 0
	}
	return a.Updated.Sub(a.Started).Milliseconds()
}

// scan records launches, completions and synchronous tool_results.
func (pt *parentTail) scan(line []byte) {
	var l transcriptLine
	if err := json.Unmarshal(line, &l); err != nil {
		return
	}

	// Background agents report their ending in a task-notification, which shows
	// up both as a queue-operation and as a queued_command attachment.
	if text := notificationText(l); text != "" {
		if m := taskIDRe.FindStringSubmatch(text); m != nil {
			f := finish{errored: true}
			if s := statusRe.FindStringSubmatch(text); s != nil {
				f.errored = s[1] != "completed"
			}
			if d := durationRe.FindStringSubmatch(text); d != nil {
				if v, err := strconv.ParseInt(d[1], 10, 64); err == nil {
					f.durationMS = v
				}
			}
			pt.done[m[1]] = f
		}
		return
	}

	if l.Type != "user" || len(l.Message) == 0 {
		return
	}

	// A launch receipt names the agent and the model it resolved to.
	var res struct {
		Status        string `json:"status"`
		AgentID       string `json:"agentId"`
		ResolvedModel string `json:"resolvedModel"`
	}
	async := false
	if len(l.ToolUseResult) > 0 {
		if json.Unmarshal(l.ToolUseResult, &res) == nil && res.Status == "async_launched" {
			async = true
			if res.ResolvedModel != "" && res.AgentID != "" {
				pt.models[res.AgentID] = res.ResolvedModel
			}
		}
	}

	var msg message
	if err := json.Unmarshal(l.Message, &msg); err != nil {
		return
	}
	for _, b := range msg.blocks() {
		if b.Type != "tool_result" || b.ToolUseID == "" {
			continue
		}
		if async {
			pt.async[b.ToolUseID] = true
			continue
		}
		pt.results[b.ToolUseID] = b.IsError
	}
}

// notificationText returns the body of a task-notification carried by this
// line, or "" if the line is not one.
func notificationText(l transcriptLine) string {
	if l.Type == "queue-operation" && strings.Contains(l.Content, "<task-notification>") {
		return l.Content
	}
	if l.Attachment != nil && l.Attachment.CommandMode == "task-notification" {
		return l.Attachment.Prompt
	}
	return ""
}

type transcriptLine struct {
	Type          string          `json:"type"`
	Timestamp     string          `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	GitBranch     string          `json:"gitBranch"`
	SessionKind   string          `json:"sessionKind"`
	Effort        string          `json:"effort"`
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
	Role    string          `json:"role"`
	Model   string          `json:"model"`
	Content json.RawMessage `json:"content"`
	Usage   *usage          `json:"usage"`
}

type usage struct {
	Input       int `json:"input_tokens"`
	Output      int `json:"output_tokens"`
	CacheRead   int `json:"cache_read_input_tokens"`
	CacheCreate int `json:"cache_creation_input_tokens"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     map[string]any  `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// blocks normalizes message.content, which is either a bare string or a list of
// typed blocks.
func (m message) blocks() []block {
	if len(m.Content) == 0 {
		return nil
	}
	var list []block
	if err := json.Unmarshal(m.Content, &list); err == nil {
		return list
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil && s != "" {
		return []block{{Type: "text", Text: s}}
	}
	return nil
}

// flatten renders a tool_result's content, which mirrors the same
// string-or-blocks shape.
func flatten(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var list []block
	if err := json.Unmarshal(raw, &list); err == nil {
		var sb strings.Builder
		for _, b := range list {
			if b.Text != "" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return string(raw)
}

// parse converts one transcript line into zero or more stream events, updating
// the agent's metadata as it goes.
func (at *agentTail) parse(line []byte) []model.Event {
	var l transcriptLine
	if err := json.Unmarshal(line, &l); err != nil {
		return []model.Event{{
			TS:     time.Now(),
			Kind:   model.EvNotice,
			Header: "unparsable transcript line",
			Body:   string(line),
			Err:    true,
		}}
	}

	ts := time.Now()
	if l.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
			ts = t
		}
	}
	at.agent.Updated = ts
	// Discovery seeds Started from the file's mtime, which for a short-lived
	// agent is already past its first line; keep the earliest timestamp seen.
	if at.agent.Started.IsZero() || ts.Before(at.agent.Started) {
		at.agent.Started = ts
	}
	if l.Cwd != "" {
		at.agent.Cwd = l.Cwd
	}
	if l.GitBranch != "" {
		at.agent.GitBranch = l.GitBranch
	}
	if l.SessionKind != "" {
		at.agent.SessionKind = l.SessionKind
	}
	if l.Effort != "" {
		at.agent.Effort = l.Effort
	}
	if l.Version != "" {
		at.agent.CLIVersion = l.Version
	}

	if l.Type == "attachment" || len(l.Message) == 0 {
		return nil
	}
	var msg message
	if err := json.Unmarshal(l.Message, &msg); err != nil {
		return nil
	}
	if msg.Model != "" {
		at.agent.Model = msg.Model
	}
	if u := msg.Usage; u != nil {
		// Per-message input counts are cumulative for the request, so the last
		// one wins; output is genuinely per-message, so it accumulates.
		at.agent.Tokens.In = u.Input
		at.agent.Tokens.CacheRead = u.CacheRead
		at.agent.Tokens.CacheWrite = u.CacheCreate
		at.agent.Tokens.Out += u.Output
		at.agent.Tokens.Total = at.agent.Tokens.In + at.agent.Tokens.Out + at.agent.Tokens.CacheRead
	}

	var events []model.Event
	for _, b := range msg.blocks() {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			kind := model.EvText
			if l.Type == "user" {
				kind = model.EvUser
				// The first user turn is the task the agent was handed; keep it
				// so history search can match against it.
				if at.agent.Prompt == "" {
					at.agent.Prompt = provider.Clip(b.Text, 4000)
				}
			}
			events = append(events, model.Event{
				TS:     ts,
				Kind:   kind,
				Header: provider.FirstLine(b.Text, 160),
				Body:   b.Text,
				Lines:  provider.CountLines(b.Text),
			})
		case "thinking":
			if strings.TrimSpace(b.Thinking) == "" {
				continue
			}
			events = append(events, model.Event{
				TS:     ts,
				Kind:   model.EvReasoning,
				Header: provider.FirstLine(b.Thinking, 160),
				Body:   b.Thinking,
				Lines:  provider.CountLines(b.Thinking),
			})
		case "tool_use":
			at.agent.ToolCount++
			if b.Name == "Task" || b.Name == "Agent" {
				// This agent spawned another; remembering the tool_use lets the
				// nested agent name its spawner instead of only its session.
				at.spawned = append(at.spawned, b.ID)
			}
			events = append(events, model.Event{
				TS:     ts,
				Kind:   model.EvToolUse,
				Tool:   b.Name,
				Header: provider.ToolHeader(b.Name, b.Input),
				Body:   provider.PrettyJSON(b.Input),
				Lines:  provider.CountLines(provider.PrettyJSON(b.Input)),
			})
		case "tool_result":
			body := flatten(b.Content)
			head := provider.FirstLine(body, 160)
			if head == "" {
				head = "(no output)"
			}
			events = append(events, model.Event{
				TS:       ts,
				Kind:     model.EvToolResult,
				Header:   head,
				Body:     body,
				Lines:    provider.CountLines(body),
				Err:      b.IsError,
				Overflow: provider.DetectOverflow(body),
			})
		}
	}
	return events
}
