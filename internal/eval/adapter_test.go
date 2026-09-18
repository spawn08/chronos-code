package eval

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
)

func TestChronosCodeAdapterCollectsEnvelopeAndEventMetrics(t *testing.T) {
	now := time.Date(2026, 9, 18, 12, 0, 0, 0, time.UTC)
	usagePayload, _ := json.Marshal(execution.Usage{PromptTokens: 10, CompletionTokens: 4, CacheReadTokens: 2})
	toolPayload, _ := json.Marshal(execution.ToolPayload{ID: "tool-1", Name: "file_write"})
	retryPayload, _ := json.Marshal(execution.RetryPayload{Attempts: 2})
	terminalPayload, _ := json.Marshal(map[string]string{"status": "succeeded"})
	events := []execution.EventEnvelope{
		{SchemaVersion: execution.SchemaVersionV1, Sequence: 1, Timestamp: now, Type: execution.EventUsage, TaskID: "task", Payload: usagePayload},
		{SchemaVersion: execution.SchemaVersionV1, Sequence: 2, Timestamp: now, Type: execution.EventTool, TaskID: "task", Payload: toolPayload},
		{SchemaVersion: execution.SchemaVersionV1, Sequence: 3, Timestamp: now, Type: execution.EventRetry, TaskID: "task", Payload: retryPayload},
		{SchemaVersion: execution.SchemaVersionV1, Sequence: 4, Timestamp: now, Type: execution.EventCompletion, TaskID: "task", Payload: terminalPayload},
	}
	adapter := ChronosCodeAdapter{Execute: func(context.Context, TaskExecution) (EnvelopeRecord, error) {
		return EnvelopeRecord{Envelope: execution.ExecutionEnvelope{
			SchemaVersion: execution.SchemaVersionV1, Status: execution.StatusSucceeded,
			Usage: execution.Usage{PromptTokens: 10, CompletionTokens: 4, CacheReadTokens: 2}, CostMicrodollars: 25,
			Verification: execution.EnvelopeVerification{Obligations: []execution.VerificationObligation{{ID: "test", Command: "go test ./...", Status: "satisfied"}}},
		}, Events: events, RepairAttempts: 1}, nil
	}}

	got, err := adapter.Run(context.Background(), TaskExecution{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.TerminalStatus != "succeeded" || got.RetryAttempts != 2 || got.RepairAttempts != 1 || got.CostMicrodollars != 25 {
		t.Fatalf("metrics = %+v", got)
	}
	if len(got.Calls) != 2 || got.Calls[0].Kind != CallModel || got.Calls[1].Kind != CallTool || len(got.Verification) != 1 {
		t.Fatalf("calls/verification = %+v / %+v", got.Calls, got.Verification)
	}
	if got.Calls[0].Usage.InputTokens != 10 || got.Calls[0].Usage.CostUSD != 0.000025 {
		t.Fatalf("usage = %+v", got.Calls[0].Usage)
	}
}
