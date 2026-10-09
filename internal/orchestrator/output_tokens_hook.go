package orchestrator

import (
	"context"
	"strings"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/execution"
)

// outputTokensHook raises the per-reply output allowance for streaming
// Anthropic calls that leave MaxTokens unset. The provider otherwise sends
// max_tokens=4096, which truncates a single large file_write mid tool call;
// the truncated call is discarded and never executes.
//
// Only streaming calls are raised: unary calls run under the provider's total
// request timeout, where a longer reply would become a timeout instead of a
// recoverable output-limit stop. Cost-capped deliveries keep their own finite
// reservation (see deliveryBudgetHook).
type outputTokensHook struct{}

func (outputTokensHook) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallBefore {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil || req.MaxTokens > 0 {
		return nil
	}
	if streaming, _ := evt.Metadata["stream"].(bool); !streaming {
		return nil
	}
	provider, ok := evt.Metadata["provider"].(model.Provider)
	if !ok || !anthropicMessagesProvider(provider) {
		return nil
	}
	if lease, ok := execution.OperationLeaseFromContext(ctx); ok && lease.Lease.Delivery.MaxCostMicrodollars > 0 {
		return nil
	}
	modelID := req.Model
	if modelID == "" {
		modelID = provider.Model()
	}
	req.MaxTokens = anthropicOutputTokens(modelID)
	return nil
}

func (outputTokensHook) After(context.Context, *hooks.Event) error { return nil }

// anthropicOutputTokens returns an output allowance every listed model family
// accepts, or 0 to keep the provider default. Claude 3.7 and 4.x accept at
// least 32000 output tokens; 3.5 accepts 8192; older 3.x models only 4096.
func anthropicOutputTokens(modelID string) int {
	id := strings.ToLower(modelID)
	switch {
	case !strings.Contains(id, "claude"):
		return 0
	case strings.Contains(id, "claude-3-5") || strings.Contains(id, "claude-3.5"):
		return 8192
	case strings.Contains(id, "claude-3-7") || strings.Contains(id, "claude-3.7"):
		return 32000
	case strings.Contains(id, "claude-3") || strings.Contains(id, "claude-2") || strings.Contains(id, "claude-instant"):
		return 0
	}
	return 32000
}
