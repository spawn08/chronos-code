package tui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/orchestrator"
)

func TestHeadlessToolCallsAreReadable(t *testing.T) {
	var out bytes.Buffer
	PrintResponse(&model.ChatResponse{ToolCalls: []model.ToolCall{
		{Name: "shell", Arguments: `{"command":"go test ./..."}`},
		{Name: "file_read", Arguments: `{"path":"internal/tui/app.go","start_line":10,"end_line":20}`},
		{Name: orchestrator.ExitPlanModeToolName, Arguments: `{"plan":"## Plan\n1. Fix parser"}`},
		{Name: "partial", Arguments: `{"path":`},
	}}, &out)
	got := ansi.Strip(out.String())
	for _, want := range []string{"> shell  go test ./...", "> file_read  internal/tui/app.go :10-20", "> exit_plan_mode  Plan", "1. Fix parser", "> partial  {\"path\":"} {
		if !strings.Contains(got, want) {
			t.Fatalf("headless output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `{"command"`) {
		t.Fatalf("headless output shows raw JSON:\n%s", got)
	}
}
