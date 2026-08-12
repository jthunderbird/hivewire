package omp

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

// sessionDir is one omp artifacts directory: the session file plus the
// directory beside it that holds subagent transcripts.
func sessionDir(t *testing.T, root, bucket, sessionID string) string {
	t.Helper()
	dir := filepath.Join(root, bucket, "2026-08-12T03-10-44-403Z_"+sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, dir+".jsonl", sessionHeader(sessionID))
	return dir
}

func sessionHeader(id string) []string {
	return []string{
		`{"type":"title","v":1,"title":"","updatedAt":"2026-08-12T03:31:29.316Z"}`,
		fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-08-12T03:31:29.316Z","cwd":"/work/tree"}`, id),
	}
}

// subagent writes a subagent transcript with omp's real header shape.
func subagent(t *testing.T, dir, name, sessionID, agent, task string, extra ...string) string {
	t.Helper()
	lines := append(sessionHeader(sessionID),
		`{"type":"model_change","id":"m1","timestamp":"2026-08-12T03:31:29.382Z","model":"anthropic/claude-sonnet-5:high"}`,
		`{"type":"thinking_level_change","id":"m2","timestamp":"2026-08-12T03:31:29.382Z","thinkingLevel":"high"}`,
		fmt.Sprintf(`{"type":"session_init","id":"m3","timestamp":"2026-08-12T03:31:29.390Z","agent":%q,"modelRole":"smol","resolvedModel":"anthropic/claude-sonnet-5:high","readOnly":true,"task":%q,"systemPrompt":"..."}`, agent, task),
	)
	lines = append(lines, extra...)
	path := filepath.Join(dir, name+".jsonl")
	write(t, path, lines)
	return path
}

func write(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		t.Fatal(err)
	}
}

func assistant(ts, blocks string) string {
	return fmt.Sprintf(`{"type":"message","id":"a1","timestamp":%q,"message":{"role":"assistant","model":"claude-sonnet-5","provider":"anthropic","content":[%s],"usage":{"input":150,"output":200,"cacheRead":9000,"cacheWrite":500,"totalTokens":9850}}}`, ts, blocks)
}

func toolResult(ts, tool, text string, isError bool) string {
	return fmt.Sprintf(`{"type":"message","id":"r1","timestamp":%q,"message":{"role":"toolResult","toolCallId":"c1","toolName":%q,"content":[{"type":"text","text":%q}],"isError":%t}}`, ts, tool, text, isError)
}

func kinds(events []model.Event) []model.EventKind {
	out := make([]model.EventKind, len(events))
	for i := range events {
		out[i] = events[i].Kind
	}
	return out
}

func agentsByNickname(updates []provider.Update) map[string]provider.Update {
	out := map[string]provider.Update{}
	for _, u := range updates {
		out[u.Agent.Nickname] = u
	}
	return out
}

func TestPollAdoptsSubagentsWithSelfDescribingMetadata(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	subagent(t, dir, "ChessNewsScout", "ses-child", "scout", "Fetch today's chess news.\nSecond line ignored in the title.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Fatalf("adopted %d agents, want 1", len(updates))
	}
	a := updates[0].Agent
	if a.ID != "omp:ses-child" || a.NativeID != "ses-child" || a.Provider != Name {
		t.Fatalf("identity = %+v", a)
	}
	if a.Name != "scout" || a.Nickname != "ChessNewsScout" {
		t.Fatalf("agent naming = %+v", a)
	}
	if a.Title != "Fetch today's chess news." || !strings.HasPrefix(a.Prompt, "Fetch today's chess news.") {
		t.Fatalf("title/prompt = %q / %q", a.Title, a.Prompt)
	}
	if a.Model != "claude-sonnet-5:high" || a.Effort != "high" || a.Cwd != "/work/tree" {
		t.Fatalf("model/effort/cwd = %+v", a)
	}
	if a.Depth != 1 || a.Parent != "ses-parent" {
		t.Fatalf("nesting = depth %d parent %q", a.Depth, a.Parent)
	}
	if a.Status != model.StatusLive {
		t.Fatalf("status = %q", a.Status)
	}
}

