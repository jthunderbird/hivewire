// Package omp adapts Oh My Pi (omp) subagent transcripts.
//
// omp writes one JSONL per session. A subagent's transcript lives in the
// artifacts directory named after its parent's session file:
//
//	~/.omp/agent/sessions/<bucket>/<ts>_<sessionId>.jsonl      ← parent session
//	~/.omp/agent/sessions/<bucket>/<ts>_<sessionId>/<id>.jsonl ← subagent
//	~/.omp/agent/sessions/<bucket>/<ts>_<sessionId>/<id>/<id>.jsonl ← nested subagent
//
// Unlike Claude Code's, an omp subagent transcript is self-describing: its
// session_init line carries the agent type, the resolved model and the whole
// task it was handed, so no parent read is needed for metadata. The parent is
// tailed only for authoritative completion, which arrives as a hub job record
// or an async-result injection.
package omp

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
	"github.com/jtaylor/hivewire/internal/tailer"
)

// Name is the provider identifier used in agent IDs and the UI.
const Name = "omp"

// Provider discovers and tails omp subagent transcripts.
type Provider struct {
	root     string        // ~/.omp/agent/sessions
	idleDone time.Duration // fallback completion timeout
	since    time.Time     // transcripts older than this are history, not live

	agents  map[string]*agentTail  // path -> tail
	parents map[string]*parentTail // parent transcript path -> tail
}

// New returns a provider rooted at the given sessions directory. Transcripts
// last modified before since are indexed as backlog rather than streamed.
func New(root string, idleDone time.Duration, since time.Time) *Provider {
	return &Provider{
		root:     root,
		idleDone: idleDone,
		since:    since,
		agents:   map[string]*agentTail{},
		parents:  map[string]*parentTail{},
	}
}

func (p *Provider) Name() string { return Name }

type agentTail struct {
	agent    model.Agent
	tail     *tailer.Tailer
	parent   string // parent transcript path
	jobID    string // hub job id, which is the agent id in the filename
	lastSeen time.Time
	finished bool
	dirty    bool
}

// parentTail follows the session that spawned an agent, to learn how it ended.
// A settled job appears either in a hub tool result or in the async-result
// message injected into the parent conversation when it yields.
type parentTail struct {
	tail *tailer.Tailer
	jobs map[string]job // agent id -> settled job
}

// job is a subagent's outcome as recorded by its parent.
type job struct {
	status     string
	durationMS int64
	label      string
}

