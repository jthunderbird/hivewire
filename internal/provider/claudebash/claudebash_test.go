package claudebash

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

// fixture builds a projects tree holding one top-level session transcript,
// mirroring the real on-disk layout the launch and its receipt appear in.
type fixture struct {
	root    string
	session string
	output  string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "-home-user-proj")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	f := fixture{
		root:    root,
		session: filepath.Join(dir, "sess.jsonl"),
		output:  filepath.Join(dir, "sess", "tasks", "bz8task79.output"),
	}
	if err := os.MkdirAll(filepath.Dir(f.output), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.session, nil, 0o644); err != nil {
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

// ts returns a timestamp offset from the current moment. Fixtures use this
// instead of a hardcoded date so "since" cutoffs in tests remain meaningful
// regardless of when the suite runs.
func ts(offset time.Duration) string {
	return time.Now().Add(offset).UTC().Format(time.RFC3339Nano)
}

func lineLaunch(cmd, desc, toolUseID string) string {
	return `{"type":"assistant","timestamp":"` + ts(0) + `","cwd":"/home/user/proj","gitBranch":"main","version":"2.1.220","message":{"role":"assistant","content":[{"type":"tool_use","id":"` + toolUseID + `","name":"Bash","input":{"command":"` + cmd + `","description":"` + desc + `","run_in_background":true}}]}}`
}

func lineReceipt(toolUseID, taskID, outputPath string) string {
	return `{"type":"user","timestamp":"` + ts(time.Millisecond) + `","toolUseResult":{"stdout":"","stderr":"","backgroundTaskId":"` + taskID + `"},"message":{"role":"user","content":[{"tool_use_id":"` + toolUseID + `","type":"tool_result","content":"Command running in background with ID: ` + taskID + `. Output is being written to: ` + outputPath + `. You will be notified when it completes."}]}}`
}

func lineNotificationDone(taskID, status, summary string) string {
	return `{"type":"queue-operation","operation":"enqueue","timestamp":"` + ts(2*time.Second) + `","content":"<task-notification>\n<task-id>` + taskID + `</task-id>\n<status>` + status + `</status>\n<summary>` + summary + `</summary>\n</task-notification>"}`
}

func poll(t *testing.T, p *Provider) []provider.Update {
	t.Helper()
	u, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestLaunchReceiptCreatesALiveTask(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.session, lineLaunch("iperf3 -s -p 5201", "Start iperf3 server", "toolu_1"))
	f.append(t, f.session, lineReceipt("toolu_1", "bz8task79", f.output))

	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	updates := poll(t, p)
	if len(updates) != 1 {
		t.Fatalf("expected one task, got %d", len(updates))
	}
	a, events := updates[0].Agent, updates[0].Events
	if a.Provider != Name || a.NativeID != "bz8task79" {
		t.Fatalf("agent identity = %+v", a)
	}
	if a.Name != "bash" || a.Nickname != "bz8task79" {
		t.Errorf("label fields = name %q nickname %q, want bash (bz8task79)", a.Name, a.Nickname)
	}
	if a.Title != "Start iperf3 server" || a.Prompt != "iperf3 -s -p 5201" {
		t.Errorf("title/prompt = %+v", a)
	}
	if a.Cwd != "/home/user/proj" || a.GitBranch != "main" || a.CLIVersion != "2.1.220" {
		t.Errorf("host metadata not applied: %+v", a)
	}
	if a.Parent != "sess" {
		t.Errorf("Parent = %q, want the session id", a.Parent)
	}
	if a.Status != model.StatusLive {
		t.Errorf("status = %q, want live", a.Status)
	}
	if a.Model != "" {
		t.Errorf("Model = %q, want empty — a background task runs no model", a.Model)
	}
	if len(events) != 1 || events[0].Kind != model.EvToolUse || !strings.Contains(events[0].Header, "iperf3 -s -p 5201") {
		t.Fatalf("expected one tool_use launch event naming the command, got %+v", events)
	}
}

func TestOutputIsStreamedAsToolResultChunks(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.session, lineLaunch("ping -c100 host", "ping test", "toolu_1"))
	f.append(t, f.session, lineReceipt("toolu_1", "bz8task79", f.output))
	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	poll(t, p)

	f.append(t, f.output, "line one", "line two")
	updates := poll(t, p)
	if len(updates) != 1 {
		t.Fatalf("expected an update carrying the new output, got %d", len(updates))
	}
	events := updates[0].Events
	if len(events) != 1 || events[0].Kind != model.EvToolResult {
		t.Fatalf("expected one tool_result chunk, got %+v", events)
	}
	if !strings.Contains(events[0].Body, "line one") || !strings.Contains(events[0].Body, "line two") {
		t.Errorf("body = %q, want both lines", events[0].Body)
	}
}

func TestCompletionComesFromTheTaskNotification(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.session, lineLaunch("go build ./...", "build", "toolu_1"))
	f.append(t, f.session, lineReceipt("toolu_1", "bz8task79", f.output))
	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	poll(t, p)

	f.append(t, f.session, lineNotificationDone("bz8task79", "completed", `Background command \"build\" completed (exit code 0)`))
	updates := poll(t, p)
	if len(updates) != 1 {
		t.Fatalf("expected a status update, got %d", len(updates))
	}
	a := updates[0].Agent
	if a.Status != model.StatusDone {
		t.Fatalf("status = %q, want done", a.Status)
	}
	kinds := map[model.EventKind]int{}
	for _, e := range updates[0].Events {
		kinds[e.Kind]++
	}
	if kinds[model.EvStatus] != 1 {
		t.Fatalf("expected a status event, got %+v", updates[0].Events)
	}
}

