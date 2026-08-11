// Package codex adapts Codex CLI's on-disk rollout transcripts.
//
// Codex writes one rollout JSONL per thread:
//
//	~/.codex/sessions/YYYY/MM/DD/rollout-<ts>-<thread id>.jsonl
//
// A subagent is just a thread whose session_meta says thread_source=="subagent";
// that same first line carries parent_thread_id, spawn depth, agent_path and the
// agent nickname. Everything hivewire needs is therefore in the file itself —
// no hooks to install and no sqlite state to read.
package codex

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
	"github.com/jtaylor/hivewire/internal/tailer"
)

// Name is the provider identifier used in agent IDs and the UI.
const Name = "codex"

// Provider discovers and tails Codex subagent rollouts.
type Provider struct {
	root     string // ~/.codex/sessions
	idleDone time.Duration
	since    time.Time // rollouts older than this are history, not live

	agents  map[string]*agentTail
	skipped map[string]bool // rollouts already classified as not-a-subagent
}

// New returns a provider rooted at the given sessions directory. Rollouts last
// modified before since are indexed as backlog rather than streamed.
func New(root string, idleDone time.Duration, since time.Time) *Provider {
	return &Provider{
		root:     root,
		idleDone: idleDone,
		since:    since,
		agents:   map[string]*agentTail{},
		skipped:  map[string]bool{},
	}
}

func (p *Provider) Name() string { return Name }

type agentTail struct {
	agent    model.Agent
	tail     *tailer.Tailer
	lastSeen time.Time
	finished bool
	dirty    bool // has state the hub has not seen yet
}

