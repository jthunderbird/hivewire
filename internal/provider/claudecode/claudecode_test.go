package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

// fixture builds a projects tree holding one subagent transcript plus its
// parent session, mirroring the real on-disk layout.
type fixture struct {
	root   string
	agent  string
	parent string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	subs := filepath.Join(root, "-home-user-proj", "sess", "subagents")
	if err := os.MkdirAll(subs, 0o755); err != nil {
		t.Fatal(err)
	}
	f := fixture{
		root:   root,
		agent:  filepath.Join(subs, "agent-abc123.jsonl"),
		parent: filepath.Join(root, "-home-user-proj", "sess.jsonl"),
	}
	meta := `{"agentType":"Explore","description":"audit the tailer","toolUseId":"toolu_99","spawnDepth":1}`
	if err := os.WriteFile(filepath.Join(subs, "agent-abc123.meta.json"), []byte(meta), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.parent, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f fixture) append(t *testing.T, path string, lines ...string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		if _, err := fh.WriteString(l + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

const (
	lineAssistantTool = `{"type":"assistant","timestamp":"2026-08-11T16:00:01.000Z","cwd":"/home/user/proj","gitBranch":"main","sessionKind":"bg","version":"2.1.227","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"thinking","thinking":"need the line count"},{"type":"tool_use","name":"Bash","input":{"command":"wc -l tui.go","description":"count lines"}}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":5}}}`
	lineToolResult    = `{"type":"user","timestamp":"2026-08-11T16:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"612 tui.go"}]}}`
	lineAssistantText = `{"type":"assistant","timestamp":"2026-08-11T16:00:09.000Z","message":{"role":"assistant","model":"claude-opus-5","content":[{"type":"text","text":"tui.go has 612 lines"}],"usage":{"input_tokens":150,"output_tokens":30}}}`

	// The parent writes a launch receipt immediately; it is not a completion.
	lineAsyncLaunch  = `{"type":"user","timestamp":"2026-08-11T16:00:00.500Z","toolUseResult":{"isAsync":true,"status":"async_launched","agentId":"abc123","resolvedModel":"claude-opus-5"},"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_99","content":"Async agent launched successfully."}]}}`
	lineNotification = `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-08-11T16:00:09.100Z","content":"<task-notification>\n<task-id>abc123</task-id>\n<status>completed</status>\n<usage><duration_ms>8178</duration_ms></usage>\n</task-notification>"}`
	lineNotifyFailed = `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-08-11T16:00:09.100Z","content":"<task-notification>\n<task-id>abc123</task-id>\n<status>failed</status>\n</task-notification>"}`
)

func poll(t *testing.T, p *Provider) []provider.Update {
	t.Helper()
	u, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestParsesToolCallsTextAndMetadata(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.agent, lineAssistantTool, lineToolResult, lineAssistantText)
	f.append(t, f.parent, lineAsyncLaunch)

	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	updates := poll(t, p)
	if len(updates) != 1 {
		t.Fatalf("expected one agent, got %d", len(updates))
	}
	a, events := updates[0].Agent, updates[0].Events

	if a.Name != "Explore" || a.Title != "audit the tailer" || a.Depth != 1 {
		t.Errorf("meta.json not applied: %+v", a)
	}
	if a.Model != "claude-opus-5" || a.Cwd != "/home/user/proj" || a.GitBranch != "main" || a.SessionKind != "bg" {
		t.Errorf("transcript metadata not applied: %+v", a)
	}
	if a.ToolCount != 1 {
		t.Errorf("ToolCount = %d, want 1", a.ToolCount)
	}
	if a.Tokens.Out != 50 || a.Tokens.In != 150 {
		t.Errorf("tokens = %+v; output should accumulate and input should track the latest", a.Tokens)
	}
	if a.Status != model.StatusLive {
		t.Errorf("status = %q, want live — the launch receipt is not a completion", a.Status)
	}

	kinds := map[model.EventKind]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	if kinds[model.EvToolUse] != 1 || kinds[model.EvToolResult] != 1 || kinds[model.EvText] != 1 || kinds[model.EvReasoning] != 1 {
		t.Fatalf("event kinds = %v", kinds)
	}
	for _, e := range events {
		if e.Kind == model.EvToolUse && !strings.Contains(e.Header, "wc -l tui.go") {
			t.Errorf("tool header should name the command, got %q", e.Header)
		}
	}
}

func TestCompletionComesFromTheTaskNotification(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.agent, lineAssistantTool, lineToolResult, lineAssistantText)
	f.append(t, f.parent, lineAsyncLaunch)

	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	poll(t, p)

	f.append(t, f.parent, lineNotification)
	updates := poll(t, p)
	if len(updates) != 1 {
		t.Fatalf("expected a status update, got %d", len(updates))
	}
	a := updates[0].Agent
	if a.Status != model.StatusDone {
		t.Fatalf("status = %q, want done", a.Status)
	}
	if a.DurationMS != 8178 {
		t.Errorf("DurationMS = %d, want the notification's 8178", a.DurationMS)
	}
}

func TestFailedNotificationMarksTheAgentErrored(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.agent, lineAssistantText)
	f.append(t, f.parent, lineAsyncLaunch, lineNotifyFailed)

	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	updates := poll(t, p)
	if updates[0].Agent.Status != model.StatusError {
		t.Fatalf("status = %q, want error", updates[0].Agent.Status)
	}
}

func TestSynchronousToolResultCompletesTheAgent(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.agent, lineAssistantText)
	// No async_launched receipt: this tool_result is the real completion.
	f.append(t, f.parent, `{"type":"user","timestamp":"2026-08-11T16:00:09.500Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_99","content":"done","is_error":true}]}}`)

	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	updates := poll(t, p)
	if updates[0].Agent.Status != model.StatusError {
		t.Fatalf("status = %q, want error from is_error", updates[0].Agent.Status)
	}
}

