package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/execution"
)

func call(name, args string) []model.ToolCall {
	return []model.ToolCall{{ID: "c", Name: name, Arguments: args}}
}

func TestFutilityDetector(t *testing.T) {
	tests := []struct {
		name   string
		rounds [][]model.ToolCall
		want   string // substring of the steer on the last round; "" for none
	}{
		{"log polling is steered to await_shell", [][]model.ToolCall{
			call("file_read", `{"path":"data/migration.log"}`),
			call("file_read", `{"path":"data/migration.log"}`),
			call("file_read", `{"path":"data/migration.log"}`),
		}, "await_shell"},
		{"argument order does not hide a repeat", [][]model.ToolCall{
			call("shell", `{"command":"git status","working_dir":"."}`),
			call("shell", `{"working_dir":".","command":"git status"}`),
			call("shell", `{"command":"git status","working_dir":"."}`),
		}, "same result"},
		{"reading many different files is not futile", [][]model.ToolCall{
			call("file_read", `{"path":"a.go"}`), call("file_read", `{"path":"b.go"}`), call("file_read", `{"path":"c.go"}`),
		}, ""},
		{"two repeats are below the threshold", [][]model.ToolCall{
			call("file_read", `{"path":"a.go"}`), call("file_read", `{"path":"a.go"}`),
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newFutilityDetector()
			var steer string
			for _, r := range tt.rounds {
				steer = d.observe(r, nil)
			}
			if tt.want == "" && steer != "" || tt.want != "" && !strings.Contains(steer, tt.want) {
				t.Fatalf("steer = %q, want containing %q", steer, tt.want)
			}
		})
	}
}

func TestFutilityDetectorChecksAndWrites(t *testing.T) {
	runtime, err := newTaskRuntime("task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d := newFutilityDetector()
	fail := 1
	now := time.Now()
	state := func() *execution.State {
		s, err := runtime.snapshot()
		if err != nil {
			t.Fatal(err)
		}
		return &s
	}
	runTest := func() string {
		if _, err := runtime.recordCommand("cargo test", execution.CommandTest, &fail, execution.TerminalExited, nil, execution.ProvenanceRuntime, now, now); err != nil {
			t.Fatal(err)
		}
		return d.observe(call("shell", `{"command":"cargo test"}`), state())
	}
	if steer := runTest(); steer != "" {
		t.Fatalf("first run steered: %q", steer)
	}
	if steer := runTest(); !strings.Contains(steer, "no file change in between") {
		t.Fatalf("re-run on an unchanged tree not flagged: %q", steer)
	}
	// An edit makes the next run a legitimate re-check.
	if _, err := runtime.recordWrite("src/lib.rs", "h1", 1, execution.ProvenanceRuntime, now); err != nil {
		t.Fatal(err)
	}
	steer := runTest()
	if strings.Contains(steer, "no file change in between") {
		t.Fatalf("re-check after an edit was flagged as a repeat: %q", steer)
	}
	if !strings.Contains(steer, "failed 3 times in a row") {
		t.Fatalf("third consecutive failure not flagged: %q", steer)
	}
}

func TestWindowGovernorSteersBeforePausing(t *testing.T) {
	g := testGovernor(t, workWindow{rounds: 2}, nil)
	g.policy.noProgressWindows = 3
	var steers []string
	stopped := 0
	for round := 1; round <= 12 && stopped == 0; round++ {
		action, err := g.AfterToolRound(context.Background(), governorRound("file_read", `{"path":"a.go"}`))
		if err != nil {
			t.Fatal(err)
		}
		if action.Steer != "" {
			steers = append(steers, fmt.Sprintf("%d:%s", round, action.Steer))
		}
		if action.Stop {
			stopped = round
		}
	}
	joined := strings.Join(steers, "\n")
	if !strings.Contains(joined, "Futility check") || !strings.Contains(joined, "state in two or three sentences") || !strings.Contains(joined, "Change strategy now") {
		t.Fatalf("missing ladder rungs before pause:\n%s", joined)
	}
	if stopped != 8 {
		t.Fatalf("paused at round %d, want 8 (one productive window, then three idle)", stopped)
	}
}

// A governor started on a runtime that already holds evidence (a resumed
// task, or a subagent sharing its parent's ledger) does not steer about it.
func TestFutilityIgnoresPriorEvidence(t *testing.T) {
	runtime, err := newTaskRuntime("task", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fail := 1
	now := time.Now()
	for i := 0; i < 4; i++ {
		if _, err := runtime.recordCommand("go test ./...", execution.CommandTest, &fail, execution.TerminalExited, nil, execution.ProvenanceRuntime, now, now); err != nil {
			t.Fatal(err)
		}
	}
	g := testGovernor(t, workWindow{rounds: 50}, runtime)
	action, err := g.AfterToolRound(context.Background(), governorRound("file_read", `{"path":"a.go"}`))
	if err != nil || action.Steer != "" {
		t.Fatalf("steer on prior evidence: %q, %v", action.Steer, err)
	}
}
