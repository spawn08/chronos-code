package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
)

func TestScopedInstructionsNeededClassification(t *testing.T) {
	for _, value := range []any{errScopedInstructionsNeeded, fmt.Errorf("hook: %w: pending", errScopedInstructionsNeeded), "hook before tool file_read: scoped_instructions_needed: file access is deferred."} {
		if !IsScopedInstructionsNeeded(value) {
			t.Fatalf("missed deferral %#v", value)
		}
	}
	for _, value := range []any{nil, "permission denied", errors.New("provider unavailable"), "discussion of scoped_instructions_needed", map[string]string{"error": "unknown"}} {
		if IsScopedInstructionsNeeded(value) {
			t.Fatalf("ordinary error classified as deferral %#v", value)
		}
	}
}

type scopedReadRecoveryProvider struct {
	guardTestProvider
	t     *testing.T
	calls int
}

func (p *scopedReadRecoveryProvider) Name() string { return "scoped-read-fixture" }

func (p *scopedReadRecoveryProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.calls++
	if p.calls == 2 {
		instructions, deferral := false, false
		for _, message := range req.Messages {
			instructions = instructions || (message.Name == "harness-scoped-rules" && strings.Contains(message.Content, "READ_RULE"))
			deferral = deferral || (message.Role == model.RoleTool && strings.Contains(message.Content, "scoped_instructions_needed"))
		}
		if !instructions || !deferral {
			p.t.Fatal("retry model call lacked loaded instructions or the preserved deferral")
		}
	}
	if p.calls <= 2 {
		return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: fmt.Sprintf("read-%d", p.calls), Name: "file_read", Arguments: `{"path":"src/CLAUDE.md"}`}}}, nil
	}
	return &model.ChatResponse{Content: "done", StopReason: model.StopReasonEnd}, nil
}

func TestScopedInstructionsReadRecoversWithoutUserQuestion(t *testing.T) {
	root := t.TempDir()
	scopedRulesFile(t, root, "src/CLAUDE.md", "READ_RULE")
	provider := &scopedReadRecoveryProvider{guardTestProvider: guardTestProvider{id: "gpt-4o"}, t: t}
	a := newExecutionTestAgent("reader", provider)
	a.Storage = pipelineAgent(t).Storage
	read := builtins.NewFileReadTool(root)
	read.Permission = tool.PermAllow
	original := read.Handler
	reads := 0
	read.Handler = func(ctx context.Context, args map[string]any) (any, error) { reads++; return original(ctx, args) }
	a.Tools.Register(read)
	a.Hooks = append(a.Hooks, &scopedRulesHook{root: root})
	if _, err := a.Chat(scopedRulesContext(root, "read-recovery"), "Read the nested project instructions."); err != nil {
		t.Fatal(err)
	}
	if provider.calls != 3 || reads != 1 {
		t.Fatalf("calls=%d reads=%d", provider.calls, reads)
	}
}
