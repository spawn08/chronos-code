// Package observability provides bounded, payload-free operational telemetry.
package observability

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/mcpdiscover"
	"github.com/spawn08/chronos-code/internal/retention"
	"github.com/spawn08/chronos-code/internal/verification"
)

// Registry is a process-local Prometheus text registry. Labels are restricted
// to bounded enums and configured provider/tool/MCP names; request identifiers
// and user-controlled payloads are deliberately excluded.
type Registry struct {
	mu       sync.Mutex
	active   int64
	queued   int64
	counters map[string]float64
	gauges   map[string]float64
}

func NewRegistry() *Registry {
	return &Registry{counters: make(map[string]float64), gauges: make(map[string]float64)}
}

func (r *Registry) WorkQueued(delta int64) {
	r.mu.Lock()
	r.queued += delta
	r.mu.Unlock()
}

func (r *Registry) WorkActive(delta int64) {
	r.mu.Lock()
	r.active += delta
	r.mu.Unlock()
}

func (r *Registry) ProviderCall(provider string, elapsed time.Duration, failed bool, retries int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	labels := labels("provider", provider)
	r.counters["chronos_code_provider_requests_total"+labels]++
	r.counters["chronos_code_provider_latency_seconds_sum"+labels] += elapsed.Seconds()
	r.counters["chronos_code_provider_latency_seconds_count"+labels]++
	if failed {
		r.counters["chronos_code_provider_failures_total"+labels]++
	}
	if retries > 0 {
		r.counters["chronos_code_provider_retries_total"+labels] += float64(retries)
	}
}

func (r *Registry) ToolFailure(tool string) {
	r.add("chronos_code_tool_failures_total"+labels("tool", tool), 1)
}

func (r *Registry) TaskOutcome(b execution.BudgetSnapshot, decision verification.Decision, reason execution.StopReason) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.counters["chronos_code_tasks_total"+labels("stop_reason", string(reason))]++
	verificationOutcome := "satisfied"
	if decision.Disagreement {
		verificationOutcome = "unmet"
	} else if len(decision.Obligations) == 0 {
		verificationOutcome = "not_required"
	}
	r.counters["chronos_code_verification_outcomes_total"+labels("outcome", verificationOutcome)]++
	values := map[string]float64{
		"repair_attempts": float64(b.RepairAttempts), "model_calls": float64(b.ModelCalls),
		"tool_calls": float64(b.ToolCalls), "tokens": float64(b.Tokens),
		"cost_microdollars": float64(b.CostMicrodollars), "wall_time_seconds": b.Elapsed.Seconds(),
	}
	for dimension, value := range values {
		r.counters["chronos_code_task_budget_consumed_total"+labels("dimension", dimension)] += value
	}
	if b.ExhaustedDimension != "" {
		r.counters["chronos_code_task_budget_exhaustions_total"+labels("dimension", string(b.ExhaustedDimension))]++
	}
}

func (r *Registry) SetMCP(statuses []mcpdiscover.ServerStatus) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key := range r.gauges {
		if strings.HasPrefix(key, "chronos_code_mcp_health") {
			delete(r.gauges, key)
		}
	}
	for _, status := range statuses {
		value := 0.0
		if status.State == mcpdiscover.StateConnected {
			value = 1
		}
		r.gauges["chronos_code_mcp_health"+labels("agent", status.Agent, "server", status.Name, "state", string(status.State))] = value
	}
}

func (r *Registry) Cleanup(result retention.Result, err error) {
	outcome := "success"
	if err != nil {
		outcome = "failure"
	}
	r.add("chronos_code_cleanup_runs_total"+labels("outcome", outcome, "dry_run", strconv.FormatBool(result.DryRun)), 1)
	r.add("chronos_code_cleanup_reclaimed_items_total", float64(result.ReclaimedItems))
	r.add("chronos_code_cleanup_reclaimed_bytes_total", float64(result.ReclaimedBytes))
	for _, status := range result.Statuses {
		r.set("chronos_code_disk_use_bytes"+labels("scope", status.Scope), float64(status.Bytes))
	}
}

func (r *Registry) SetDiskUse(scope string, bytes int64) {
	r.set("chronos_code_disk_use_bytes"+labels("scope", scope), float64(bytes))
}

func (r *Registry) WritePrometheus(w io.Writer) error {
	r.mu.Lock()
	active, queued := r.active, r.queued
	values := make(map[string]float64, len(r.counters)+len(r.gauges)+2)
	for key, value := range r.counters {
		values[key] = value
	}
	for key, value := range r.gauges {
		values[key] = value
	}
	r.mu.Unlock()
	values["chronos_code_work_active"] = float64(active)
	values["chronos_code_work_queued"] = float64(queued)
	// Keep required series discoverable before their first event.
	for _, name := range []string{"chronos_code_provider_latency_seconds_sum", "chronos_code_provider_latency_seconds_count", "chronos_code_provider_retries_total", "chronos_code_tool_failures_total", "chronos_code_task_budget_consumed_total", "chronos_code_task_budget_exhaustions_total", "chronos_code_verification_outcomes_total", "chronos_code_mcp_health", "chronos_code_cleanup_runs_total", "chronos_code_disk_use_bytes"} {
		if _, ok := values[name]; !ok {
			values[name] = 0
		}
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := fmt.Fprintf(w, "%s %s\n", key, strconv.FormatFloat(values[key], 'f', -1, 64)); err != nil {
			return err
		}
	}
	return nil
}

func (r *Registry) add(key string, value float64) {
	r.mu.Lock()
	r.counters[key] += value
	r.mu.Unlock()
}
func (r *Registry) set(key string, value float64) { r.mu.Lock(); r.gauges[key] = value; r.mu.Unlock() }

func labels(values ...string) string {
	if len(values) == 0 {
		return ""
	}
	parts := make([]string, 0, len(values)/2)
	for i := 0; i+1 < len(values); i += 2 {
		parts = append(parts, values[i]+"="+strconv.Quote(strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, values[i+1])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