func TestPreexistingTranscriptsAreBacklogNotLive(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.agent, lineAssistantTool, lineToolResult)

	// since is in the future, so the file counts as pre-existing.
	p := New(f.root, time.Minute, time.Now().Add(time.Hour))
	updates := poll(t, p)
	if len(updates) != 1 {
		t.Fatalf("backlog agents should still be indexed, got %d updates", len(updates))
	}
	if !updates[0].Agent.Backlog {
		t.Error("pre-existing transcript should be marked backlog")
	}
	if len(updates[0].Events) != 0 {
		t.Errorf("backlog transcripts must not be replayed, got %d events", len(updates[0].Events))
	}
}

func TestHarnessTruncationIsSurfacedWithItsFile(t *testing.T) {
	body := `{"type":"user","timestamp":"2026-08-11T16:00:03.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"<persisted-output>\nOutput too large (142.2KB). Full output saved to: /home/user/.claude/projects/p/s/tool-results/x.txt\n\nPreview:\nstuff"}]}}`
	f := newFixture(t)
	f.append(t, f.agent, body)

	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	updates := poll(t, p)

	var found *model.Overflow
	for _, e := range updates[0].Events {
		if e.Overflow != nil {
			found = e.Overflow
		}
	}
	if found == nil {
		t.Fatal("overflow marker not detected")
	}
	if found.Path != "/home/user/.claude/projects/p/s/tool-results/x.txt" {
		t.Errorf("overflow path = %q", found.Path)
	}
	if !strings.Contains(found.Note, "142.2KB") {
		t.Errorf("overflow note should carry the size, got %q", found.Note)
	}
}

func TestReplayRebuildsTheStreamFromDisk(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.agent, lineAssistantTool, lineToolResult, lineAssistantText)

	a, events, err := Replay(f.agent)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "Explore" || a.Model != "claude-opus-5" {
		t.Errorf("replayed agent = %+v", a)
	}
	if len(events) != 4 {
		t.Fatalf("replayed %d events, want 4", len(events))
	}
	if a.DurationMS != 8000 {
		t.Errorf("DurationMS = %d, want 8000 from the transcript timestamps", a.DurationMS)
	}
}

func TestNestedSubagentNamesTheAgentThatSpawnedIt(t *testing.T) {
	root := t.TempDir()
	subs := filepath.Join(root, "-home-user-proj", "sess", "subagents")
	if err := os.MkdirAll(subs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "-home-user-proj", "sess.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// The lead agent issues a Task tool_use; the nested agent's meta names that
	// same tool_use as the thing that spawned it.
	write := func(name, meta, body string) {
		if err := os.WriteFile(filepath.Join(subs, name+".meta.json"), []byte(meta), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(subs, name+".jsonl"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("agent-lead", `{"agentType":"Explore","description":"lead","toolUseId":"toolu_top","spawnDepth":1}`,
		`{"type":"assistant","timestamp":"2026-08-12T03:00:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_nested","name":"Task","input":{"description":"dig deeper"}}]}}`+"\n")
	write("agent-helper", `{"agentType":"Explore","description":"helper","toolUseId":"toolu_nested","spawnDepth":2}`,
		`{"type":"assistant","timestamp":"2026-08-12T03:00:01.000Z","message":{"role":"assistant","content":[{"type":"text","text":"digging"}]}}`+"\n")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	// The spawning tool_use is read on the first poll; the link lands on the next.
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	byNative := map[string]model.Agent{}
	for _, u := range updates {
		byNative[u.Agent.NativeID] = u.Agent
	}
	for _, at := range p.agents {
		byNative[at.agent.NativeID] = at.agent
	}
	if helper := byNative["helper"]; helper.Parent != "lead" || helper.Depth != 2 {
		t.Fatalf("nested agent = depth %d parent %q, want depth 2 under lead", helper.Depth, helper.Parent)
	}
	if lead := byNative["lead"]; lead.Parent != "sess" {
		t.Fatalf("top-level agent parent = %q, want its session", lead.Parent)
	}
}
