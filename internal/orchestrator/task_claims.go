package orchestrator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/claims"
	"github.com/spawn08/chronos-code/internal/execution"
)

// maxPromptClaims bounds the working-memory lines added to runtime-built
// prompts. Non-live claims are listed first because they carry the signal
// the model most needs: a span it read has changed since.
const maxPromptClaims = 12

// recordReadClaim anchors a ranged file_read to the exact span it returned,
// so the task's working memory knows when that span later changes. Whole-file
// reads, outlines and extracted documents are skipped: a whole-file anchor goes
// stale on any edit and outlines/documents do not map to source lines.
// Claims are best-effort working memory and never fail the tool call.
func (r *taskRuntime) recordReadClaim(args map[string]any, result any) {
	if r == nil || r.claims == nil {
		return
	}
	if args["start_line"] == nil && args["end_line"] == nil {
		return
	}
	values, _ := result.(map[string]any)
	if truthy(values["outline"]) || truthy(values["document"]) {
		return
	}
	path, _ := values["path"].(string)
	if path == "" {
		path, _ = args["path"].(string)
	}
	start, okStart := intValue(values["start_line"])
	end, okEnd := intValue(values["end_line"])
	if !okStart || !okEnd {
		// Compressed or middleware-shaped results lose coordinates; fall
		// back to the requested range.
		start, okStart = intValue(args["start_line"])
		end, okEnd = intValue(args["end_line"])
		if !okStart {
			start, okStart = 1, true
		}
	}
	if path == "" || !okStart || start < 1 {
		return
	}
	rel, err := r.claims.RelPath(path)
	if err != nil {
		return
	}
	total := fileLineCount(filepath.Join(r.workspaceRoot, filepath.FromSlash(rel)))
	if total == 0 {
		return
	}
	if !okEnd || end > total {
		end = total
	}
	if end < start {
		return
	}
	for _, existing := range r.claims.List() {
		if existing.Status != claims.StatusLive || len(existing.Anchors) != 1 {
			continue
		}
		a := existing.Anchors[0]
		if a.Path == rel && a.StartLine == start && a.EndLine == end {
			return
		}
	}
	_, _ = r.claims.Add(claims.Input{
		Text:    fmt.Sprintf("read %s:%d-%d", rel, start, end),
		Anchors: []claims.Anchor{{Path: rel, StartLine: start, EndLine: end}},
	})
}

// refreshClaims re-checks claims anchored to paths (every claim when paths is
// empty) and records each status transition as an audit-only ledger event.
func (r *taskRuntime) refreshClaims(completedAt time.Time, paths ...string) error {
	if r == nil || r.claims == nil {
		return nil
	}
	for _, change := range r.claims.Refresh(paths...) {
		claim, ok := r.claims.Get(change.ID)
		if !ok {
			continue
		}
		anchorPaths := make([]string, 0, len(claim.Anchors))
		for _, a := range claim.Anchors {
			anchorPaths = append(anchorPaths, a.Path)
		}
		detail := fmt.Sprintf("claim %s %s -> %s: %s", change.ID, change.From, change.To, claim.Text)
		if claim.Reason != "" {
			detail += " (" + claim.Reason + ")"
		}
		if _, err := r.ledger.Record(execution.Event{
			Type:        execution.EventClaim,
			Paths:       anchorPaths,
			Detail:      detail,
			Provenance:  execution.ProvenanceRuntime,
			CompletedAt: completedAt,
		}); err != nil {
			return fmt.Errorf("record claim transition: %w", err)
		}
	}
	return nil
}

// claimsDigest renders bounded working-memory lines for runtime prompts.
// Stale and doubted claims come first so the model re-reads before relying
// on them; live claims fill the remaining budget. Order is deterministic.
func (r *taskRuntime) claimsDigest() []string {
	if r == nil || r.claims == nil {
		return nil
	}
	all := r.claims.List()
	if len(all) == 0 {
		return nil
	}
	var lines []string
	for _, status := range []claims.Status{claims.StatusStale, claims.StatusDoubted, claims.StatusLive} {
		for _, claim := range all {
			if claim.Status != status || len(lines) >= maxPromptClaims {
				continue
			}
			line := fmt.Sprintf("- [%s] %s %s", claim.Status, claim.ID, claim.Text)
			switch claim.Status {
			case claims.StatusStale:
				line += " — changed since read; re-read before relying on it"
			case claims.StatusDoubted:
				line += " — " + claim.Reason + "; re-check"
			}
			lines = append(lines, line)
		}
	}
	omitted := len(all) - len(lines)
	header := "Working memory (anchored reads from this task):"
	if omitted > 0 {
		header = fmt.Sprintf("Working memory (anchored reads from this task; %d live claims omitted):", omitted)
	}
	return append([]string{header}, lines...)
}

func fileLineCount(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return len(strings.Split(string(data), "\n"))
}

func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	default:
		return 0, false
	}
}

func truthy(v any) bool {
	b, _ := v.(bool)
	return b
}
