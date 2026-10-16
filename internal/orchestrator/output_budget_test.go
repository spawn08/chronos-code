package orchestrator

import (
	"context"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
)

func TestUnaryOutputAllowancePrecedesContextReservation(t *testing.T) {
	req := &model.ChatRequest{Model: "fixture-window", Messages: []model.Message{{Role: model.RoleUser, Content: "inspect"}}}
	evt := &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}
	if err := (outputTokensHook{unaryTokens: 4096}).Before(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	guard := newContextGuardHook(req.Model, 0, contextGuardOptions{ContextLimit: 8192, MaxOutputTokens: 4096})
	if err := guard.Before(context.Background(), evt); err != nil {
		t.Fatal(err)
	}
	if req.MaxTokens != 4096 {
		t.Fatalf("unary allowance=%d", req.MaxTokens)
	}
	budget, ok := evt.Metadata[guardBudgetMetadataKey].(map[string]any)
	if !ok || budget["output_reserve_tokens"] != 4096 || budget["message_budget_tokens"] != 4096 {
		t.Fatalf("incoherent request budget: %v", budget)
	}
}
