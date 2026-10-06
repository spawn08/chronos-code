package orchestrator

import (
	"strings"
	"testing"

	"github.com/spawn08/chronos/sdk/agent"
)

func TestAppendSystemPromptAddsOperatorSectionAfterPersona(t *testing.T) {
	a := &agent.Agent{ID: "coder", SystemPrompt: "persona\nsecurity floor\n"}
	o := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, primary: "coder"}
	o.AppendSystemPrompt("  stop and ask on the issue  ")
	want := "persona\nsecurity floor\n\n" + operatorPromptHeader + "\n\nstop and ask on the issue"
	if a.SystemPrompt != want {
		t.Fatalf("SystemPrompt = %q, want %q", a.SystemPrompt, want)
	}
	o.AppendSystemPrompt("   ")
	if strings.Count(a.SystemPrompt, operatorPromptHeader) != 1 {
		t.Fatalf("blank text changed the prompt: %q", a.SystemPrompt)
	}
}