func TestPollIgnoresParentSessionsAndNonTranscripts(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	if err := os.WriteFile(filepath.Join(dir, "Scout.md"), []byte("full output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "2.read.log"), []byte("URL: https://example.com"), 0o644); err != nil {
		t.Fatal(err)
	}

	p := New(root, 0, time.Now().Add(-time.Minute))
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 0 {
		t.Fatalf("adopted non-subagent files: %+v", updates)
	}
}

func TestPollStreamsEventsAndTokens(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	path := subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}

	appendLines(t, path,
		`{"type":"message","id":"u1","timestamp":"2026-08-12T03:31:30.000Z","message":{"role":"user","content":[{"type":"text","text":"Complete assignment thoroughly."}]}}`,
		assistant("2026-08-12T03:31:31.000Z", `{"type":"thinking","thinking":""},{"type":"thinking","thinking":"Reading the parser first."},{"type":"text","text":"Starting the audit."},{"type":"toolCall","id":"c1","name":"read","arguments":{"filePath":"/work/tree/parser.go"},"intent":"Read the parser"}`),
		toolResult("2026-08-12T03:31:32.000Z", "read", "1: package parser\n2: ...", false),
	)
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %d", len(updates))
	}
	want := []model.EventKind{model.EvUser, model.EvReasoning, model.EvText, model.EvToolUse, model.EvToolResult}
	if got := kinds(updates[0].Events); !reflect.DeepEqual(got, want) {
		t.Fatalf("event kinds = %v, want %v (empty thinking must be dropped)", got, want)
	}
	a := updates[0].Agent
	if a.ToolCount != 1 || a.Tokens.In != 150 || a.Tokens.Out != 200 || a.Tokens.CacheRead != 9000 || a.Tokens.CacheWrite != 500 {
		t.Fatalf("counts/tokens = %+v", a)
	}
	if a.Tokens.Total != 9350 {
		t.Fatalf("token total = %d, want in+out+cacheRead", a.Tokens.Total)
	}
	use := updates[0].Events[3]
	if use.Tool != "read" || !strings.Contains(use.Header, "Read the parser") || !strings.Contains(use.Body, "parser.go") {
		t.Fatalf("tool use = %+v", use)
	}
	result := updates[0].Events[4]
	if result.Tool != "read" || result.Err || result.Lines != 2 {
		t.Fatalf("tool result = %+v", result)
	}
}

func TestYieldCompletesTheAgent(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	path := subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	appendLines(t, path, toolResult("2026-08-12T03:33:34.000Z", "yield", "Result submitted.", false))

	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Agent.Status != model.StatusDone {
		t.Fatalf("yield did not complete the agent: %+v", updates)
	}
	if got := kinds(updates[0].Events); !reflect.DeepEqual(got, []model.EventKind{model.EvToolResult, model.EvStatus}) {
		t.Fatalf("yield events = %v", got)
	}
	if updates[0].Agent.DurationMS <= 0 {
		t.Fatalf("duration = %d", updates[0].Agent.DurationMS)
	}
	if again, err := p.Poll(); err != nil || len(again) != 0 {
		t.Fatalf("repeat poll = %+v, %v", again, err)
	}
}

func TestParentHubJobDecidesHowAnAgentEnded(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	appendLines(t, dir+".jsonl",
		`{"type":"message","id":"h1","timestamp":"2026-08-12T03:33:34.000Z","message":{"role":"toolResult","toolName":"hub","content":[{"type":"text","text":"## Failed (1)"}],"details":{"op":"wait","jobs":[{"id":"Scout","type":"task","status":"failed","label":"Audit the parser","durationMs":124891}]}}}`,
	)
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %+v", updates)
	}
	a := updates[0].Agent
	if a.Status != model.StatusError || a.DurationMS != 124891 || a.Title != "Audit the parser" {
		t.Fatalf("settled agent = %+v", a)
	}
}

