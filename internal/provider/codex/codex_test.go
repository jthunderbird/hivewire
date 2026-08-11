package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jtaylor/hivewire/internal/model"
	"github.com/jtaylor/hivewire/internal/provider"
)

const (
	metaSubagent = `{"timestamp":"2026-08-11T15:53:41.346Z","type":"session_meta","payload":{"id":"019ff187-90dc","parent_thread_id":"019ff187-7aad","timestamp":"2026-08-11T15:53:41.346Z","cwd":"/home/user/proj","cli_version":"0.147.0","thread_source":"subagent","agent_nickname":"Carson","agent_path":"/root/count_markdown","model_provider":"openai","source":{"subagent":{"thread_spawn":{"parent_thread_id":"019ff187-7aad","depth":1,"agent_path":"/root/count_markdown","agent_nickname":"Carson"}}},"base_instructions":{"text":"long static prompt"}}}`
	metaUser     = `{"timestamp":"2026-08-11T15:53:35.665Z","type":"session_meta","payload":{"id":"019ff187-7aad","timestamp":"2026-08-11T15:53:35.665Z","cwd":"/home/user/proj","source":"exec","thread_source":"user"}}`

	turnContext  = `{"timestamp":"2026-08-11T15:53:42.000Z","type":"turn_context","payload":{"model":"gpt-5.6-sol","approval_policy":"never","sandbox_policy":{"type":"read-only"},"effort":"low"}}`
	userMessage  = `{"timestamp":"2026-08-11T15:53:42.100Z","type":"event_msg","payload":{"type":"user_message","message":"count the markdown files"}}`
	toolCall     = `{"timestamp":"2026-08-11T15:53:43.000Z","type":"response_item","payload":{"type":"custom_tool_call","name":"exec","call_id":"c1","input":"tools.exec_command({cmd:\"rg --files docs\"})"}}`
	toolOutput   = `{"timestamp":"2026-08-11T15:53:44.000Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"c1","output":[{"type":"input_text","text":"Script completed\n"},{"type":"input_text","text":"10\n"}]}}`
	agentMessage = `{"timestamp":"2026-08-11T15:53:45.000Z","type":"event_msg","payload":{"type":"agent_message","message":"10 markdown files","phase":"final_answer"}}`
	devMessage   = `{"timestamp":"2026-08-11T15:53:42.050Z","type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"multi-KB skills instructions"}]}}`
	encReasoning = `{"timestamp":"2026-08-11T15:53:42.500Z","type":"response_item","payload":{"type":"reasoning","summary":[],"encrypted_content":"gAAAAA..."}}`
	tokenCount   = `{"timestamp":"2026-08-11T15:53:45.100Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":16276,"cached_input_tokens":11008,"output_tokens":78,"reasoning_output_tokens":11,"total_tokens":16354},"model_context_window":258400}}}`
	taskComplete = `{"timestamp":"2026-08-11T15:53:46.000Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"t1","duration_ms":5972,"last_agent_message":"10"}}`
)

func rollout(t *testing.T, root, name string, lines ...string) string {
	t.Helper()
	dir := filepath.Join(root, "2026", "08", "11")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func poll(t *testing.T, p *Provider) []provider.Update {
	t.Helper()
	u, err := p.Poll()
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestOnlySubagentThreadsAreAdopted(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "rollout-user.jsonl", metaUser, agentMessage)
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, agentMessage)

	updates := poll(t, New(root, time.Minute, time.Now().Add(-time.Hour)))
	if len(updates) != 1 {
		t.Fatalf("expected only the subagent thread, got %d", len(updates))
	}
	if updates[0].Agent.NativeID != "019ff187-90dc" {
		t.Fatalf("adopted the wrong thread: %+v", updates[0].Agent)
	}
}

func TestSubagentMetadataComesFromSessionMeta(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, turnContext, userMessage)

	a := poll(t, New(root, time.Minute, time.Now().Add(-time.Hour)))[0].Agent
	if a.Name != "count_markdown" || a.Nickname != "Carson" {
		t.Errorf("name/nickname = %q/%q", a.Name, a.Nickname)
	}
	if a.Depth != 1 || a.Parent != "019ff187-7aad" {
		t.Errorf("spawn linkage = depth %d parent %q", a.Depth, a.Parent)
	}
	if a.Model != "gpt-5.6-sol" || a.Sandbox != "read-only" || a.Approval != "never" || a.Effort != "low" {
		t.Errorf("turn_context not applied: %+v", a)
	}
	if a.Cwd != "/home/user/proj" || a.CLIVersion != "0.147.0" {
		t.Errorf("session metadata not applied: %+v", a)
	}
}

func TestToolCallsAndOutputBecomeEvents(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, userMessage, toolCall, toolOutput, agentMessage)

	u := poll(t, New(root, time.Minute, time.Now().Add(-time.Hour)))[0]
	kinds := map[model.EventKind]int{}
	for _, e := range u.Events {
		kinds[e.Kind]++
	}
	if kinds[model.EvToolUse] != 1 || kinds[model.EvToolResult] != 1 || kinds[model.EvText] != 1 || kinds[model.EvUser] != 1 {
		t.Fatalf("event kinds = %v", kinds)
	}
	if u.Agent.ToolCount != 1 {
		t.Errorf("ToolCount = %d", u.Agent.ToolCount)
	}
	for _, e := range u.Events {
		if e.Kind == model.EvToolResult && !strings.Contains(e.Body, "10") {
			t.Errorf("tool output text not concatenated: %q", e.Body)
		}
	}
}

