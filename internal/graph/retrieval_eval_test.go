package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

// Retrieval eval (docs/chronos-indexer.md, "Evaluation"): tasks phrased as
// an agent receives them, each with the declarations a principal engineer
// would open first. Recall is the share of those found in one
// codebase_context call at 4,096 tokens; excerpt recall counts only those
// shown with source. Run with:
//
//	go test ./internal/graph -run '^$' -bench RetrievalEval -benchtime=1x
var retrievalTasks = []struct {
	task string
	gold []string
}{
	{"the watcher should flush a burst of file events into one index update", []string{"loop", "Flush", "Update"}},
	{"a corrupt segment must be detected on open and the index rebuilt", []string{"Parse", "load", "reset"}},
	{"compaction merges overlays into a new base segment", []string{"maybeCompact", "Compact"}},
	{"list the packages that import a given Go package", []string{"PackageDependents", "Importers"}},
	{"codebase_context exceeds its token budget", []string{"fitEvidence", "shrinkEvidence"}},
	{"graph tools should accept a names list and answer each name", []string{"batched", "batchNames"}},
	{"inject repository context before each turn", []string{"Prefetch", "Retrieve"}},
	{"BM25 ranking of symbols for codebase_search", []string{"Search", "buildSegIndex"}},
	{"find implementations of an interface without type checking", []string{"Implementations", "methodSet"}},
	{"two sessions must not write the same index; use a lock", []string{"lockDir", "openRoot"}},
}

// documentTasks (M8) ask about this repository's documents; the gold names
// are the sections that answer them (Markdown headings). They are scored
// separately so the code recall above stays comparable across milestones.
var documentTasks = []struct {
	task string
	gold []string
}{
	{"why was the old SQLite code graph slow", []string{"Why the current graph is slow"}},
	{"what must not be built in v1", []string{"10. Explicit rejects (do not build in v1)"}},
	{"how does the writer lock stop two processes writing one index", []string{"Writer lock"}},
	{"what precision must each language reach for import_resolved edges", []string{"M7 precision targets"}},
	{"how are overlays routed and compacted", []string{"Overlays, routing, compaction"}},
	{"binary size strategy to stay under 20 MB", []string{"6. Binary size strategy — hitting <20 MB"}},
	{"precedence between global and project configuration", []string{"Precedence"}},
	{"enterprise authentication for Claude and Codex", []string{"5.3 Enterprise Claude / Codex authentication"}},
}

func BenchmarkRetrievalEval(b *testing.B) {
	scope, _ := benchScope(b)
	counter, _ := evidenceCounter()
	def := toolFrom(&testing.T{}, scope.Tools(), "codebase_context")
	found, excerpts, total, tokens := 0, 0, 0, 0
	for _, task := range retrievalTasks {
		out, err := def.Handler(context.Background(), map[string]any{"query": task.task, "max_tokens": 4096})
		if err != nil {
			b.Fatal(err)
		}
		r := out.(*evidenceResult)
		data, _ := json.Marshal(r)
		tokens += counter.CountString(string(data))
		names, shown := map[string]bool{}, map[string]bool{}
		for _, it := range r.Items {
			names[it.Name] = true
			if it.Source != nil && it.Source.Text != "" {
				shown[it.Name] = true
			}
		}
		var missed []string
		for _, g := range task.gold {
			total++
			if names[g] {
				found++
			} else {
				missed = append(missed, g)
			}
			if shown[g] {
				excerpts++
			} else if names[g] && os.Getenv("EVAL_DEBUG") != "" {
				fmt.Printf("    no excerpt: %s\n", g)
			}
		}
		if testing.Verbose() {
			fmt.Printf("%-70q missed %v\n", task.task, missed)
		}
	}
	docFound, docTotal := 0, 0
	for _, task := range documentTasks {
		out, err := def.Handler(context.Background(), map[string]any{"query": task.task, "max_tokens": 4096})
		if err != nil {
			b.Fatal(err)
		}
		names := map[string]bool{}
		var got []string
		for _, it := range out.(*evidenceResult).Items {
			names[it.Name] = true
			got = append(got, it.Name)
		}
		var missed []string
		for _, g := range task.gold {
			docTotal++
			if names[g] {
				docFound++
			} else {
				missed = append(missed, g)
			}
		}
		if testing.Verbose() {
			fmt.Printf("doc %-66q missed %v\n", task.task, missed)
			if len(missed) > 0 && os.Getenv("EVAL_DEBUG") != "" {
				fmt.Printf("    got %q\n", got)
			}
		}
	}
	b.ReportMetric(float64(docFound)/float64(docTotal), "doc_recall")
	b.ReportMetric(float64(found)/float64(total), "recall")
	b.ReportMetric(float64(excerpts)/float64(total), "excerpt_recall")
	b.ReportMetric(float64(tokens)/float64(len(retrievalTasks)), "tokens/task")
}
