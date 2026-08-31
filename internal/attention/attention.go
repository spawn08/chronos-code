// Package attention implements PRD P3-008: attention budgeting for context
// compaction. It assigns relevance weights to tool results by category —
// edit targets and test output are preserved at full fidelity, while old
// exploratory reads are compressed aggressively — producing a leaner, more
// focused context window than uniform compression.
package attention

import (
	"context"
	"strings"
	"sync"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/storage"
)

// Category classifies a piece of context by its relevance to the active task.
type Category string

const (
	CatEditTarget Category = "edit_target"
	CatPlan       Category = "plan"
	CatTestOutput Category = "test_output"
	CatGraph      Category = "graph"
	CatRead       Category = "read"
	CatOther      Category = "other"
)

// categoryWeights maps each category to its attention weight. Higher weight
// means the item should be preserved with more fidelity (higher compression
// threshold → less compression). Values are from PRD §13, Feature 6.
var categoryWeights = map[Category]float64{
	CatEditTarget: 1.0,
	CatPlan:       0.95,
	CatTestOutput: 0.9,
	CatGraph:      0.7,
	CatRead:       0.3,
	CatOther:      0.1,
}

// Weight returns the attention weight for cat, defaulting to 0.1 for unknown
// categories.
func Weight(cat Category) float64 {
	if w, ok := categoryWeights[cat]; ok {
		return w
	}
	return 0.1
}

// Classify returns the attention category for a tool call based on its name
// and arguments.
func Classify(toolName string, args map[string]any) Category {
	switch {
	case toolName == "file_write":
		return CatEditTarget
	case toolName == "update_plan" || toolName == "create_plan":
		return CatPlan
	case toolName == "shell":
		if isTestCommand(args) {
			return CatTestOutput
		}
		return CatOther
	case strings.HasPrefix(toolName, "graph_") ||
		toolName == "find_callers" ||
		toolName == "find_implementations" ||
		toolName == "resolve_symbol" ||
		toolName == "multi_resolution_view" ||
		toolName == "impact_analysis" ||
		toolName == "test_map" ||
		toolName == "co_change":
		return CatGraph
	case toolName == "file_read" || toolName == "file_list" ||
		toolName == "file_glob" || toolName == "file_grep" ||
		toolName == "semantic_search" || toolName == "workspace_info":
		return CatRead
	default:
		return CatOther
	}
}

func isTestCommand(args map[string]any) bool {
	cmd, _ := args["command"].(string)
	return strings.Contains(cmd, "test") ||
		strings.Contains(cmd, "pytest") ||
		strings.Contains(cmd, "jest") ||
		strings.Contains(cmd, "cargo test") ||
		strings.Contains(cmd, "make test")
}

// AdjustThreshold scales a base compression threshold by attention weight.
// High-weight items (edit targets, tests) get a higher threshold (less
// compression). Low-weight items (old reads, misc) get a lower threshold
// (more compression). The result is clamped to [50, base*2].
func AdjustThreshold(base int, weight float64) int {
	factor := 0.25 + weight*1.75
	adjusted := int(float64(base) * factor)
	if adjusted < 50 {
		adjusted = 50
	}
	ceiling := base * 2
	if ceiling < 100 {
		ceiling = 100
	}
	if adjusted > ceiling {
		adjusted = ceiling
	}
	return adjusted
}

// contextKey is the context key for the current tool name, set by the Hook
// before tool execution so downstream compression can look it up.
type contextKey struct{}

// ToolNameFromContext returns the tool name stored by the Budgeter hook, or
// "" if none is set.
func ToolNameFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(contextKey{}).(string); ok {
		return v
	}
	return ""
}

// Budgeter tracks tool calls per session and classifies them by attention
// category. It implements hooks.Hook to observe tool calls and inject the
// current tool name into the context for downstream compression.
type Budgeter struct {
	mu    sync.Mutex
	calls map[string][]callRecord // sessionID -> recent calls
	max   int
}

type callRecord struct {
	ToolName string
	Category Category
	Weight   float64
}

// NewBudgeter creates a Budgeter that tracks the last maxRecords tool calls
// per session.
func NewBudgeter(maxRecords int) *Budgeter {
	if maxRecords <= 0 {
		maxRecords = 100
	}
	return &Budgeter{
		calls: make(map[string][]callRecord),
		max:   maxRecords,
	}
}

// Before implements hooks.Hook. On EventToolCallBefore, it stores the tool
// name in the context (via the returned error's absence — chronos doesn't
// support context mutation from hooks, so the tool name is recorded
// internally instead and looked up via CurrentWeight).
func (b *Budgeter) Before(_ context.Context, evt *hooks.Event) error {
	return nil
}

// After implements hooks.Hook. On EventToolCallAfter, it records the tool
// call with its attention category for session-level tracking.
func (b *Budgeter) After(ctx context.Context, evt *hooks.Event) error {
	if evt.Type != hooks.EventToolCallAfter {
		return nil
	}
	toolName := evt.Name
	args, _ := evt.Input.(map[string]any)
	cat := Classify(toolName, args)
	w := Weight(cat)

	sid := sessionFromContext(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	recs := b.calls[sid]
	if len(recs) >= b.max {
		recs = recs[1:]
	}
	b.calls[sid] = append(recs, callRecord{
		ToolName: toolName,
		Category: cat,
		Weight:   w,
	})
	return nil
}

// CurrentWeight returns the attention weight for the most recent tool call
// in the given session. If no calls have been recorded, it returns 0.5 (a
// neutral weight).
func (b *Budgeter) CurrentWeight(sessionID string) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	recs := b.calls[sessionID]
	if len(recs) == 0 {
		return 0.5
	}
	return recs[len(recs)-1].Weight
}

// WeightForTool returns the attention weight for the given tool name and
// arguments.
func (b *Budgeter) WeightForTool(toolName string, args map[string]any) float64 {
	return Weight(Classify(toolName, args))
}

// CategoryDistribution returns the count of tool calls per category for the
// given session, useful for diagnostics.
func (b *Budgeter) CategoryDistribution(sessionID string) map[Category]int {
	b.mu.Lock()
	defer b.mu.Unlock()
	dist := make(map[Category]int)
	for _, r := range b.calls[sessionID] {
		dist[r.Category]++
	}
	return dist
}

// HighAttentionSummary returns a one-line summary of what the agent is
// currently focused on, based on the most recent high-weight tool calls.
func (b *Budgeter) HighAttentionSummary(sessionID string) string {
	b.mu.Lock()
	recs := b.calls[sessionID]
	b.mu.Unlock()
	if len(recs) == 0 {
		return ""
	}

	var edits, tests int
	for _, r := range recs {
		switch r.Category {
		case CatEditTarget:
			edits++
		case CatTestOutput:
			tests++
		}
	}
	var parts []string
	if edits > 0 {
		parts = append(parts, "editing")
	}
	if tests > 0 {
		parts = append(parts, "testing")
	}
	if len(parts) == 0 {
		return "exploring"
	}
	return strings.Join(parts, "+")
}

func sessionFromContext(ctx context.Context) string {
	if id := storage.SessionFromContext(ctx); id != "" {
		return id
	}
	return "_default"
}
