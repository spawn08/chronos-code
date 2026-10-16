package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos/sdk/harness"
)

func handoffFixture(t *testing.T) *taskRuntime {
	t.Helper()
	r, err := newTaskRuntime("handoff-test", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func handoffRead(t *testing.T, r *taskRuntime, path, content string, start, end int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(r.workspaceRoot, path), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(content, "\n")
	r.rememberRead(map[string]any{"path": path, "content": strings.Join(lines[start-1:end], "\n"), "start_line": start, "end_line": end, "content_hash": readFileState(r.workspaceRoot, path).hash})
}

func TestSubagentHandoffRanksRelevantRangesAndRejectsStaleEvidence(t *testing.T) {
	r := handoffFixture(t)
	handoffRead(t, r, "target.go", "target first\nother\ntarget last", 1, 1)
	r.rememberRead(map[string]any{"path": "target.go", "content": "target last", "start_line": 3, "end_line": 3})
	for _, path := range []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go", "g.go"} {
		handoffRead(t, r, path, "unrelated", 1, 1)
	}
	packet := r.handoff("inspect target.go")
	if !strings.Contains(packet, "target.go:1-1 @") || !strings.Contains(packet, "target.go:3-3 @") || strings.Index(packet, "target.go:") > strings.Index(packet, "g.go:") {
		t.Fatalf("relevant ranges missing or ranked late: %s", packet)
	}
	if err := os.WriteFile(filepath.Join(r.workspaceRoot, "target.go"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if packet := r.handoff("target.go"); strings.Contains(packet, "target.go:") {
		t.Fatalf("stale evidence retained: %s", packet)
	}
	if err := os.Remove(filepath.Join(r.workspaceRoot, "g.go")); err != nil {
		t.Fatal(err)
	}
	if packet := r.handoff("g.go"); strings.Contains(packet, "g.go:") {
		t.Fatalf("deleted evidence retained: %s", packet)
	}
}

func TestSubagentHandoffRejectsMismatchedReadsAndEscapingSymlinks(t *testing.T) {
	r := handoffFixture(t)
	handoffRead(t, r, "source.go", "current", 1, 1)
	for _, read := range []map[string]any{
		{"path": "source.go", "content": "old", "start_line": 1, "end_line": 1},
		{"path": "source.go", "content": "current", "content_hash": "old hash"},
		{"path": "source.go", "content": "current", "outline": true},
		{"path": "source.go", "content": "current", "document": true},
	} {
		r.rememberRead(read)
	}
	if len(r.knowledge) != 1 {
		t.Fatalf("invalid reads retained: %v", r.knowledge)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("external secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.workspaceRoot, "link")); err != nil {
		t.Fatal(err)
	}
	r.rememberRead(map[string]any{"path": "link", "content": "external secret"})
	if len(r.knowledge) != 1 {
		t.Fatal("outside-workspace evidence retained")
	}
	if err := os.Remove(filepath.Join(r.workspaceRoot, "source.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(r.workspaceRoot, "source.go")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(r.handoff("source.go"), "source.go:") {
		t.Fatal("replaced symlink evidence retained")
	}
}

func TestSubagentHandoffBoundedWithAcceptanceAndFailures(t *testing.T) {
	r := handoffFixture(t)
	handoffRead(t, r, "source.go", strings.Repeat("界", 500), 1, 1)
	if _, err := r.ledger.Record(execution.Event{Type: execution.EventRequirement, Detail: "preserve public API"}); err != nil {
		t.Fatal(err)
	}
	exit := 1
	if _, err := r.ledger.Record(execution.Event{Type: execution.EventToolCall, Command: "go test ./target", ExitCode: &exit, Detail: "old hypothesis failed"}); err != nil {
		t.Fatal(err)
	}
	packet := r.handoff("source.go")
	if !utf8.ValidString(packet) || len(packet) > subagentHandoffLimit || !strings.Contains(packet, "excerpt truncated") || !strings.Contains(packet, "preserve public API") || !strings.Contains(packet, "old hypothesis failed") || !strings.HasSuffix(packet, subagentHandoffFooter) {
		t.Fatalf("invalid packet: %s", packet)
	}
	for _, limit := range []int{0, 200, 400, 1000} {
		packet := r.boundedHandoff("source.go", limit)
		if len(packet) > limit || !utf8.ValidString(packet) || (packet != "" && !strings.HasSuffix(packet, subagentHandoffFooter)) {
			t.Fatalf("packet cut at %d: %q", limit, packet)
		}
	}
}

func TestSubagentHandoffPartialEvidenceKeepsTailAndError(t *testing.T) {
	failure := errors.New("child interrupted")
	text := "GOAL: investigate\n" + strings.Repeat("界", subagentResultLimit) + "\nEVIDENCE: artifact:late-finding; NEXT: inspect source.go"
	runner := &configuredAgentRunner{fallback: resourceRunnerFunc(func(context.Context, harness.SubAgentSpec, string) (string, error) { return text, failure })}
	report, err := runner.Run(context.Background(), harness.SubAgentSpec{Name: "child"}, "investigate")
	if !errors.Is(err, failure) || !strings.HasPrefix(report, "STATUS: partial") || len(report) > subagentResultLimit || !utf8.ValidString(report) || !strings.Contains(report, "artifact:late-finding") || !strings.Contains(report, "middle omitted") {
		t.Fatalf("partial evidence lost: len=%d error=%v", len(report), err)
	}
	preview := boundedSubagentEvidence(text, subagentPreviewLimit)
	if len(preview) > subagentPreviewLimit || !utf8.ValidString(preview) || !strings.Contains(preview, "GOAL: investigate") || !strings.Contains(preview, "artifact:late-finding") {
		t.Fatalf("preview lost evidence: %q", preview)
	}
}

func TestSubagentHandoffRunnerPreservesTaskBudget(t *testing.T) {
	r := handoffFixture(t)
	handoffRead(t, r, "source.go", "observed", 1, 1)
	var seen string
	runner := &configuredAgentRunner{fallback: resourceRunnerFunc(func(_ context.Context, _ harness.SubAgentSpec, task string) (string, error) {
		seen = task
		return "STATUS: complete", nil
	})}
	task := strings.Repeat("x", subagentTaskLimit-500)
	if _, err := runner.Run(withTaskRuntime(context.Background(), r), harness.SubAgentSpec{Name: "child"}, task); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(seen, task) || len(seen)+len("child") > subagentTaskLimit || !strings.HasSuffix(seen, subagentHandoffFooter) {
		t.Fatalf("task bound or footer lost: %d", len(seen))
	}
	task = strings.Repeat("x", subagentTaskLimit-len("child"))
	if _, err := runner.Run(withTaskRuntime(context.Background(), r), harness.SubAgentSpec{Name: "child"}, task); err != nil {
		t.Fatal(err)
	}
	if seen != task {
		t.Fatal("near-limit task was truncated or extended")
	}
}
