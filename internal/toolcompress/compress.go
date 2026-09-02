// Package toolcompress implements PRD P1-006: tool result compression. Any
// tool result whose JSON encoding exceeds a token budget is evicted to
// storage and replaced in the conversation history with a short preview plus
// a reference key, retrievable via the read_stored_result tool. This keeps
// the median tool result in context small without discarding information the
// agent might need later.
package toolcompress

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

// DefaultThresholdTokens is the token budget above which a tool result is
// compressed (PRD P1-006 default).
const DefaultThresholdTokens = 500

// ReadStoredResultTool is the name of the tool registered by Wrap to retrieve
// a previously evicted result.
const ReadStoredResultTool = "read_stored_result"

const (
	defaultStoredResultChunkBytes = 8 << 10
	maxStoredResultChunkBytes     = 16 << 10
)

// Wrap wraps every handler currently registered on a so results exceeding
// thresholdTokens (0 uses DefaultThresholdTokens) are evicted to a.Storage
// and replaced with a compact preview, and registers ReadStoredResultTool so
// the agent can pull the full result back on demand. a.Storage must be set
// before calling Wrap; if it is nil, Wrap is a no-op (compression requires
// somewhere to put the evicted data).
func Wrap(a *agent.Agent, thresholdTokens int) {
	if thresholdTokens <= 0 {
		thresholdTokens = DefaultThresholdTokens
	}
	WrapDynamic(a, func(context.Context) int { return thresholdTokens })
}

// WrapDynamic is Wrap with a per-call threshold: thresholdFn is invoked on
// every tool result (with the call's context) to decide the compression
// threshold for that call, letting a caller ramp compression up as a
// session's budget usage grows (PRD P2-009's progressive compression ramp)
// instead of fixing the threshold for the agent's whole lifetime.
func WrapDynamic(a *agent.Agent, thresholdFn func(context.Context) int) {
	if a.Storage == nil {
		return
	}
	counter := model.NewTokenCounter(a.Model.Model())
	agentID := a.ID
	store := a.Storage

	for _, def := range a.Tools.List() {
		if def.Name == ReadStoredResultTool || def.Handler == nil {
			continue
		}
		orig := def.Handler
		name := def.Name
		def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			result, err := orig(ctx, args)
			if err != nil || result == nil {
				return result, err
			}
			thresholdTokens := thresholdFn(ctx)
			if thresholdTokens <= 0 {
				thresholdTokens = DefaultThresholdTokens
			}
			data, mErr := json.Marshal(result)
			if mErr != nil || counter.CountString(string(data)) <= thresholdTokens {
				return result, nil
			}
			sessionID := sessionOrAgent(ctx, agentID)
			evicted, evErr := agent.EvictLargeResult(ctx, store, sessionID, name, result)
			if evErr != nil || evicted == nil {
				return result, nil
			}
			return map[string]any{
				"compressed":      true,
				"preview":         evicted.Preview,
				"full_size_bytes": evicted.FullSize,
				"storage_key":     evicted.StorageKey,
			}, nil
		}
	}

	a.Tools.Register(&tool.Definition{
		Name:        ReadStoredResultTool,
		Description: "Retrieve a bounded chunk of a compressed tool result. Continue with next_offset only when more content is necessary.",
		Permission:  tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":       map[string]any{"type": "string", "description": "The storage_key returned alongside a compressed tool result"},
				"offset":    map[string]any{"type": "integer", "description": "Byte offset to start reading from (default 0)"},
				"max_bytes": map[string]any{"type": "integer", "description": "Maximum bytes to return (default 8192, capped at 16384)"},
			},
			"required": []string{"key"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			key, _ := args["key"].(string)
			if key == "" {
				return nil, fmt.Errorf("read_stored_result: key is required")
			}
			content, err := agent.ReadStoredResult(ctx, store, sessionOrAgent(ctx, agentID), key)
			if err != nil {
				return nil, err
			}
			offset := intArg(args["offset"])
			if offset < 0 || offset > len(content) {
				return nil, fmt.Errorf("read_stored_result: offset %d outside result of %d bytes", offset, len(content))
			}
			maxBytes := intArg(args["max_bytes"])
			if maxBytes <= 0 {
				maxBytes = defaultStoredResultChunkBytes
			}
			if maxBytes > maxStoredResultChunkBytes {
				maxBytes = maxStoredResultChunkBytes
			}
			end := offset + maxBytes
			if end > len(content) {
				end = len(content)
			}
			return map[string]any{
				"content":     content[offset:end],
				"offset":      offset,
				"next_offset": end,
				"total_bytes": len(content),
				"truncated":   end < len(content),
			}, nil
		},
	})
}

func intArg(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

func sessionOrAgent(ctx context.Context, agentID string) string {
	if id := storage.SessionFromContext(ctx); id != "" {
		return id
	}
	return agentID
}
