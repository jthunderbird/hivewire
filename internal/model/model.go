// Package model defines the provider-neutral types that flow through hivewire:
// an Agent (one subagent run) and the Events it emits.
package model

import "time"

// Status is the lifecycle state of an agent, and drives title-bar color.
type Status string

const (
	StatusLive  Status = "live"  // green
	StatusDone  Status = "done"  // gray
	StatusError Status = "error" // red
)

// Tokens is the running token accounting for an agent.
type Tokens struct {
	In            int `json:"in"`
	Out           int `json:"out"`
	CacheRead     int `json:"cache_read"`
	CacheWrite    int `json:"cache_write"`
	Reasoning     int `json:"reasoning"`
	Total         int `json:"total"`
	ContextWindow int `json:"context_window"`
}

// Agent is one subagent run, normalized across providers.
type Agent struct {
	ID       string `json:"id"`       // globally unique: "<provider>:<native id>"
	NativeID string `json:"nativeId"` // provider-local id
	Provider string `json:"provider"` // "claude" | "codex"
	Model    string `json:"model"`

	Name     string `json:"name"`     // claude: agentType; codex: basename(agent_path)
	Nickname string `json:"nickname"` // codex only
	Title    string `json:"title"`    // claude: Task description; codex: derived
	Prompt   string `json:"prompt"`   // the task the agent was given, for search
	Depth    int    `json:"depth"`
	Parent   string `json:"parent"`

	Cwd         string `json:"cwd"`
	GitBranch   string `json:"gitBranch"`
	Sandbox     string `json:"sandbox"`
	Approval    string `json:"approval"`
	Effort      string `json:"effort"`
	SessionKind string `json:"sessionKind"`
	CLIVersion  string `json:"cliVersion"`

	Source string `json:"source"` // transcript path on disk

	Started time.Time `json:"started"`
	Updated time.Time `json:"updated"`
	Status  Status    `json:"status"`

	// Backlog marks a transcript that already existed when hivewire started.
	// It is indexed for the history browser but never occupies a live slot and
	// its contents are not streamed, so launching hivewire does not replay
	// every subagent ever run.
	Backlog bool `json:"backlog,omitempty"`

	Tokens     Tokens `json:"tokens"`
	ToolCount  int    `json:"toolCount"`
	EventCount int    `json:"eventCount"`
	Bytes      int64  `json:"bytes"`   // bytes currently held in the ring buffer
	Dropped    int    `json:"dropped"` // events evicted by the ring buffer
	DurationMS int64  `json:"durationMs"`
}

// Label is the short human name for a pane title bar.
func (a Agent) Label() string {
	if a.Nickname != "" && a.Name != "" {
		return a.Name + " (" + a.Nickname + ")"
	}
	if a.Name != "" {
		return a.Name
	}
	return a.NativeID
}

// EventKind classifies a stream entry.
type EventKind string

const (
	EvText       EventKind = "text"        // assistant prose
	EvReasoning  EventKind = "reasoning"   // thinking / reasoning summary
	EvToolUse    EventKind = "tool_use"    // tool invocation
	EvToolResult EventKind = "tool_result" // tool output
	EvUser       EventKind = "user"        // prompt / injected message
	EvStatus     EventKind = "status"      // lifecycle transition
	EvNotice     EventKind = "notice"      // hivewire's own warnings (buffer wrap, parse error)
)

// Overflow points at output the *agent harness itself* truncated before it ever
// reached the transcript. The full bytes are still on disk at Path, so the
// viewer can fetch them on demand.
type Overflow struct {
	Path string `json:"path"`
	Note string `json:"note"`
}

// Event is one entry in an agent's stream.
type Event struct {
	Seq      uint64    `json:"seq"`
	AgentID  string    `json:"agentId"`
	TS       time.Time `json:"ts"`
	Kind     EventKind `json:"kind"`
	Tool     string    `json:"tool,omitempty"`
	Header   string    `json:"header"`         // always-visible one-liner
	Body     string    `json:"body,omitempty"` // full text, never truncated by hivewire
	Lines    int       `json:"lines"`
	Err      bool      `json:"err,omitempty"`
	Overflow *Overflow `json:"overflow,omitempty"`
}

// Size approximates the memory cost of an event for ring-buffer accounting.
func (e Event) Size() int64 {
	return int64(len(e.Header) + len(e.Body) + 64)
}