// sessionDirRe matches omp's artifacts directory name, "<timestamp>_<sessionId>".
var sessionDirRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T[\d-]+Z_`)

var taskResultRe = regexp.MustCompile(`<task-result\s+id="([^"]+)"[^>]*\bstatus="([^"]+)"(?:[^>]*\bduration="([^"]+)")?`)

// Poll discovers new subagent transcripts and drains everything appended since
// the last call.
func (p *Provider) Poll() ([]provider.Update, error) {
	p.discover()

	// Parents first, so a completion recorded this tick lands on the agent in
	// the same tick.
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
	for _, at := range p.agents {
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
		if st, changed := p.settle(at); changed {
			events = append(events, model.Event{
				AgentID: at.agent.ID,
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
			events[i].AgentID = at.agent.ID
		}
		updates = append(updates, provider.Update{Agent: at.agent, Events: events})
	}
	return updates, nil
}

// discover finds subagent transcripts that appeared since the last tick. omp
// nests a subagent's own subagents one directory deeper, so both levels are
// globbed and the depth is read back off the path.
func (p *Provider) discover() {
	for depth, pattern := range map[int]string{
		1: filepath.Join(p.root, "*", "*", "*.jsonl"),
		2: filepath.Join(p.root, "*", "*", "*", "*.jsonl"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, path := range matches {
			p.adopt(path, depth)
		}
	}
}

// adopt registers one subagent transcript.
func (p *Provider) adopt(path string, depth int) {
	if _, ok := p.agents[path]; ok {
		return
	}
	header, ok := readHeader(path)
	if !ok {
		return
	}

	jobID := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	native := header.sessionID
	if native == "" {
		native = jobID
	}
	parent := strings.TrimSuffix(filepath.Dir(path), string(filepath.Separator)) + ".jsonl"
	if _, ok := p.parents[parent]; !ok {
		p.parents[parent] = &parentTail{tail: tailer.New(parent), jobs: map[string]job{}}
	}

	started, backlog := time.Now(), false
	if fi, err := os.Stat(path); err == nil {
		started = fi.ModTime()
		backlog = fi.ModTime().Before(p.since)
	}
	if !header.started.IsZero() {
		started = header.started
	}

	at := &agentTail{
		agent: model.Agent{
			ID:       Name + ":" + native,
			NativeID: native,
			Provider: Name,
			Name:     header.agent,
			Nickname: jobID,
			Title:    taskTitle(header.task),
			Prompt:   provider.Clip(header.task, 4000),
			Model:    cleanModel(header.model),
			Effort:   header.effort,
			Cwd:      header.cwd,
			Depth:    depth,
			Parent:   parentNativeID(path, depth),
			Source:   path,
			Started:  started,
			Status:   model.StatusLive,
		},
		tail:     tailer.New(path),
		parent:   parent,
		jobID:    jobID,
		lastSeen: time.Now(),
		dirty:    true,
	}
	if backlog {
		// Index it for history, but do not replay it into a live pane. The
		// header already carries everything history search matches on.
		at.tail.SeekEnd()
		at.agent.Backlog = true
		at.agent.Status = model.StatusDone
		at.agent.Updated = started
		at.finished = true
	}
	p.agents[path] = at
}

// parentNativeID names the session that spawned this agent: the enclosing
// session for a direct subagent, and the enclosing *agent* for a nested one.
func parentNativeID(path string, depth int) string {
	dir := filepath.Dir(path)
	if depth > 1 {
		// .../<ts>_<sessionId>/<parent agent>/<id>.jsonl
		if header, ok := readHeader(dir + ".jsonl"); ok && header.sessionID != "" {
			return header.sessionID
		}
		return filepath.Base(dir)
	}
	// .../<ts>_<sessionId>/<id>.jsonl — the directory is named for the session
	// file that owns it, whose id follows the timestamp.
	base := filepath.Base(dir)
	if i := strings.Index(base, "_"); sessionDirRe.MatchString(base) && i >= 0 {
		return base[i+1:]
	}
	return base
}

// settle resolves the agent's status: its own yield is the completion it
// controls, the parent's settled job is authoritative for how it ended, and a
// silent transcript falls back to the idle timer.
func (p *Provider) settle(at *agentTail) (model.Status, bool) {
	if at.finished {
		return at.agent.Status, false
	}
	if pt, ok := p.parents[at.parent]; ok {
		if j, done := pt.jobs[at.jobID]; done {
			at.finished = true
			at.agent.Status = model.StatusDone
			if j.status != "completed" {
				at.agent.Status = model.StatusError
			}
			if j.label != "" {
				at.agent.Title = j.label
			}
			at.agent.DurationMS = j.durationMS
			if at.agent.DurationMS == 0 {
				at.agent.DurationMS = duration(at.agent)
			}
			return at.agent.Status, true
		}
	}
	if at.agent.Status == model.StatusDone || at.agent.Status == model.StatusError {
		// yield or a session exit already settled it while parsing.
		at.finished = true
		at.agent.DurationMS = duration(at.agent)
		return at.agent.Status, true
	}
	if p.idleDone > 0 && !at.lastSeen.IsZero() && time.Since(at.lastSeen) > p.idleDone {
		at.finished = true
		at.agent.Status = model.StatusDone
		at.agent.DurationMS = duration(at.agent)
		return at.agent.Status, true
	}
	return at.agent.Status, false
}

func duration(a model.Agent) int64 {
	if a.Started.IsZero() || a.Updated.Before(a.Started) {
		return 0
	}
	return a.Updated.Sub(a.Started).Milliseconds()
}

// scan records settled subagent jobs from the spawning session.
func (pt *parentTail) scan(line []byte) {
	var l transcriptLine
	if err := json.Unmarshal(line, &l); err != nil {
		return
	}
	switch l.Type {
	case "message":
		var msg message
		if err := json.Unmarshal(l.Message, &msg); err != nil {
			return
		}
		if msg.Role != "toolResult" || msg.ToolName != "hub" || len(msg.Details) == 0 {
			return
		}
		var details struct {
			Jobs []struct {
				ID         string `json:"id"`
				Type       string `json:"type"`
				Status     string `json:"status"`
				Label      string `json:"label"`
				DurationMS int64  `json:"durationMs"`
			} `json:"jobs"`
		}
		if json.Unmarshal(msg.Details, &details) != nil {
			return
		}
		for _, j := range details.Jobs {
			if j.Type != "task" || j.ID == "" || settling(j.Status) {
				continue
			}
			pt.jobs[j.ID] = job{status: j.Status, durationMS: j.DurationMS, label: j.Label}
		}
	case "custom_message":
		// A yielded agent whose parent never called hub gets its result injected
		// instead; the block names the agent and how it ended.
		for _, m := range taskResultRe.FindAllStringSubmatch(l.Content, -1) {
			j := job{status: m[2]}
			if len(m) > 3 {
				j.durationMS = parseDuration(m[3])
			}
			pt.jobs[m[1]] = j
		}
	}
}

// settling reports whether a job is still in flight.
func settling(status string) bool {
	return status == "" || status == "running" || status == "pending" || status == "queued"
}

// parseDuration reads omp's compact "2m4s" job durations.
func parseDuration(s string) int64 {
	var total, value int64
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			value = value*10 + int64(r-'0')
		case r == 'h':
			total, value = total+value*3600_000, 0
		case r == 'm':
			total, value = total+value*60_000, 0
		case r == 's':
			total, value = total+value*1000, 0
		default:
			return total
		}
	}
	return total
}

// headerLines bounds how far into a transcript the metadata sniff reads.
const headerLines = 40

type header struct {
	sessionID string
	cwd       string
	agent     string
	task      string
	model     string
	effort    string
	started   time.Time
}

// readHeader pulls the session and session_init lines that describe an omp
// transcript without consuming it, so discovery can name an agent immediately.
func readHeader(path string) (header, bool) {
	f, err := os.Open(path)
	if err != nil {
		return header{}, false
	}
	defer f.Close()

	var h header
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 0; n < headerLines && sc.Scan(); n++ {
		var l transcriptLine
		if json.Unmarshal(sc.Bytes(), &l) != nil {
			continue
		}
		switch l.Type {
		case "session":
			h.sessionID, h.cwd = l.ID, l.Cwd
			if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
				h.started = t
			}
		case "session_init":
			h.agent, h.task = l.Agent, l.Task
			if l.ResolvedModel != "" {
				h.model = l.ResolvedModel
			}
		case "model_change":
			if h.model == "" && l.Model != "" {
				h.model = l.Model
			}
		case "thinking_level_change":
			h.effort = l.ThinkingLevel
		}
		if h.sessionID != "" && h.agent != "" && h.task != "" {
			break
		}
	}
	return h, h.sessionID != "" || h.agent != ""
}

// taskTitle names a run from the task it was handed. omp wraps every
// assignment in a fixed preamble and the tasks themselves are usually
// markdown, so the first line is boilerplate and the second is often just a
// heading; skip both to reach the sentence that actually describes the work.
func taskTitle(task string) string {
	var parts []string
	for _, line := range strings.Split(task, "\n") {
		line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#"))
		if line == "" || strings.EqualFold(line, "Complete assignment thoroughly:") {
			continue
		}
		parts = append(parts, strings.TrimSpace(line))
		// A short lead line is a section heading; carry the next line with it.
		if len([]rune(parts[0])) >= 24 || len(parts) == 2 {
			break
		}
	}
	return provider.Clip(strings.Join(parts, " — "), 120)
}

// cleanModel drops the provider prefix omp carries in resolved model names.
func cleanModel(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

type transcriptLine struct {
	Type          string          `json:"type"`
	CustomType    string          `json:"customType"`
	ID            string          `json:"id"`
	Timestamp     string          `json:"timestamp"`
	Cwd           string          `json:"cwd"`
	Message       json.RawMessage `json:"message"`
	Content       string          `json:"content"`
	Display       *bool           `json:"display"`
	Model         string          `json:"model"`
	ThinkingLevel string          `json:"thinkingLevel"`
	Agent         string          `json:"agent"`
	Task          string          `json:"task"`
	ResolvedModel string          `json:"resolvedModel"`
	Data          json.RawMessage `json:"data"`
}

type message struct {
	Role     string          `json:"role"`
	Content  json.RawMessage `json:"content"`
	Model    string          `json:"model"`
	Provider string          `json:"provider"`
	ToolName string          `json:"toolName"`
	IsError  bool            `json:"isError"`
	Details  json.RawMessage `json:"details"`
	Usage    *usage          `json:"usage"`
}

type usage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cacheRead"`
	CacheWrite int `json:"cacheWrite"`
}

type block struct {
	Type      string         `json:"type"`
	Text      string         `json:"text"`
	Thinking  string         `json:"thinking"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
	Intent    string         `json:"intent"`
}

// blocks normalizes message.content, which is a list of typed blocks or, for
// injected messages, a bare string.
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
	if at.agent.Started.IsZero() || ts.Before(at.agent.Started) {
		at.agent.Started = ts
	}

	switch l.Type {
	case "session":
		if l.Cwd != "" {
			at.agent.Cwd = l.Cwd
		}
		return nil
	case "session_init":
		if l.Agent != "" {
			at.agent.Name = l.Agent
		}
		if l.Task != "" {
			at.agent.Prompt = provider.Clip(l.Task, 4000)
			if at.agent.Title == "" {
				at.agent.Title = taskTitle(l.Task)
			}
		}
		if l.ResolvedModel != "" {
			at.agent.Model = cleanModel(l.ResolvedModel)
		}
		return nil
	case "model_change":
		if l.Model != "" {
			at.agent.Model = cleanModel(l.Model)
		}
		return nil
	case "thinking_level_change":
		at.agent.Effort = l.ThinkingLevel
		return nil
	case "custom":
		// A session that exits on a signal never yielded; anything else is an
		// ordinary teardown after the agent already finished.
		if l.CustomType == "session_exit" {
			var data struct {
				Reason string `json:"reason"`
				Kind   string `json:"kind"`
			}
			if json.Unmarshal(l.Data, &data) == nil && data.Kind == "signal" && !at.finished {
				at.agent.Status = model.StatusError
				return []model.Event{{
					TS:     ts,
					Kind:   model.EvNotice,
					Header: "session exited (" + data.Reason + ")",
					Err:    true,
				}}
			}
		}
		return nil
	case "custom_message":
		if l.Display == nil || !*l.Display || strings.TrimSpace(l.Content) == "" {
			return nil
		}
		return []model.Event{{
			TS:     ts,
			Kind:   model.EvNotice,
			Header: provider.FirstLine(l.Content, 160),
			Body:   l.Content,
			Lines:  provider.CountLines(l.Content),
		}}
	case "message":
	default:
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
		// Per-request counts: input and cache reflect the request that was just
		// billed, output is genuinely per-message and accumulates.
		at.agent.Tokens.In = u.Input
		at.agent.Tokens.CacheRead = u.CacheRead
		at.agent.Tokens.CacheWrite = u.CacheWrite
		at.agent.Tokens.Out += u.Output
		at.agent.Tokens.Total = at.agent.Tokens.In + at.agent.Tokens.Out + at.agent.Tokens.CacheRead
	}

	if msg.Role == "toolResult" {
		body := textOf(msg.blocks())
		head := provider.FirstLine(body, 160)
		if head == "" {
			head = "(no output)"
		}
		// The hidden yield tool is how every omp subagent must finish.
		if msg.ToolName == "yield" && !msg.IsError {
			at.agent.Status = model.StatusDone
		}
		return []model.Event{{
			TS:       ts,
			Kind:     model.EvToolResult,
			Tool:     msg.ToolName,
			Header:   head,
			Body:     body,
			Lines:    provider.CountLines(body),
			Err:      msg.IsError,
			Overflow: provider.DetectOverflow(body),
		}}
	}

	var events []model.Event
	for _, b := range msg.blocks() {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) == "" {
				continue
			}
			kind := model.EvText
			if msg.Role == "user" {
				kind = model.EvUser
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
			// Anthropic returns signature-only thinking blocks whose text is
			// empty; they carry nothing to show.
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
		case "toolCall":
			at.agent.ToolCount++
			body := provider.PrettyJSON(b.Arguments)
			header := provider.ToolHeader(b.Name, b.Arguments)
			if b.Intent != "" {
				header = b.Name + "  " + provider.FirstLine(b.Intent, 140)
			}
			events = append(events, model.Event{
				TS:     ts,
				Kind:   model.EvToolUse,
				Tool:   b.Name,
				Header: header,
				Body:   body,
				Lines:  provider.CountLines(body),
			})
		}
	}
	return events
}

// textOf concatenates the text blocks of a tool result.
func textOf(blocks []block) string {
	var sb strings.Builder
	for _, b := range blocks {
		if b.Text != "" {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}
