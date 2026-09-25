package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
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
	if _, err := r.claims.Add(claims.Input{
		Text:    fmt.Sprintf("read %s:%d-%d", rel, start, end),
		Anchors: []claims.Anchor{{Path: rel, StartLine: start, EndLine: end}},
	}); err == nil {
		_ = r.saveClaims()
	}
}

// refreshClaims re-checks claims anchored to paths (every claim when paths is
// empty) and records each status transition as an audit-only ledger event.
func (r *taskRuntime) refreshClaims(completedAt time.Time, paths ...string) error {
	if r == nil || r.claims == nil {
		return nil
	}
	// Refresh can relocate moved anchors without a status change, so the
	// snapshot is rewritten whenever claims exist. Persistence is best-effort
	// working memory and never fails the caller.
	defer func() { _ = r.saveClaims() }()
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

// claimsSnapshotVersion identifies the on-disk claims snapshot format.
const claimsSnapshotVersion = 1

type claimsSnapshot struct {
	Version int            `json:"version"`
	Claims  []claims.Claim `json:"claims"`
}

// claimsSnapshotPath places the claims snapshot beside the task's ledger file.
func claimsSnapshotPath(ledgerPath string) string {
	return strings.TrimSuffix(ledgerPath, ".jsonl") + ".claims.json"
}

// loadClaims replaces the in-memory store with the durable snapshot at
// claimsPath. A missing snapshot is an empty store. Callers must Refresh
// afterwards: the workspace may have changed while no task was running.
func (r *taskRuntime) loadClaims() error {
	if r == nil || r.claimsPath == "" {
		return nil
	}
	data, err := os.ReadFile(r.claimsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read claims snapshot: %w", err)
	}
	var snapshot claimsSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return fmt.Errorf("decode claims snapshot: %w", err)
	}
	if snapshot.Version != claimsSnapshotVersion {
		return fmt.Errorf("claims snapshot version %d is not supported", snapshot.Version)
	}
	store, err := claims.Restore(r.workspaceRoot, snapshot.Claims)
	if err != nil {
		return fmt.Errorf("restore claims snapshot: %w", err)
	}
	r.claims = store
	return nil
}

// saveClaims atomically rewrites the durable snapshot (temp file, fsync,
// rename), so a crash leaves either the previous or the new snapshot.
func (r *taskRuntime) saveClaims() error {
	if r == nil || r.claims == nil || r.claimsPath == "" {
		return nil
	}
	r.claimsSaveMu.Lock()
	defer r.claimsSaveMu.Unlock()
	data, err := json.Marshal(claimsSnapshot{Version: claimsSnapshotVersion, Claims: r.claims.List()})
	if err != nil {
		return fmt.Errorf("encode claims snapshot: %w", err)
	}
	dir := filepath.Dir(r.claimsPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create claims snapshot dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(r.claimsPath)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create claims snapshot: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write claims snapshot: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync claims snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close claims snapshot: %w", err)
	}
	if err := os.Rename(tmp.Name(), r.claimsPath); err != nil {
		return fmt.Errorf("commit claims snapshot: %w", err)
	}
	return nil
}

// withWorkingMemory appends the claims digest to a turn's opening prompt so
// the model sees carried-over reads, and which of them have since changed,
// before it acts. With no claims the prompt is unchanged.
func (r *taskRuntime) withWorkingMemory(ctx context.Context, message string) string {
	lines := r.claimsDigest()
	if len(lines) == 0 {
		contextSourceOmitted(ctx, ContextSourceWorkingMemory, ContextOmittedNotSelected)
		return message
	}
	block := strings.Join(lines, "\n")
	truncated := r.claims != nil && len(r.claims.List()) > len(lines)-1
	contextSourceSelected(ctx, ContextSourceWorkingMemory, len(lines)-1, len(block), truncated)
	return message + "\n\n" + block
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