func TestRunningHubJobLeavesTheAgentLive(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	appendLines(t, dir+".jsonl",
		`{"type":"message","id":"h1","timestamp":"2026-08-12T03:32:00.000Z","message":{"role":"toolResult","toolName":"hub","content":[{"type":"text","text":"## Running (1)"}],"details":{"op":"jobs","jobs":[{"id":"Scout","type":"task","status":"running","label":"Audit the parser"}]}}}`,
	)
	if updates, err := p.Poll(); err != nil {
		t.Fatal(err)
	} else {
		for _, u := range updates {
			if u.Agent.Status != model.StatusLive {
				t.Fatalf("running job settled the agent: %+v", u.Agent)
			}
		}
	}
}

func TestAsyncResultInjectionSettlesTheAgent(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	appendLines(t, dir+".jsonl",
		`{"type":"custom_message","customType":"async-result","display":true,"timestamp":"2026-08-12T03:33:34.000Z","content":"<system-notice>\nBackground job has completed.\n<task-result id=\"Scout\" agent=\"scout\" status=\"completed\" duration=\"2m4s\">\n<meta lines=\"38\" />\n"}`,
	)
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Agent.Status != model.StatusDone {
		t.Fatalf("async-result did not settle the agent: %+v", updates)
	}
	if updates[0].Agent.DurationMS != 124000 {
		t.Fatalf("duration from %q = %d, want 124000", "2m4s", updates[0].Agent.DurationMS)
	}
}

func TestSignalExitMarksTheAgentErrored(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	path := subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	appendLines(t, path,
		`{"type":"custom","customType":"session_exit","id":"x1","timestamp":"2026-08-12T03:34:00.000Z","data":{"reason":"sigterm","kind":"signal"}}`,
	)
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Agent.Status != model.StatusError {
		t.Fatalf("signal exit = %+v", updates)
	}
	if got := kinds(updates[0].Events); !reflect.DeepEqual(got, []model.EventKind{model.EvNotice, model.EvStatus}) {
		t.Fatalf("signal exit events = %v", got)
	}
}

func TestNormalDisposeAfterYieldDoesNotError(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	path := subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	appendLines(t, path,
		toolResult("2026-08-12T03:33:34.000Z", "yield", "Result submitted.", false),
		`{"type":"custom","customType":"session_exit","id":"x1","timestamp":"2026-08-12T03:40:34.000Z","data":{"reason":"dispose","kind":"normal"}}`,
	)
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	for _, at := range p.agents {
		if at.agent.Status != model.StatusDone {
			t.Fatalf("normal dispose changed status to %q", at.agent.Status)
		}
	}
}

func TestNestedSubagentsRecordTheirParentAgent(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	subagent(t, dir, "Lead", "ses-lead", "task", "Coordinate the audit.")

	nested := filepath.Join(dir, "Lead")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	subagent(t, nested, "Helper", "ses-helper", "scout", "Read one file.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	got := agentsByNickname(updates)
	if len(got) != 2 {
		t.Fatalf("adopted %d agents, want lead and nested helper", len(got))
	}
	if lead := got["Lead"].Agent; lead.Depth != 1 || lead.Parent != "ses-parent" {
		t.Fatalf("lead nesting = depth %d parent %q", lead.Depth, lead.Parent)
	}
	helper := got["Helper"].Agent
	if helper.Depth != 2 || helper.Parent != "ses-lead" {
		t.Fatalf("nested nesting = depth %d parent %q, want depth 2 under the lead agent", helper.Depth, helper.Parent)
	}
}

func TestPreexistingTranscriptsAreBacklogWithSearchableMetadata(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	path := subagent(t, dir, "Scout", "ses-child", "scout", "Historical audit.",
		assistant("2026-08-12T03:31:31.000Z", `{"type":"text","text":"old output"}`))

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	p := New(root, 0, time.Now())
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 {
		t.Fatalf("updates = %+v", updates)
	}
	a := updates[0].Agent
	if !a.Backlog || a.Status != model.StatusDone || len(updates[0].Events) != 0 {
		t.Fatalf("backlog agent = %+v events=%d", a, len(updates[0].Events))
	}
	if a.Prompt != "Historical audit." || a.Name != "scout" || a.Model != "claude-sonnet-5:high" {
		t.Fatalf("backlog metadata not sniffed: %+v", a)
	}
}

func TestIdleFallbackCompletesASilentAgent(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 40*time.Millisecond, time.Now().Add(-time.Minute))
	first, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Agent.Status != model.StatusLive {
		t.Fatalf("agent settled before the idle timeout: %+v", first)
	}
	time.Sleep(60 * time.Millisecond)
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Agent.Status != model.StatusDone {
		t.Fatalf("idle fallback = %+v", updates)
	}
}

