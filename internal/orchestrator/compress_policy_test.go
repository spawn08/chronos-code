package orchestrator

import "testing"

func TestNeverCompressTool(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want bool
	}{
		{"spawn_subagent", nil, true},
		{skillToolName, nil, true},
		{"await_shell", nil, true},
		{"file_read", map[string]any{"path": "a.go", "start_line": 10.0}, true},
		{"file_read", map[string]any{"path": "a.go"}, false},
		{"shell", map[string]any{"command": "ls"}, false},
	}
	for _, tt := range tests {
		if got := neverCompressTool(tt.name, tt.args); got != tt.want {
			t.Errorf("neverCompressTool(%q, %v) = %v, want %v", tt.name, tt.args, got, tt.want)
		}
	}
}
