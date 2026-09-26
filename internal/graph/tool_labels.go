package graph

import (
	"context"
	"fmt"
	"strings"

	"github.com/spawn08/chronos/engine/tool"
)

// maxBatchNames bounds the names answered by one batched call.
const maxBatchNames = 16

// labelResult attaches the backend's freshness report to a tool result, and
// how relationships were matched when relation is true. When empty is true a
// note says what was searched and how current the index is, so an agent can
// trust the absence instead of re-checking with grep. Backends that do not
// report (test fakes) leave results unchanged.
func labelResult(store Backend, out map[string]any, relation, empty bool, note string) map[string]any {
	r, ok := store.(Reporter)
	if !ok {
		return out
	}
	report := r.IndexReport()
	index := map[string]any{"mode": report.Mode, "generation": report.Generation, "files": report.Files, "up_to_date": report.UpToDate}
	if report.Pending > 0 {
		index["pending_changes"] = report.Pending
	}
	if report.Building {
		index["building"] = true
	}
	if report.Partial {
		index["coverage"] = "partial"
	}
	if report.Error != "" {
		index["error"] = report.Error
	}
	out["index"] = index
	if relation && report.Relations != "" {
		out["resolution"] = report.Relations
	}
	if empty && note != "" {
		out["note"] = note + " (" + describeReport(report) + ")."
	}
	return out
}

// batched lets a single-name tool answer a "names" list in one call. Each
// entry of "results" is exactly what a separate call with that name returns,
// minus the shared "index" report, which is hoisted to the top level.
func batched(def *tool.Definition) *tool.Definition {
	single := def.Handler
	params := cloneSchema(def.Parameters)
	props, _ := params["properties"].(map[string]any)
	if props != nil {
		props["names"] = map[string]any{
			"type": "array", "minItems": 1, "maxItems": maxBatchNames, "items": map[string]any{"type": "string"},
			"description": "Batch: several names answered in one call; each result equals a separate call with that name. Use instead of name.",
		}
	}
	delete(params, "required")
	def.Parameters = params
	def.Description += " Pass names to look up several at once."
	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		raw, has := args["names"]
		if !has {
			return single(ctx, args)
		}
		names, err := batchNames(def.Name, raw, args["name"])
		if err != nil {
			return nil, err
		}
		results := make([]map[string]any, 0, len(names))
		var index any
		for _, name := range names {
			one := make(map[string]any, len(args))
			for k, v := range args {
				if k != "names" {
					one[k] = v
				}
			}
			one["name"] = name
			res, err := single(ctx, one)
			entry := map[string]any{"name": name}
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				entry["error"] = err.Error()
			default:
				if m, ok := res.(map[string]any); ok {
					if idx, ok := m["index"]; ok {
						index = idx
						delete(m, "index")
					}
				}
				entry["result"] = res
			}
			results = append(results, entry)
		}
		out := map[string]any{"results": results}
		if index != nil {
			out["index"] = index
		}
		return out, nil
	}
	return def
}

func batchNames(tool string, raw, name any) ([]string, error) {
	var names []string
	if n, ok := name.(string); ok && strings.TrimSpace(n) != "" {
		names = append(names, strings.TrimSpace(n))
	}
	switch v := raw.(type) {
	case []string:
		names = append(names, v...)
	case []any:
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("%s: names must be strings", tool)
			}
			names = append(names, s)
		}
	default:
		return nil, fmt.Errorf("%s: names must be a list of strings", tool)
	}
	seen := map[string]bool{}
	out := names[:0]
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n != "" && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s: names must contain at least one name", tool)
	}
	if len(out) > maxBatchNames {
		return nil, fmt.Errorf("%s: at most %d names per call", tool, maxBatchNames)
	}
	return out, nil
}

func cloneSchema(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			v = cloneSchema(sub)
		}
		out[k] = v
	}
	return out
}
