package learning

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/spawn08/chronos/storage"
)

// Report aggregates execution-trace statistics across a set of sessions for
// a single agent into counts only — never raw trace input/output payloads,
// which may contain source code, file contents, or shell output that
// shouldn't be forwarded to a distillation model verbatim (PRD P3-001's
// "capture execution traces" feeding P3-002's "LLM-powered analysis").
type Report struct {
	AgentID       string
	Sessions      []string
	TotalSpans    int
	ModelCalls    int
	ToolCalls     int
	Errors        int
	ToolCounts    map[string]int
	ToolSequences map[string]int // "toolA>toolB" bigram counts
}

// Analyze pulls every trace for sessionIDs from store and reduces them to a
// Report. Traces within a session are ordered by StartedAt before computing
// tool-call bigrams, so sequences reflect actual execution order.
func Analyze(ctx context.Context, store storage.Storage, agentID string, sessionIDs []string) (*Report, error) {
	report := &Report{
		AgentID:       agentID,
		Sessions:      sessionIDs,
		ToolCounts:    map[string]int{},
		ToolSequences: map[string]int{},
	}
	for _, sid := range sessionIDs {
		traces, err := store.ListTraces(ctx, sid)
		if err != nil {
			return nil, fmt.Errorf("learning: list traces for %q: %w", sid, err)
		}
		sort.Slice(traces, func(i, j int) bool { return traces[i].StartedAt.Before(traces[j].StartedAt) })

		var lastTool string
		for _, t := range traces {
			report.TotalSpans++
			if t.Error != "" {
				report.Errors++
			}
			switch t.Kind {
			case "model_call":
				report.ModelCalls++
			case "tool_call":
				report.ToolCalls++
				name := strings.TrimPrefix(t.Name, "tool:")
				report.ToolCounts[name]++
				if lastTool != "" {
					report.ToolSequences[lastTool+">"+name]++
				}
				lastTool = name
			}
		}
	}
	return report, nil
}

// countEntry pairs a name with its count, used for stable top-N ranking.
type countEntry struct {
	name  string
	count int
}

func topN(counts map[string]int, n int) []countEntry {
	entries := make([]countEntry, 0, len(counts))
	for name, count := range counts {
		entries = append(entries, countEntry{name, count})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].count != entries[j].count {
			return entries[i].count > entries[j].count
		}
		return entries[i].name < entries[j].name // stable tie-break
	})
	if n > 0 && len(entries) > n {
		entries = entries[:n]
	}
	return entries
}

// Empty reports whether the report has no recorded spans at all, meaning
// tracing hasn't captured anything yet for these sessions (PRD P3-001 not
// wired, or the sessions genuinely made no tool/model calls).
func (r *Report) Empty() bool {
	return r.TotalSpans == 0
}

// Summary renders the report as a compact, deterministic plain-text block
// suitable as the user-turn content of a distillation prompt (see
// Distiller.Suggest). It stays under a few hundred tokens regardless of how
// many sessions were analyzed, since it only ever lists top-N aggregates.
func (r *Report) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent: %s\n", r.AgentID)
	fmt.Fprintf(&b, "Sessions analyzed: %d\n", len(r.Sessions))
	fmt.Fprintf(&b, "Total spans: %d (model calls: %d, tool calls: %d, errors: %d)\n",
		r.TotalSpans, r.ModelCalls, r.ToolCalls, r.Errors)

	b.WriteString("Top tools by call count:\n")
	for _, e := range topN(r.ToolCounts, 10) {
		fmt.Fprintf(&b, "  %s: %d\n", e.name, e.count)
	}

	b.WriteString("Top tool sequences (A>B = A immediately followed by B):\n")
	for _, e := range topN(r.ToolSequences, 10) {
		fmt.Fprintf(&b, "  %s: %d\n", e.name, e.count)
	}

	if r.TotalSpans > 0 {
		errRate := float64(r.Errors) / float64(r.TotalSpans)
		fmt.Fprintf(&b, "Error rate: %s\n", strconv.FormatFloat(errRate, 'f', 3, 64))
	}

	return b.String()
}