func TestStaticInstructionsAndEncryptedReasoningAreSkipped(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, devMessage, encReasoning, agentMessage)

	u := poll(t, New(root, time.Minute, time.Now().Add(-time.Hour)))[0]
	if len(u.Events) != 1 {
		t.Fatalf("expected only the agent message, got %d events", len(u.Events))
	}
	if u.Events[0].Kind != model.EvText {
		t.Fatalf("kept the wrong event: %+v", u.Events[0])
	}
}

func TestRoutingEnvelopeIsNotUsedAsPromptOrTitle(t *testing.T) {
	// Codex encrypts the task itself; only this dispatch header is plaintext,
	// and it merely repeats the agent name.
	envelope := `{"timestamp":"2026-08-11T15:53:42.000Z","type":"response_item","payload":{"type":"agent_message","author":"/root","recipient":"/root/count_markdown","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: /root/count_markdown\nSender: /root\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"gAAA"}]}}`
	root := t.TempDir()
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, envelope)

	u := poll(t, New(root, time.Minute, time.Now().Add(-time.Hour)))[0]
	if strings.Contains(u.Agent.Prompt, "NEW_TASK") {
		t.Errorf("routing envelope leaked into the searchable prompt: %q", u.Agent.Prompt)
	}
	if strings.Contains(u.Agent.Title, "NEW_TASK") {
		t.Errorf("routing envelope leaked into the title: %q", u.Agent.Title)
	}
	// It is still worth showing in the stream.
	if len(u.Events) != 1 {
		t.Fatalf("the envelope should still appear as an event, got %d", len(u.Events))
	}
}

func TestRealUserMessageBecomesThePrompt(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, userMessage)

	u := poll(t, New(root, time.Minute, time.Now().Add(-time.Hour)))[0]
	if u.Agent.Prompt != "count the markdown files" {
		t.Fatalf("Prompt = %q", u.Agent.Prompt)
	}
}

func TestBacklogRolloutsAreSniffedForSearchMetadata(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, turnContext, userMessage, toolCall, toolOutput)

	u := poll(t, New(root, time.Minute, time.Now().Add(time.Hour)))[0]
	if !u.Agent.Backlog {
		t.Fatal("expected a backlog agent")
	}
	if u.Agent.Prompt == "" || u.Agent.Model == "" {
		t.Fatalf("backlog agents must still be searchable: prompt=%q model=%q", u.Agent.Prompt, u.Agent.Model)
	}
	if len(u.Events) != 0 {
		t.Errorf("sniffing must not emit events, got %d", len(u.Events))
	}
	if u.Agent.ToolCount != 0 {
		t.Errorf("sniffed tool counts are partial and should be cleared, got %d", u.Agent.ToolCount)
	}
}

func TestTaskCompleteInTheSameBatchStillFinishes(t *testing.T) {
	// Regression: the "woke up again" check used to run after parsing, so a
	// batch containing task_complete flipped the agent straight back to live.
	root := t.TempDir()
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, turnContext, toolCall, toolOutput, tokenCount, agentMessage, taskComplete)

	a := poll(t, New(root, time.Minute, time.Now().Add(-time.Hour)))[0].Agent
	if a.Status != model.StatusDone {
		t.Fatalf("status = %q, want done", a.Status)
	}
	if a.DurationMS != 5972 {
		t.Errorf("DurationMS = %d, want 5972", a.DurationMS)
	}
	if a.Tokens.Total != 16354 || a.Tokens.ContextWindow != 258400 {
		t.Errorf("tokens = %+v", a.Tokens)
	}
}

func TestActivityAfterCompletionReopensTheAgent(t *testing.T) {
	root := t.TempDir()
	path := rollout(t, root, "rollout-sub.jsonl", metaSubagent, agentMessage, taskComplete)
	p := New(root, time.Minute, time.Now().Add(-time.Hour))
	if a := poll(t, p)[0].Agent; a.Status != model.StatusDone {
		t.Fatalf("status = %q, want done", a.Status)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(userMessage + "\n")
	f.Close()

	if a := poll(t, p)[0].Agent; a.Status != model.StatusLive {
		t.Fatalf("a second turn should reopen the agent, status = %q", a.Status)
	}
}

func TestPreexistingRolloutsAreBacklogNotLive(t *testing.T) {
	root := t.TempDir()
	rollout(t, root, "rollout-sub.jsonl", metaSubagent, toolCall, toolOutput)

	u := poll(t, New(root, time.Minute, time.Now().Add(time.Hour)))[0]
	if !u.Agent.Backlog {
		t.Error("pre-existing rollout should be marked backlog")
	}
	if len(u.Events) != 0 {
		t.Errorf("backlog rollouts must not be replayed, got %d events", len(u.Events))
	}
}

func TestReplayRebuildsTheStreamFromDisk(t *testing.T) {
	root := t.TempDir()
	path := rollout(t, root, "rollout-sub.jsonl", metaSubagent, turnContext, userMessage, toolCall, toolOutput, agentMessage, taskComplete)

	a, events, err := Replay(path)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != "count_markdown" || a.Nickname != "Carson" {
		t.Errorf("replayed agent = %+v", a)
	}
	if len(events) != 5 {
		t.Fatalf("replayed %d events, want 5", len(events))
	}
}
