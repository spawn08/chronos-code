package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
)

func BenchmarkContextPreflight(b *testing.B) {
	h := newContextGuardHook("gpt-4o", 0)
	messages := []model.Message{{Role: model.RoleSystem, Content: strings.Repeat("Follow project conventions.\n", 200)}}
	for i := 0; i < 40; i++ {
		messages = append(messages, model.Message{Role: model.RoleTool, ToolCallID: fmt.Sprint(i),
			Content: fmt.Sprintf("file %d\n%s", i, strings.Repeat("func example() { return value }\n", 100))})
	}
	messages = append(messages, model.Message{Role: model.RoleUser, Content: "Explain these files."})
	b.ReportAllocs()
	for b.Loop() {
		event := &hooks.Event{Type: hooks.EventModelCallBefore, Input: &model.ChatRequest{
			Model: "gpt-4o", Messages: messages, MaxTokens: 1024,
		}}
		if err := h.Before(context.Background(), event); err != nil {
			b.Fatal(err)
		}
	}
}
