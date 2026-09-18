package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/cli"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/server"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/engine/model"
)

func TestExecutionEnvelopeV1FixtureAndAdapterParity(t *testing.T) {
	want := loadEnvelopeFixture(t)
	result, metadata := contractResult()
	blocking := orchestrator.ExecutionEnvelope(result, nil, metadata)
	if !reflect.DeepEqual(blocking, want) {
		t.Fatalf("blocking envelope = %#v, want %#v", blocking, want)
	}

	stream := make(chan *model.ChatResponse, 1)
	stream <- &model.ChatResponse{Content: "answer", Delta: true, Usage: model.Usage{PromptTokens: 7, CompletionTokens: 3, CacheReadTokens: 2, CacheCreationTokens: 1}}
	close(stream)
	completion := make(chan orchestrator.ExecutionCompletion, 1)
	completion <- orchestrator.ExecutionCompletion{
		Verification: result.Verification, StopReason: execution.StopSuccess,
		ChangedPaths: result.ChangedPaths, Usage: result.Usage, CostMicrodollars: result.CostMicrodollars,
	}
	close(completion)
	result.Response = nil
	result.Stream = stream
	result.Completion = completion
	recorder := httptest.NewRecorder()
	server.WriteExecutionEventStream(t.Context(), recorder, recorder, result, metadata)

	events := decodeSSEEvents(t, recorder.Body.String())
	if err := execution.ValidateEventOrder(events); err != nil {
		t.Fatalf("SSE event order: %v", err)
	}
	if events[len(events)-1].Type != execution.EventCompletion {
		t.Fatalf("terminal event = %q, want completion", events[len(events)-1].Type)
	}
	var terminal execution.ExecutionEnvelope
	if err := json.Unmarshal(events[len(events)-1].Payload, &terminal); err != nil {
		t.Fatalf("decode terminal payload: %v", err)
	}
	if !reflect.DeepEqual(terminal, want) {
		t.Fatalf("SSE terminal envelope = %#v, want blocking envelope %#v", terminal, want)
	}
}

func TestExecutionEventV1FixtureIsOrderedAndTerminal(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "execution_events_v1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var events []execution.EventEnvelope
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var event execution.EventEnvelope
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("decode JSONL event: %v", err)
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if err := execution.ValidateEventOrder(events); err != nil {
		t.Fatal(err)
	}

	wantTypes := map[execution.EnvelopeEventType]bool{
		execution.EventContent: true, execution.EventTool: true, execution.EventSubagent: true,
		execution.EventRetry: true, execution.EventUsage: true, execution.EventVerificationResult: true,
		execution.EventError: true, execution.EventCompletion: true,
	}
	if len(wantTypes) != 8 {
		t.Fatalf("event type contract has %d members, want 8", len(wantTypes))
	}
}

func TestExecutionEventOrderRejectsMultipleTerminals(t *testing.T) {
	now := time.Now()
	events := []execution.EventEnvelope{
		{SchemaVersion: execution.SchemaVersionV1, Sequence: 1, Timestamp: now, Type: execution.EventError},
		{SchemaVersion: execution.SchemaVersionV1, Sequence: 2, Timestamp: now, Type: execution.EventCompletion},
	}
	if err := execution.ValidateEventOrder(events); err == nil {
		t.Fatal("ValidateEventOrder accepted multiple terminal events")
	}
}

func TestExecutionSSEVerificationFailureHasOneErrorTerminal(t *testing.T) {
	stream := make(chan *model.ChatResponse, 1)
	stream <- &model.ChatResponse{Err: errors.New("verification does not support successful completion")}
	close(stream)
	completion := make(chan orchestrator.ExecutionCompletion, 1)
	completion <- orchestrator.ExecutionCompletion{
		Verification: verification.Decision{Disagreement: true},
		StopReason:   execution.StopVerificationFailed,
		Err:          &execution.TerminalError{Reason: execution.StopVerificationFailed, Err: errors.New("verification does not support successful completion")},
	}
	close(completion)
	result := orchestrator.ExecutionResult{TaskID: "task-1", SessionID: "session-1", AgentID: "coder", Stream: stream, Completion: completion}
	recorder := httptest.NewRecorder()
	server.WriteExecutionEventStream(t.Context(), recorder, recorder, result, orchestrator.EnvelopeMetadata{})

	if strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("verification failure stream contains legacy DONE marker: %q", recorder.Body.String())
	}
	events := decodeSSEEvents(t, recorder.Body.String())
	if err := execution.ValidateEventOrder(events); err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Type != execution.EventError {
		t.Fatalf("terminal type = %q, want error", events[len(events)-1].Type)
	}
}

func TestAutonomousExitCodeMapping(t *testing.T) {
	tests := map[execution.Status]int{
		execution.StatusSucceeded:          cli.ExitSuccess,
		execution.StatusApprovalBlocked:    cli.ExitApprovalBlocked,
		execution.StatusInvalidRequest:     cli.ExitInvalidRequest,
		execution.StatusRetryableProvider:  cli.ExitRetryableProvider,
		execution.StatusTimedOut:           cli.ExitTimeout,
		execution.StatusBudgetExhausted:    cli.ExitBudgetExhausted,
		execution.StatusVerificationFailed: cli.ExitVerificationFailure,
	}
	for status, want := range tests {
		if got := cli.ExitCodeForStatus(status); got != want {
			t.Errorf("ExitCodeForStatus(%q) = %d, want %d", status, got, want)
		}
	}
}

func loadEnvelopeFixture(t *testing.T) execution.ExecutionEnvelope {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", "execution_envelope_v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var envelope execution.ExecutionEnvelope
	if err := json.Unmarshal(contents, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func contractResult() (orchestrator.ExecutionResult, orchestrator.EnvelopeMetadata) {
	decision := verification.Decision{Allowed: true, Obligations: []verification.Obligation{{
		ID: "test:go test ./...", Kind: verification.KindTest, Status: verification.StatusSatisfied,
		Command: "go test ./...", Paths: []string{"main.go"},
	}}}
	return orchestrator.ExecutionResult{
		TaskID: "task-1", SessionID: "session-1", AgentID: "coder",
		Response: &model.ChatResponse{Content: "answer"}, StopReason: execution.StopSuccess,
		Usage:            model.Usage{PromptTokens: 7, CompletionTokens: 3, CacheReadTokens: 2, CacheCreationTokens: 1},
		CostMicrodollars: 22, ChangedPaths: []string{"main.go"}, Verification: decision,
	}, orchestrator.EnvelopeMetadata{TenantID: "tenant-1", CorrelationID: "request-1"}
}

func decodeSSEEvents(t *testing.T, body string) []execution.EventEnvelope {
	t.Helper()
	var events []execution.EventEnvelope
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		var data []byte
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") {
				data = []byte(strings.TrimPrefix(line, "data: "))
			}
		}
		if len(data) == 0 {
			continue
		}
		var event execution.EventEnvelope
		if err := json.NewDecoder(bytes.NewReader(data)).Decode(&event); err != nil {
			t.Fatalf("decode SSE event: %v", err)
		}
		events = append(events, event)
	}
	return events
}
