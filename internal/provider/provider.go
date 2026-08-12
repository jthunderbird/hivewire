// Package provider defines how a coding-agent CLI is adapted into hivewire's
// normalized stream, plus helpers shared by the adapters.
package provider

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/jtaylor/hivewire/internal/model"
)

// Update carries whatever a provider learned during one Poll: an agent upsert,
// some events, or both.
type Update struct {
	Agent  model.Agent
	Events []model.Event
}

// Provider adapts one agent CLI's on-disk transcripts. Poll is called on a
// ticker; it discovers new agents and drains newly appended transcript lines.
// Implementations must not block.
type Provider interface {
	Name() string
	Poll() ([]Update, error)
}

// overflowRe matches the marker Claude Code writes when it truncates a tool
// result before persisting it: the full bytes live at the captured path.
var overflowRe = regexp.MustCompile(`Full output saved to: (\S+)`)

// DetectOverflow reports whether the agent harness truncated this body itself,
// returning a pointer to the on-disk file holding the untruncated output.
func DetectOverflow(body string) *model.Overflow {
	m := overflowRe.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	note := "truncated by harness"
	if i := strings.Index(body, "Output too large ("); i >= 0 {
		if j := strings.Index(body[i:], ")"); j > 0 {
			note = "truncated by harness — " + body[i+len("Output too large ("):i+j]
		}
	}
	return &model.Overflow{Path: strings.TrimRight(m[1], ".,"), Note: note}
}

// CountLines returns the number of display lines in s.
func CountLines(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// FirstLine returns s up to its first newline, clipped to max runes.
func FirstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + " …"
	}
	return Clip(s, max)
}

// Clip shortens s to max runes, appending an ellipsis when it had to cut.
func Clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// ToolHeader builds the one-line summary shown for a tool invocation, using
// per-tool knowledge of which argument actually matters. Names are matched
// case-insensitively because the CLIs disagree on capitalization for what is
// otherwise the same tool: Claude Code writes "Bash", OpenCode writes "bash".
func ToolHeader(tool string, input map[string]any) string {
	str := func(k string) string {
		if v, ok := input[k].(string); ok {
			return v
		}
		return ""
	}
	var detail string
	switch strings.ToLower(tool) {
	case "bash", "bashoutput":
		detail = str("command")
		if d := str("description"); d != "" && detail == "" {
			detail = d
		}
	case "read", "write", "notebookedit":
		detail = str("file_path")
		if detail == "" {
			detail = str("filePath")
		}
	case "edit":
		detail = str("file_path")
		if detail == "" {
			detail = str("filePath")
		}
		if old := str("old_string"); old != "" {
			detail += "  ← " + FirstLine(old, 40)
		} else if old := str("oldString"); old != "" {
			detail += "  ← " + FirstLine(old, 40)
		}
	case "grep":
		detail = str("pattern")
		if p := str("path"); p != "" {
			detail += "  in " + p
		}
	case "glob":
		detail = str("pattern")
	case "agent", "task":
		detail = str("description")
	case "webfetch", "websearch", "codesearch":
		detail = str("url") + str("query")
	case "skill":
		detail = strings.TrimSpace(str("skill") + str("name") + " " + str("args"))
	case "apply_patch":
		detail = FirstLine(str("patchText"), 120)
	default:
		if b, err := json.Marshal(input); err == nil {
			detail = string(b)
		}
	}
	detail = FirstLine(detail, 160)
	if detail == "" {
		return tool
	}
	return fmt.Sprintf("%s  %s", tool, detail)
}

// PrettyJSON renders v indented, falling back to a compact form on error.
func PrettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
