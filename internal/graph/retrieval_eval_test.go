package graph

import (
	"context"
	"encoding/json"
	"fmt"
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

func BenchmarkRetrievalEval(b *testing.B) {
	store, root := ownRepoStore(b)
	scope, _ := benchScope(b)
	counter, _ := evidenceCounter()
	for _, backend := range []struct {
		name string
		tool func() any
	}{{"store", nil}, {"index", nil}} {
		var def = toolFrom(&testing.T{}, Tools(store, root), "codebase_context")
		if backend.name == "index" {
			def = toolFrom(&testing.T{}, scope.Tools(), "codebase_context")
		}
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
				}
			}
			if testing.Verbose() {
				fmt.Printf("%s %-70q missed %v\n", backend.name, task.task, missed)
			}
		}
		b.ReportMetric(float64(found)/float64(total), backend.name+"_recall")
		b.ReportMetric(float64(excerpts)/float64(total), backend.name+"_excerpt_recall")
		b.ReportMetric(float64(tokens)/float64(len(retrievalTasks)), backend.name+"_tokens/task")
	}
}
