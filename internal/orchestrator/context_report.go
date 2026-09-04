package orchestrator

import (
	"context"
	"sync"
)

// ContextSourceKind identifies a dynamic context source without exposing the
// source's content or configuration.
type ContextSourceKind string

const (
	ContextSourceSessionSummaries ContextSourceKind = "session_summaries"
	ContextSourceMemory           ContextSourceKind = "memory"
	ContextSourceLearnedPattern   ContextSourceKind = "learned_pattern"
	ContextSourceProjectDocs      ContextSourceKind = "project_docs"
	ContextSourceSkills           ContextSourceKind = "skills"
	ContextSourceDiagnostics      ContextSourceKind = "diagnostics"
	ContextSourceGraphPrediction  ContextSourceKind = "graph_prediction"
	ContextSourceUserHook         ContextSourceKind = "user_hook"

	ContextOmittedNotConfigured = "not_configured"
	ContextOmittedNotSelected   = "not_selected"
	ContextOmittedSourceError   = "source_error"
	ContextOmittedDisabled      = "disabled"
)

// ContextSourceReport is metadata-only. ID and Title are static source
// identities, never record IDs, paths, queries, hook names, or prompt text.
type ContextSourceReport struct {
	Kind           ContextSourceKind `json:"kind"`
	ID             string            `json:"id"`
	Title          string            `json:"title"`
	SelectedCount  int               `json:"selected_count"`
	Bytes          int               `json:"bytes"`
	BudgetBytes    int               `json:"budget_bytes,omitempty"`
	OmissionReason string            `json:"omission_reason,omitempty"`
	Truncated      bool              `json:"truncated,omitempty"`
}

// ContextReport describes dynamic context composition without disclosing any
// of the composed context itself.
type ContextReport struct {
	Sources     []ContextSourceReport `json:"sources"`
	TotalCount  int                   `json:"total_count"`
	TotalBytes  int                   `json:"total_bytes"`
	BudgetBytes int                   `json:"budget_bytes"`
	Truncated   bool                  `json:"truncated"`
}

type contextReportKey struct{}

type contextReportCollector struct {
	mu      sync.Mutex
	sources []ContextSourceReport
}

var contextSourceDefinitions = []ContextSourceReport{
	{Kind: ContextSourceSessionSummaries, ID: "session-summaries", Title: "Prior session summaries", BudgetBytes: maxPriorSessionSummaryBytes},
	{Kind: ContextSourceMemory, ID: "memory", Title: "Memory intent and recall", BudgetBytes: 800},
	{Kind: ContextSourceLearnedPattern, ID: "learned-pattern", Title: "Learned pattern", BudgetBytes: 1000},
	{Kind: ContextSourceProjectDocs, ID: "project-docs", Title: "Project instructions", BudgetBytes: 64000},
	{Kind: ContextSourceSkills, ID: "skills", Title: "Selected skills", BudgetBytes: 32000},
	{Kind: ContextSourceDiagnostics, ID: "diagnostics", Title: "LSP diagnostics"},
	{Kind: ContextSourceGraphPrediction, ID: "graph-prediction", Title: "Graph prediction"},
	{Kind: ContextSourceUserHook, ID: "user-hook", Title: "User prompt hooks", BudgetBytes: userHookPromptContextTokens * 4},
}

func newContextReportCollector() *contextReportCollector {
	sources := append([]ContextSourceReport(nil), contextSourceDefinitions...)
	for i := range sources {
		sources[i].OmissionReason = ContextOmittedNotConfigured
	}
	return &contextReportCollector{sources: sources}
}

func withContextReportCollector(ctx context.Context, collector *contextReportCollector) context.Context {
	return context.WithValue(ctx, contextReportKey{}, collector)
}

func contextSourceSelected(ctx context.Context, kind ContextSourceKind, count, bytes int, truncated bool) {
	if collector, _ := ctx.Value(contextReportKey{}).(*contextReportCollector); collector != nil {
		collector.record(kind, count, bytes, "", truncated)
	}
}

func contextSourceOmitted(ctx context.Context, kind ContextSourceKind, reason string) {
	if collector, _ := ctx.Value(contextReportKey{}).(*contextReportCollector); collector != nil {
		collector.record(kind, 0, 0, safeOmissionReason(reason), false)
	}
}

func safeOmissionReason(reason string) string {
	switch reason {
	case ContextOmittedNotConfigured, ContextOmittedNotSelected, ContextOmittedSourceError, ContextOmittedDisabled:
		return reason
	case "auto_extract_disabled", "memory_disabled":
		return ContextOmittedDisabled
	default:
		return ContextOmittedSourceError
	}
}

func (c *contextReportCollector) record(kind ContextSourceKind, count, bytes int, reason string, truncated bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.sources {
		if c.sources[i].Kind != kind {
			continue
		}
		if reason != "" && c.sources[i].SelectedCount > 0 {
			return
		}
		c.sources[i].SelectedCount = count
		c.sources[i].Bytes = bytes
		c.sources[i].OmissionReason = reason
		c.sources[i].Truncated = truncated
		return
	}
}

func (c *contextReportCollector) report() ContextReport {
	c.mu.Lock()
	defer c.mu.Unlock()
	report := ContextReport{Sources: append([]ContextSourceReport(nil), c.sources...)}
	for _, source := range report.Sources {
		report.TotalCount += source.SelectedCount
		report.TotalBytes += source.Bytes
		report.BudgetBytes += source.BudgetBytes
		report.Truncated = report.Truncated || source.Truncated
	}
	return report
}