// Poll discovers new subagent rollouts and drains newly appended lines.
func (p *Provider) Poll() ([]provider.Update, error) {
	p.discover()

	var updates []provider.Update
	for id, at := range p.agents {
		lines, err := at.tail.Poll()
		if err != nil {
			continue
		}
		// The wake-up check must happen before parsing: a batch that *contains*
		// task_complete would otherwise be read as activity after completion and
		// flip the agent straight back to live.
		if len(lines) > 0 && at.finished && at.agent.Status != model.StatusError && !at.agent.Backlog {
			at.finished = false
			at.agent.Status = model.StatusLive
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

func (p *Provider) settle(at *agentTail) (model.Status, bool) {
	if at.finished || at.agent.Status == model.StatusError {
		return at.agent.Status, false
	}
	if p.idleDone > 0 && !at.lastSeen.IsZero() && time.Since(at.lastSeen) > p.idleDone {
		at.finished = true
		at.agent.Status = model.StatusDone
		return at.agent.Status, true
	}
	return at.agent.Status, false
}

// discover looks for rollout files and admits the ones whose session_meta marks
// them as subagent threads.
func (p *Provider) discover() {
	matches, err := filepath.Glob(filepath.Join(p.root, "*", "*", "*", "rollout-*.jsonl"))
	if err != nil {
		return
	}
	for _, path := range matches {
		if p.skipped[path] {
			continue
		}
		meta, ok := readSessionMeta(path)
		if !ok {
			continue // first line not flushed yet; retry next tick
		}
		if meta.ThreadSource != "subagent" {
			p.skipped[path] = true
			continue
		}
		id := Name + ":" + meta.ID
		if _, exists := p.agents[id]; exists {
			p.skipped[path] = true
			continue
		}

		spawn := meta.Source.Subagent.ThreadSpawn
		started := time.Now()
		if t, err := time.Parse(time.RFC3339Nano, meta.Timestamp); err == nil {
			started = t
		}
		backlog := false
		if fi, err := os.Stat(path); err == nil {
			backlog = fi.ModTime().Before(p.since)
		}
		at := &agentTail{
			agent: model.Agent{
				ID:         id,
				NativeID:   meta.ID,
				Provider:   Name,
				Name:       filepath.Base(spawn.AgentPath),
				Nickname:   firstNonEmpty(spawn.AgentNickname, meta.AgentNickname),
				Depth:      spawn.Depth,
				Parent:     firstNonEmpty(spawn.ParentThreadID, meta.ParentThreadID),
				Cwd:        meta.Cwd,
				CLIVersion: meta.CLIVersion,
				Source:     path,
				Started:    started,
				Status:     model.StatusLive,
			},
			tail:     tailer.New(path),
			lastSeen: time.Now(),
			dirty:    true,
		}
		if backlog {
			// Index it for history, but do not replay it into a live pane.
			// Sniff the head first so history search can match its prompt.
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

// sniffLines bounds how far into a pre-existing rollout we read purely to
// recover searchable metadata.
const sniffLines = 200

// sniff parses the head of a rollout for its prompt and model, discarding the
// events, so transcripts that predate hivewire are still searchable by prompt.
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
	at.agent.Tokens = model.Tokens{}
	at.agent.ToolCount = 0
}

type sessionMeta struct {
	ID             string `json:"id"`
	ParentThreadID string `json:"parent_thread_id"`
	Timestamp      string `json:"timestamp"`
	Cwd            string `json:"cwd"`
	CLIVersion     string `json:"cli_version"`
	ThreadSource   string `json:"thread_source"`
	AgentNickname  string `json:"agent_nickname"`
	AgentPath      string `json:"agent_path"`
	Source         struct {
		Subagent struct {
			ThreadSpawn struct {
				ParentThreadID string `json:"parent_thread_id"`
				Depth          int    `json:"depth"`
				AgentPath      string `json:"agent_path"`
				AgentNickname  string `json:"agent_nickname"`
			} `json:"thread_spawn"`
		} `json:"subagent"`
	} `json:"source"`
}

// readSessionMeta reads just the first line of a rollout. The `source` field is
// a bare string ("exec", "cli") for user threads and an object for subagents,
// so it is decoded leniently.
func readSessionMeta(path string) (sessionMeta, bool) {
	f, err := os.Open(path)
	if err != nil {
		return sessionMeta{}, false
	}
	defer f.Close()

	buf := make([]byte, 64*1024)
	n, _ := f.Read(buf)
	if n == 0 {
		return sessionMeta{}, false
	}
	nl := strings.IndexByte(string(buf[:n]), '\n')
	if nl < 0 {
		return sessionMeta{}, false
	}

	var line struct {
		Type    string          `json:"type"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(buf[:nl], &line); err != nil || line.Type != "session_meta" {
		return sessionMeta{}, false
	}

	// Probe thread_source before the strict decode, so a string-valued `source`
	// on a user thread cannot make us discard a line we understood fine.
	var probe struct {
		ThreadSource string `json:"thread_source"`
	}
	_ = json.Unmarshal(line.Payload, &probe)
	if probe.ThreadSource != "subagent" {
		return sessionMeta{ThreadSource: probe.ThreadSource}, true
	}

	var meta sessionMeta
	if err := json.Unmarshal(line.Payload, &meta); err != nil {
		return sessionMeta{}, false
	}
	return meta, true
}

type rolloutLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type payloadHead struct {
	Type string `json:"type"`
}

// parse converts one rollout line into zero or more stream events.
func (at *agentTail) parse(line []byte) []model.Event {
	var l rolloutLine
	if err := json.Unmarshal(line, &l); err != nil {
		return nil
	}
	ts := time.Now()
	if l.Timestamp != "" {
		if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
			ts = t
		}
	}
	at.agent.Updated = ts

	var head payloadHead
	_ = json.Unmarshal(l.Payload, &head)

	switch l.Type {
	case "turn_context":
		at.applyTurnContext(l.Payload)
		return nil
	case "event_msg":
		return at.parseEvent(head.Type, l.Payload, ts)
	case "response_item":
		return at.parseItem(head.Type, l.Payload, ts)
	}
	return nil
}

func (at *agentTail) applyTurnContext(raw json.RawMessage) {
	var tc struct {
		Model          string `json:"model"`
		ApprovalPolicy string `json:"approval_policy"`
		Effort         string `json:"effort"`
		SandboxPolicy  struct {
			Type string `json:"type"`
		} `json:"sandbox_policy"`
		CollaborationMode struct {
			Settings struct {
				ReasoningEffort string `json:"reasoning_effort"`
			} `json:"settings"`
		} `json:"collaboration_mode"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return
	}
	if tc.Model != "" {
		at.agent.Model = tc.Model
	}
	if tc.ApprovalPolicy != "" {
		at.agent.Approval = tc.ApprovalPolicy
	}
	if tc.SandboxPolicy.Type != "" {
		at.agent.Sandbox = tc.SandboxPolicy.Type
	}
	if e := firstNonEmpty(tc.Effort, tc.CollaborationMode.Settings.ReasoningEffort); e != "" {
		at.agent.Effort = e
	}
}

func (at *agentTail) parseEvent(kind string, raw json.RawMessage, ts time.Time) []model.Event {
	switch kind {
	case "user_message":
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &p)
		if strings.TrimSpace(p.Message) == "" {
			return nil
		}
		if at.agent.Title == "" {
			at.agent.Title = provider.FirstLine(p.Message, 120)
		}
		if at.agent.Prompt == "" {
			at.agent.Prompt = provider.Clip(p.Message, 4000)
		}
		return []model.Event{{
			TS: ts, Kind: model.EvUser,
			Header: provider.FirstLine(p.Message, 160),
			Body:   p.Message, Lines: provider.CountLines(p.Message),
		}}

	case "agent_message":
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &p)
		if strings.TrimSpace(p.Message) == "" {
			return nil
		}
		return []model.Event{{
			TS: ts, Kind: model.EvText,
			Header: provider.FirstLine(p.Message, 160),
			Body:   p.Message, Lines: provider.CountLines(p.Message),
		}}

	case "token_count":
		var p struct {
			Info struct {
				Total struct {
					Input     int `json:"input_tokens"`
					Cached    int `json:"cached_input_tokens"`
					CacheWr   int `json:"cache_write_input_tokens"`
					Output    int `json:"output_tokens"`
					Reasoning int `json:"reasoning_output_tokens"`
					Total     int `json:"total_tokens"`
				} `json:"total_token_usage"`
				ContextWindow int `json:"model_context_window"`
			} `json:"info"`
		}
		if err := json.Unmarshal(raw, &p); err == nil {
			t := p.Info.Total
			at.agent.Tokens = model.Tokens{
				In: t.Input, Out: t.Output, CacheRead: t.Cached, CacheWrite: t.CacheWr,
				Reasoning: t.Reasoning, Total: t.Total, ContextWindow: p.Info.ContextWindow,
			}
		}
		return nil

	case "task_complete":
		var p struct {
			DurationMS int64 `json:"duration_ms"`
		}
		_ = json.Unmarshal(raw, &p)
		if p.DurationMS > 0 {
			at.agent.DurationMS = p.DurationMS
		}
		at.finished = true
		at.agent.Status = model.StatusDone
		return []model.Event{{
			TS: ts, Kind: model.EvStatus, Header: "agent done",
		}}

	case "error", "stream_error":
		var p struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &p)
		at.agent.Status = model.StatusError
		at.finished = true
		return []model.Event{{
			TS: ts, Kind: model.EvNotice, Err: true,
			Header: provider.FirstLine(firstNonEmpty(p.Message, "agent error"), 160),
			Body:   p.Message,
		}}
	}
	return nil
}

func (at *agentTail) parseItem(kind string, raw json.RawMessage, ts time.Time) []model.Event {
	switch kind {
	case "custom_tool_call", "function_call":
		var p struct {
			Name      string `json:"name"`
			Input     string `json:"input"`
			Arguments string `json:"arguments"`
		}
		_ = json.Unmarshal(raw, &p)
		body := firstNonEmpty(p.Input, p.Arguments)
		at.agent.ToolCount++
		return []model.Event{{
			TS: ts, Kind: model.EvToolUse, Tool: p.Name,
			Header: strings.TrimSpace(p.Name + "  " + provider.FirstLine(body, 160)),
			Body:   body, Lines: provider.CountLines(body),
		}}

	case "custom_tool_call_output", "function_call_output":
		body := extractText(raw)
		head := provider.FirstLine(body, 160)
		if head == "" {
			head = "(no output)"
		}
		return []model.Event{{
			TS: ts, Kind: model.EvToolResult,
			Header: head, Body: body, Lines: provider.CountLines(body),
			Overflow: provider.DetectOverflow(body),
		}}

	case "reasoning":
		// Reasoning content is encrypted unless summaries are enabled; only the
		// plaintext summary is ever renderable.
		var p struct {
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		}
		_ = json.Unmarshal(raw, &p)
		var parts []string
		for _, s := range p.Summary {
			if strings.TrimSpace(s.Text) != "" {
				parts = append(parts, s.Text)
			}
		}
		if len(parts) == 0 {
			return nil
		}
		body := strings.Join(parts, "\n")
		return []model.Event{{
			TS: ts, Kind: model.EvReasoning,
			Header: provider.FirstLine(body, 160), Body: body,
			Lines: provider.CountLines(body),
		}}

	case "agent_message":
		// Inter-agent routing. Only the envelope is plaintext — the task itself
		// sits in the encrypted_content sibling — so it is shown as an event but
		// never used as the title or the searchable prompt, where it would just
		// repeat the agent's name.
		body := extractText(raw)
		if strings.TrimSpace(body) == "" {
			return nil
		}
		if !isRoutingEnvelope(body) {
			if at.agent.Title == "" {
				at.agent.Title = provider.FirstLine(body, 120)
			}
			if at.agent.Prompt == "" {
				at.agent.Prompt = provider.Clip(body, 4000)
			}
		}
		return []model.Event{{
			TS: ts, Kind: model.EvUser,
			Header: provider.FirstLine(body, 160), Body: body,
			Lines: provider.CountLines(body),
		}}

	case "message":
		var p struct {
			Role string `json:"role"`
		}
		_ = json.Unmarshal(raw, &p)
		// developer/system messages are multi-KB static instructions.
		if p.Role == "developer" || p.Role == "system" {
			return nil
		}
		body := extractText(raw)
		if strings.TrimSpace(body) == "" {
			return nil
		}
		kindOut := model.EvText
		if p.Role == "user" {
			kindOut = model.EvUser
		}
		return []model.Event{{
			TS: ts, Kind: kindOut,
			Header: provider.FirstLine(body, 160), Body: body,
			Lines: provider.CountLines(body),
		}}
	}
	return nil
}

// isRoutingEnvelope reports whether text is Codex's inter-agent dispatch header
// rather than real content. The header looks like:
//
//	Message Type: NEW_TASK
//	Task name: /root/count_markdown
//	Sender: /root
//	Payload:
//
// with the actual instruction encrypted alongside it.
func isRoutingEnvelope(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "Message Type:") && strings.Contains(t, "Task name:")
}

// extractText concatenates the plaintext of a payload's content/output array,
// ignoring encrypted or binary parts.
func extractText(raw json.RawMessage) string {
	var p struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, c := range p.Content {
		if c.Text != "" {
			sb.WriteString(c.Text)
		}
	}
	if len(p.Output) > 0 {
		var s string
		if err := json.Unmarshal(p.Output, &s); err == nil {
			sb.WriteString(s)
		} else {
			var list []struct {
				Text string `json:"text"`
			}
			if err := json.Unmarshal(p.Output, &list); err == nil {
				for _, o := range list {
					sb.WriteString(o.Text)
				}
			}
		}
	}
	return sb.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
