package orchestrator

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/execution"
)

// futilityDetector recognises work that is going nowhere, round by round,
// so the governor can steer the model at once instead of letting it spend a
// whole idle window. It never limits how long work runs: long runs that keep
// learning, changing files or passing new checks are never flagged.
//
// The detectors match patterns seen in real sessions: one log read 14 times
// while a background job ran, `cargo test` re-run four times on an unchanged
// tree, and the same failing check retried without a change of approach.
type futilityDetector struct {
	// calls counts identical calls (name+arguments) since the last file
	// content change.
	calls map[string]int
	// lastNotice holds the round of the last steer per finding, so one
	// finding is raised once per state, not every round.
	lastNotice map[string]int
	// checks counts identical verification results (command, exit code)
	// with no file change in between.
	checks map[string]int
	// failures counts consecutive failures of each check command.
	failures map[string]int
	// eventsSeen is how much of the ledger was processed.
	eventsSeen int
	round      int
}

func newFutilityDetector() *futilityDetector {
	return &futilityDetector{
		calls: map[string]int{}, lastNotice: map[string]int{},
		checks: map[string]int{}, failures: map[string]int{},
	}
}

// seed skips ledger entries recorded before this detector started.
func (d *futilityDetector) seed(eventCount int) { d.eventsSeen = eventCount }

// Thresholds: how many identical repetitions without any state change
// before the model is steered.
const (
	repeatedCallThreshold   = 3
	repeatedCheckThreshold  = 2
	repeatedFailureAttempts = 3
)

// observe processes one round and the ledger events appended since the
// last round, returning a steer for the model, or "" when nothing futile is
// happening.
func (d *futilityDetector) observe(calls []model.ToolCall, events []execution.Event) string {
	d.round++
	if d.writesIn(events) {
		// New file content resets repetition: the same read or test after an
		// edit is a legitimate re-check.
		d.calls = map[string]int{}
		d.checks = map[string]int{}
		d.lastNotice = map[string]int{}
	}
	var findings []string
	for _, call := range calls {
		key := call.Name + "\x00" + canonicalArgs(call.Arguments)
		d.calls[key]++
		if n := d.calls[key]; n >= repeatedCallThreshold && d.notice("call:"+key) {
			findings = append(findings, repeatedCallSteer(call, n))
		}
	}
	findings = append(findings, d.checkFindings(events)...)
	if len(findings) == 0 {
		return ""
	}
	return "Futility check: " + strings.Join(findings, " ")
}

// writesIn reports whether events include a file or workspace change.
func (d *futilityDetector) writesIn(events []execution.Event) bool {
	for _, event := range events {
		if event.Type == execution.EventWrite {
			return true
		}
	}
	return false
}

// checkFindings flags a check re-run on an unchanged tree and a check that
// keeps failing across fix attempts.
func (d *futilityDetector) checkFindings(events []execution.Event) []string {
	var findings []string
	for _, event := range events {
		if event.Type != execution.EventVerification || event.Command == "" {
			continue
		}
		exit := "?"
		if event.ExitCode != nil {
			exit = strconv.Itoa(*event.ExitCode)
		}
		key := event.Command + "\x00" + exit
		d.checks[key]++
		if n := d.checks[key]; n >= repeatedCheckThreshold && d.notice("check:"+key) {
			findings = append(findings, fmt.Sprintf("`%s` has now run %d times with the same result (exit %s) and no file change in between; its result will not change until the code does.", event.Command, n, exit))
		}
		if event.Passed {
			delete(d.failures, event.Command)
			continue
		}
		d.failures[event.Command]++
		if n := d.failures[event.Command]; n >= repeatedFailureAttempts && d.notice(fmt.Sprintf("fail:%s:%d", event.Command, n)) {
			findings = append(findings, fmt.Sprintf("`%s` has failed %d times in a row. Before another attempt, state the failure's root cause from its output, what the previous attempts changed, and why they did not fix it; then take a different approach, or report the failure if it predates your change.", event.Command, n))
		}
	}
	return findings
}

// notice reports whether finding should be raised now: once per distinct
// finding until state changes clear it.
func (d *futilityDetector) notice(finding string) bool {
	if _, seen := d.lastNotice[finding]; seen {
		return false
	}
	d.lastNotice[finding] = d.round
	return true
}

func repeatedCallSteer(call model.ToolCall, n int) string {
	target := callTarget(call)
	switch call.Name {
	case "file_read":
		if isLogLike(target) {
			return fmt.Sprintf("you have read %s %d times with no change in between. If a background command is writing it, use await_shell on that shell_id (with a pattern) instead of re-reading.", target, n)
		}
		return fmt.Sprintf("you have read %s %d times with no change in between; its content is the same as before. Use what you already read.", target, n)
	case "shell":
		if strings.Contains(target, "sleep") {
			return fmt.Sprintf("the command `%s` has run %d times. Do not poll with sleep: start long work with block_until_ms 0 and use await_shell.", target, n)
		}
		return fmt.Sprintf("`%s` has run %d times with identical arguments and no file change in between, so it will return the same result.", target, n)
	case "await_shell":
		return fmt.Sprintf("you have waited on %s %d times. If its idle_ms keeps growing with no new output, it may be hung or waiting for input: decide whether to keep waiting, kill it, or change approach.", target, n)
	}
	return fmt.Sprintf("%s has been called %d times with identical arguments and nothing changed in between; repeat it only if you expect a different result.", call.Name, n)
}

// callTarget is the most recognisable argument of a call, for steer text.
func callTarget(call model.ToolCall) string {
	var args map[string]any
	if json.Unmarshal([]byte(call.Arguments), &args) != nil {
		return call.Name
	}
	for _, key := range []string{"path", "command", "shell_id", "pattern", "query", "symbol"} {
		if v, ok := args[key].(string); ok && v != "" {
			if len(v) > 120 {
				v = v[:120] + "…"
			}
			return v
		}
	}
	return call.Name
}

func isLogLike(p string) bool {
	switch strings.ToLower(path.Ext(p)) {
	case ".log", ".out", ".txt":
		return true
	}
	return strings.Contains(strings.ToLower(p), "log")
}

// canonicalArgs normalises JSON argument order so the same call with
// reordered keys counts as a repeat.
func canonicalArgs(arguments string) string {
	var v any
	if json.Unmarshal([]byte(arguments), &v) != nil {
		return arguments
	}
	return canonicalJSON(v)
}

func canonicalJSON(v any) string {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = strconv.Quote(k) + ":" + canonicalJSON(t[k])
		}
		return "{" + strings.Join(parts, ",") + "}"
	case []any:
		parts := make([]string, len(t))
		for i := range t {
			parts[i] = canonicalJSON(t[i])
		}
		return "[" + strings.Join(parts, ",") + "]"
	default:
		b, _ := json.Marshal(t)
		return string(b)
	}
}