func TestFailedNotificationMarksTheTaskErrored(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.session, lineLaunch("flaky-cmd", "flaky", "toolu_1"))
	f.append(t, f.session, lineReceipt("toolu_1", "bz8task79", f.output))
	f.append(t, f.session, lineNotificationDone("bz8task79", "failed", `Background command \"flaky\" failed with exit code 1`))

	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	updates := poll(t, p)
	if updates[0].Agent.Status != model.StatusError {
		t.Fatalf("status = %q, want error", updates[0].Agent.Status)
	}
}

func TestPreexistingLaunchesAreBacklogNotLive(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.session, lineLaunch("old-cmd", "old", "toolu_1"))
	f.append(t, f.session, lineReceipt("toolu_1", "bz8task79", f.output))
	if err := os.WriteFile(f.output, []byte("stale output\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// since is after the launch's own timestamp, so it counts as pre-existing.
	p := New(f.root, time.Minute, time.Now().Add(time.Hour))
	updates := poll(t, p)
	if len(updates) != 1 {
		t.Fatalf("backlog tasks should still be indexed, got %d updates", len(updates))
	}
	if !updates[0].Agent.Backlog {
		t.Error("pre-existing launch should be marked backlog")
	}
	if updates[0].Agent.Status != model.StatusDone {
		t.Errorf("status = %q, want done", updates[0].Agent.Status)
	}
	if len(updates[0].Events) != 0 {
		t.Errorf("backlog tasks must not be replayed, got %d events", len(updates[0].Events))
	}
}

func TestIdleTimeoutCompletesATaskWithNoNotification(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.session, lineLaunch("long-cmd", "long", "toolu_1"))
	f.append(t, f.session, lineReceipt("toolu_1", "bz8task79", f.output))

	p := New(f.root, time.Millisecond, time.Now().Add(-time.Hour))
	poll(t, p)
	time.Sleep(5 * time.Millisecond)
	updates := poll(t, p)
	if len(updates) != 1 || updates[0].Agent.Status != model.StatusDone {
		t.Fatalf("expected idle timeout to complete the task, got %+v", updates)
	}
}

func TestForegroundBashIsIgnored(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.session, `{"type":"assistant","timestamp":"2026-08-04T03:56:00.000Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls","description":"list files"}}]}}`)
	f.append(t, f.session, `{"type":"user","timestamp":"2026-08-04T03:56:00.500Z","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"a.txt\nb.txt"}]}}`)

	p := New(f.root, time.Minute, time.Now().Add(-time.Hour))
	updates := poll(t, p)
	if len(updates) != 0 {
		t.Fatalf("a foreground Bash call must not become a task, got %+v", updates)
	}
}

func TestSubagentHostReportsTheSubagentAsParent(t *testing.T) {
	root := t.TempDir()
	subs := filepath.Join(root, "-home-user-proj", "sess", "subagents")
	if err := os.MkdirAll(subs, 0o755); err != nil {
		t.Fatal(err)
	}
	agentFile := filepath.Join(subs, "agent-helper.jsonl")
	if err := os.WriteFile(agentFile, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(subs, "tasks", "btask.output")
	if err := os.MkdirAll(filepath.Dir(output), 0o755); err != nil {
		t.Fatal(err)
	}

	appendLine := func(path, line string) {
		fh, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		defer fh.Close()
		if _, err := fh.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	appendLine(agentFile, lineLaunch("scan the repo", "scan", "toolu_1"))
	appendLine(agentFile, lineReceipt("toolu_1", "btask", output))

	p := New(root, time.Minute, time.Now().Add(-time.Hour))
	updates := poll(t, p)
	if len(updates) != 1 {
		t.Fatalf("expected one task, got %d", len(updates))
	}
	if updates[0].Agent.Parent != "helper" {
		t.Fatalf("Parent = %q, want the subagent's native id", updates[0].Agent.Parent)
	}
}

func TestReplayRebuildsTheStreamFromDisk(t *testing.T) {
	f := newFixture(t)
	f.append(t, f.session, lineLaunch("go test ./...", "run tests", "toolu_1"))
	f.append(t, f.session, lineReceipt("toolu_1", "bz8task79", f.output))
	f.append(t, f.session, lineNotificationDone("bz8task79", "completed", `Background command \"run tests\" completed (exit code 0)`))
	if err := os.WriteFile(f.output, []byte("ok\tpkg\t0.5s\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	a, events, err := Replay(f.session, "bz8task79")
	if err != nil {
		t.Fatal(err)
	}
	if a.Title != "run tests" || a.Prompt != "go test ./..." {
		t.Errorf("replayed agent = %+v", a)
	}
	if a.Status != model.StatusDone {
		t.Errorf("status = %q, want done", a.Status)
	}
	if len(events) != 2 {
		t.Fatalf("replayed %d events, want 2 (launch + output)", len(events))
	}
	if events[0].Kind != model.EvToolUse || events[1].Kind != model.EvToolResult {
		t.Fatalf("event kinds = %v, %v", events[0].Kind, events[1].Kind)
	}
	if !strings.Contains(events[1].Body, "ok\tpkg") {
		t.Errorf("output event body = %q", events[1].Body)
	}
}
