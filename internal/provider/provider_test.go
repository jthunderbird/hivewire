package provider

import "testing"

func TestToolHeaderMatchesNamesFromEveryCLI(t *testing.T) {
	tests := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{"claude bash", "Bash", map[string]any{"command": "go test ./..."}, "Bash  go test ./..."},
		{"opencode bash", "bash", map[string]any{"command": "go test ./...", "description": "run tests"}, "bash  go test ./..."},
		{"claude read", "Read", map[string]any{"file_path": "/work/main.go"}, "Read  /work/main.go"},
		{"opencode read", "read", map[string]any{"filePath": "/work/main.go"}, "read  /work/main.go"},
		{"opencode edit", "edit", map[string]any{"filePath": "/work/main.go", "oldString": "func main"}, "edit  /work/main.go  ← func main"},
		{"opencode grep", "grep", map[string]any{"pattern": "TODO", "path": "internal"}, "grep  TODO  in internal"},
		{"opencode skill", "skill", map[string]any{"name": "caveman"}, "skill  caveman"},
		{"opencode task", "task", map[string]any{"description": "Review the parser"}, "task  Review the parser"},
		{"unknown tool falls back to JSON", "mystery", map[string]any{"a": "b"}, `mystery  {"a":"b"}`},
		{"no usable detail keeps the name", "bash", map[string]any{}, "bash"},
	}
	for _, tt := range tests {
		if got := ToolHeader(tt.tool, tt.input); got != tt.want {
			t.Errorf("%s: ToolHeader(%q) = %q, want %q", tt.name, tt.tool, got, tt.want)
		}
	}
}