func TestMalformedLineBecomesANotice(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	path := subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.")

	p := New(root, 0, time.Now().Add(-time.Minute))
	if _, err := p.Poll(); err != nil {
		t.Fatal(err)
	}
	appendLines(t, path, `{"type":"message","message":{`)
	updates, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || updates[0].Events[0].Kind != model.EvNotice || !updates[0].Events[0].Err {
		t.Fatalf("malformed line = %+v", updates)
	}
}

func TestReplayRebuildsTheWholeTranscript(t *testing.T) {
	root := t.TempDir()
	dir := sessionDir(t, root, "home-work-abc", "ses-parent")
	path := subagent(t, dir, "Scout", "ses-child", "scout", "Audit the parser.",
		`{"type":"message","id":"u1","timestamp":"2026-08-12T03:31:30.000Z","message":{"role":"user","content":[{"type":"text","text":"Complete assignment."}]}}`,
		assistant("2026-08-12T03:31:31.000Z", `{"type":"thinking","thinking":"Planning."},{"type":"toolCall","id":"c1","name":"grep","arguments":{"pattern":"TODO"}}`),
		toolResult("2026-08-12T03:31:32.000Z", "grep", "3 matches", false),
		toolResult("2026-08-12T03:33:34.000Z", "yield", "Result submitted.", false),
	)

	a, events, err := Replay(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != "omp:ses-child" || a.Name != "scout" || a.Nickname != "Scout" || a.Depth != 1 || a.Parent != "ses-parent" {
		t.Fatalf("replayed agent = %+v", a)
	}
	want := []model.EventKind{model.EvUser, model.EvReasoning, model.EvToolUse, model.EvToolResult, model.EvToolResult}
	if got := kinds(events); !reflect.DeepEqual(got, want) {
		t.Fatalf("replayed kinds = %v, want %v", got, want)
	}
	if a.EventCount != len(events) {
		t.Fatalf("event count = %d, events = %d", a.EventCount, len(events))
	}
	for i, e := range events {
		if e.Seq != uint64(i+1) || e.AgentID != a.ID {
			t.Fatalf("event %d = %+v", i, e)
		}
	}
	if _, _, err := Replay(filepath.Join(root, "missing.jsonl")); err == nil {
		t.Fatal("replaying a missing transcript succeeded")
	}
}

func TestParseDurationReadsCompactJobDurations(t *testing.T) {
	for in, want := range map[string]int64{
		"2m4s": 124000, "45s": 45000, "1h2m3s": 3723000, "": 0, "12": 0,
	} {
		if got := parseDuration(in); got != want {
			t.Errorf("parseDuration(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestTaskTitleSkipsThePreambleAndBareHeadings(t *testing.T) {
	cases := map[string]string{
		"Complete assignment thoroughly:\n\nRun the test suite and report failures.":              "Run the test suite and report failures.",
		"Complete assignment thoroughly:\n\n# Target\nhttps://example.com/news — the index page.": "Target — https://example.com/news — the index page.",
		"# Audit the parser for allocation churn in the hot loop\n\nDetails follow.":              "Audit the parser for allocation churn in the hot loop",
		"":                                "",
		"Complete assignment thoroughly:": "",
	}
	for task, want := range cases {
		if got := taskTitle(task); got != want {
			t.Errorf("taskTitle(%q) = %q, want %q", task, got, want)
		}
	}
}
