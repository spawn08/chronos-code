package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"
)

type requestMetricsHook struct{ store storage.Storage }

func (h requestMetricsHook) Before(_ context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallBefore {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil {
		return nil
	}
	tools, err := json.Marshal(req.Tools)
	if err != nil {
		return fmt.Errorf("encode request metrics: %w", err)
	}
	messages, err := json.Marshal(req.Messages)
	if err != nil {
		return fmt.Errorf("encode request metrics: %w", err)
	}
	if evt.Metadata == nil {
		evt.Metadata = map[string]any{}
	}
	metrics := map[string]any{"model": req.Model, "tools": len(req.Tools), "tool_schema_hash": fmt.Sprintf("%x", sha256.Sum256(tools)), "prompt_hash": fmt.Sprintf("%x", sha256.Sum256(messages)), "tool_schema_bytes": len(tools), "message_bytes": len(messages), "block_hashes": requestBlockHashes(req), "max_output_tokens": req.MaxTokens, "summary_request": summaryRequest(req)}
	var mutable, raw []model.Message
	for _, msg := range req.Messages {
		if msg.Uncached || msg.TurnScoped {
			mutable = append(mutable, msg)
		}
		if rawUserMessage(msg) {
			raw = append(raw, msg)
		}
	}
	if data, err := json.Marshal(mutable); err == nil {
		metrics["mutable_tail_hash"] = fmt.Sprintf("%x", sha256.Sum256(data))
		metrics["mutable_tail_bytes"] = len(data)
	}
	if data, err := json.Marshal(raw); err == nil {
		metrics["raw_user_hash"] = fmt.Sprintf("%x", sha256.Sum256(data))
	}
	if budget, ok := evt.Metadata[guardBudgetMetadataKey]; ok {
		metrics["effective_budget"] = budget
	}
	if trim, ok := evt.Metadata["chronos_code.context_guard_trim"]; ok {
		metrics["trim"] = trim
	}
	evt.Metadata["harness_request_metrics"] = metrics
	return nil
}

func (h requestMetricsHook) After(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallAfter || h.store == nil || storage.SessionFromContext(ctx) == "" {
		return nil
	}
	metrics, ok := evt.Metadata["harness_request_metrics"].(map[string]any)
	if !ok {
		return nil
	}
	ctx, cancel := durableObservationContext(ctx)
	defer cancel()
	now := time.Now().UTC()
	return h.store.InsertTrace(ctx, &storage.Trace{ID: "request-" + rand.Text(), SessionID: storage.SessionFromContext(ctx), Kind: "request_context", Name: "request_context", Output: metrics, StartedAt: now, EndedAt: now})
}

func requestBlockHashes(req *model.ChatRequest) map[string]string {
	blocks := map[string]string{}
	if req == nil {
		return blocks
	}
	digest := func(value any) string { data, _ := json.Marshal(value); return fmt.Sprintf("%x", sha256.Sum256(data)) }
	blocks["tool_schemas"] = digest(req.Tools)
	blocks["output_schema"] = digest(map[string]any{"format": req.ResponseFormat, "schema": req.Metadata["json_schema"]})
	counts := map[string]int{}
	for _, msg := range req.Messages {
		if msg.Role != model.RoleSystem || msg.TurnScoped || msg.Uncached {
			continue
		}
		name := msg.Name
		if name == "" {
			name = string(contextSourceFromMessage(msg))
		}
		if name == "" {
			name = "system"
		}
		counts[name]++
		blocks[fmt.Sprintf("standing/%s/%d", name, counts[name])] = digest(msg)
	}
	return blocks
}

func changedRequestBlocks(before, after map[string]string) []string {
	if before == nil {
		return nil
	}
	var changed []string
	for name, hash := range after {
		if before[name] != hash {
			changed = append(changed, name)
		}
	}
	for name := range before {
		if _, exists := after[name]; !exists {
			changed = append(changed, name)
		}
	}
	sort.Strings(changed)
	return changed
}
