package tui

import (
	"reflect"
	"testing"
)

func TestCommandCompletions(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{name: "empty", input: "", want: nil},
		{name: "plain text", input: "help", want: nil},
		{name: "arguments", input: "/agent coder", want: nil},
		{name: "prefix orders shortest first", input: "/ag", want: []string{"/agent", "/agents"}},
		{name: "exact first", input: "/agent", want: []string{"/agent", "/agents"}},
		{name: "fuzzy subsequence", input: "/mdl", want: []string{"/model"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := commandCompletions(tt.input); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("commandCompletions(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}
