// Package toolcompress implements PRD P1-006: tool result compression. Any
// tool result whose JSON encoding exceeds a token budget is evicted to
// storage and replaced in the conversation history with a short preview plus
// a reference key, retrievable via the read_stored_result tool. This keeps
// the median tool result in context small without discarding information the
// agent might need later.
package toolcompress

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"path/filepath"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/tokencache"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
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
	// These were 8KiB/16KiB, which meant reassembling one moderately large
	// stored result (e.g. ~80KB) cost 5+ separate read_stored_result calls —
	// each its own model round trip that resends the whole growing
	// transcript, the single biggest driver of cumulative token-budget
	// exhaustion observed in practice. Raised so the common case fits in
	// one or two calls instead.
	defaultStoredResultChunkBytes = 64 << 10
	maxStoredResultChunkBytes     = 256 << 10
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
	WrapDynamicForTool(a, func(ctx context.Context, _ string, _ map[string]any) int { return thresholdFn(ctx) })
}

// WrapDynamicForTool selects a compression threshold from the operation that
// produced this result, rather than another concurrently completed tool call.
func WrapDynamicForTool(a *agent.Agent, thresholdFn func(context.Context, string, map[string]any) int) {
	if a.Storage == nil {
		return
	}
	agentID := a.ID
	store := a.Storage
	var counters tokencache.Cache

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
			thresholdTokens := thresholdFn(ctx, name, args)
			if thresholdTokens <= 0 {
				thresholdTokens = DefaultThresholdTokens
			}
			data, mErr := json.Marshal(result)
			// Byte-level BPE cannot produce more tokens than input bytes. Tiny
			// results (especially write receipts) need no tokenizer or cache entry.
			if mErr != nil || len(data) <= thresholdTokens {
				return result, nil
			}
			counter := counters.ForModel(a.Model.Model())
			if counter.CountString(string(data)) <= thresholdTokens {
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
	RegisterReader(a)
}

// RegisterReader is idempotent, allowing hooks to wrap the retrieval tool before
// the output pipeline is installed. It never overwrites an existing handler.
func RegisterReader(a *agent.Agent) {
	if a.Storage == nil {
		return
	}
	if _, exists := a.Tools.Get(ReadStoredResultTool); exists {
		return
	}
	store, agentID := a.Storage, a.ID
	a.Tools.Register(&tool.Definition{
		Name:         ReadStoredResultTool,
		ParallelSafe: true,
		Effects:      []tool.Effect{tool.EffectRead},
		Description:  "Retrieve a bounded chunk of a compressed tool result. Continue with next_offset only when more content is necessary.",
		Permission:   tool.PermAllow,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"key":       map[string]any{"type": "string", "description": "The storage_key returned alongside a compressed tool result"},
				"offset":    map[string]any{"type": "integer", "description": "Byte offset to start reading from (default 0)"},
				"max_bytes": map[string]any{"type": "integer", "description": fmt.Sprintf("Maximum bytes to return (default %d, capped at %d)", defaultStoredResultChunkBytes, maxStoredResultChunkBytes)},
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
			if offset < len(content) && !utf8.RuneStart(content[offset]) {
				return nil, fmt.Errorf("read_stored_result: offset must be a UTF-8 character boundary")
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
			// Bound the encoded envelope, not just source bytes: JSON escaping
			// can otherwise cause the outer cap to discard continuation offsets.
			for {
				for end > offset && end < len(content) && !utf8.RuneStart(content[end]) {
					end--
				}
				result := map[string]any{
					"content": content[offset:end], "offset": offset, "next_offset": end,
					"total_bytes": len(content), "truncated": end < len(content), "storage_key": key,
				}
				encoded, err := json.Marshal(result)
				if err != nil {
					return nil, fmt.Errorf("read_stored_result: encode chunk: %w", err)
				}
				if len(encoded) <= 96<<10 {
					return result, nil
				}
				if end == offset {
					return nil, fmt.Errorf("read_stored_result: reference metadata exceeds chunk budget")
				}
				end = offset + (end-offset)/2
			}
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
	owner := agentID
	if id := storage.SessionFromContext(ctx); id != "" {
		owner = id
	}
	scope := ""
	if identity, ok := agent.RunIdentityFromContext(ctx); ok {
		scope = identity.ArtifactSnapshot
	}
	if scope == "" {
		if root, ok := builtins.WorkspaceRootFromContext(ctx); ok {
			if canonical, err := filepath.EvalSymlinks(root); err == nil {
				scope = canonical
			} else if absolute, absErr := filepath.Abs(root); absErr == nil {
				scope = filepath.Clean(absolute)
			}
		}
	}
	if scope == "" {
		return owner
	}
	digest := sha256.Sum256([]byte(scope))
	return fmt.Sprintf("%s@%x", owner, digest[:8])
}
