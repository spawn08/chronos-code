package observability

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/retention"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/engine/hooks"
)

func TestRegistryPublishesRequiredMetricsWithoutIdentifiers(t *testing.T) {
	r := NewRegistry()
	r.WorkQueued(1)
	r.WorkActive(1)
	r.WorkQueued(-1)
	r.ProviderCall("provider-a", time.Second, true, 1)
	r.ToolFailure("shell")
	r.TaskOutcome(execution.BudgetSnapshot{ModelCalls: 2, Tokens: 12}, verification.Decision{Allowed: false}, execution.StopVerificationFailed)
	r.Cleanup(retention.Result{ReclaimedItems: 2, ReclaimedBytes: 9, Statuses: []retention.ScopeStatus{{Scope: "sessions", Bytes: 20}}}, errors.New("cleanup failed"))
	var output bytes.Buffer
	if err := r.WritePrometheus(&output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"chronos_code_work_active", "chronos_code_work_queued", "chronos_code_provider_latency_seconds_sum", "chronos_code_provider_retries_total", "chronos_code_tool_failures_total", "chronos_code_task_budget_consumed_total", "chronos_code_task_budget_exhaustions_total", "chronos_code_verification_outcomes_total", "chronos_code_mcp_health", "chronos_code_cleanup_runs_total", "chronos_code_disk_use_bytes"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("metrics missing %s", want)
		}
	}
	for _, forbidden := range []string{"request-1", "session-1", "prompt text", "cleanup failed"} {
		if strings.Contains(output.String(), forbidden) {
			t.Errorf("metrics disclosed %q", forbidden)
		}
	}
}

func TestHookRecordsRetryMetadataWithoutPayload(t *testing.T) {
	r := NewRegistry()
	h := NewHook(r)
	event := &hooks.Event{Type: hooks.EventModelCallBefore, Name: "provider-a", Input: "secret", Metadata: map[string]any{"correlation_id": "call-1"}}
	_ = h.Before(context.TODO(), event)
	event.Type = hooks.EventModelCallAfter
	event.Metadata["retry_count"] = 1
	_ = h.After(context.TODO(), event)
	var output bytes.Buffer
	_ = r.WritePrometheus(&output)
	if !strings.Contains(output.String(), `chronos_code_provider_retries_total{provider="provider-a"} 1`) {
		t.Fatalf("metrics=%s", output.String())
	}
	if strings.Contains(output.String(), "secret") {
		t.Fatal("metrics contained model payload")
	}
}
