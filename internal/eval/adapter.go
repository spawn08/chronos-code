package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
)

// EnvelopeRecord is the production execution contract plus observations that
// are not represented in the terminal envelope itself.
type EnvelopeRecord struct {
	Envelope       execution.ExecutionEnvelope
	Events         []execution.EventEnvelope
	Calls          []Call
	RetryAttempts  int
	RepairAttempts int
}

type EnvelopeExecutor func(context.Context, TaskExecution) (EnvelopeRecord, error)

// ChronosCodeAdapter converts ExecutionEnvelope v1 into evaluator records.
// It does not automate or claim parity with any external coding agent.
type ChronosCodeAdapter struct {
	Execute EnvelopeExecutor
}

func (a ChronosCodeAdapter) Run(ctx context.Context, task TaskExecution) (TaskExecutionResult, error) {
	if a.Execute == nil {
		return TaskExecutionResult{}, fmt.Errorf("eval: Chronos Code adapter requires an executor")
	}
	record, err := a.Execute(ctx, task)
	if err != nil {
		return TaskExecutionResult{}, err
	}
	if record.Envelope.SchemaVersion != execution.SchemaVersionV1 {
		return TaskExecutionResult{}, fmt.Errorf("eval: unsupported execution envelope %q", record.Envelope.SchemaVersion)
	}
	if len(record.Events) > 0 {
		if err := execution.ValidateEventOrder(record.Events); err != nil {
			return TaskExecutionResult{}, fmt.Errorf("eval: invalid execution event stream: %w", err)
		}
	}

	completedAt := time.Now().UTC()
	result := TaskExecutionResult{
		TerminalStatus: string(record.Envelope.Status), CostMicrodollars: record.Envelope.CostMicrodollars,
		RetryAttempts: record.RetryAttempts, RepairAttempts: record.RepairAttempts,
		Grader:         GraderOutcome{Passed: record.Envelope.Status == execution.StatusSucceeded},
		LatestChangeAt: completedAt,
	}
	result.Calls = append(result.Calls, record.Calls...)
	deriveCallsFromEvents := len(record.Calls) == 0
	for _, event := range record.Events {
		result.Events = append(result.Events, TaskEvent{ID: fmt.Sprintf("event-%d", event.Sequence), Name: string(event.Type)})
		switch event.Type {
		case execution.EventTool:
			var payload execution.ToolPayload
			if deriveCallsFromEvents && json.Unmarshal(event.Payload, &payload) == nil {
				if payload.ID == "" {
					payload.ID = fmt.Sprintf("tool-%d", event.Sequence)
				}
				result.Calls = append(result.Calls, Call{ID: payload.ID, Kind: CallTool})
			}
		case execution.EventUsage:
			var usage execution.Usage
			if deriveCallsFromEvents && json.Unmarshal(event.Payload, &usage) == nil {
				result.Calls = append(result.Calls, Call{ID: fmt.Sprintf("model-%d", event.Sequence), Kind: CallModel, Usage: envelopeUsage(usage, 0)})
			}
		case execution.EventRetry:
			var payload execution.RetryPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Attempts > result.RetryAttempts {
				result.RetryAttempts = payload.Attempts
			}
		}
	}
	if !hasModelCall(result.Calls) {
		result.Calls = append(result.Calls, Call{ID: "model-terminal", Kind: CallModel, Usage: envelopeUsage(record.Envelope.Usage, record.Envelope.CostMicrodollars)})
	} else if len(result.Calls) > 0 {
		// Cost is terminal-envelope data and is attributed once.
		for i := range result.Calls {
			if result.Calls[i].Kind == CallModel {
				result.Calls[i].Usage.CostUSD = float64(record.Envelope.CostMicrodollars) / 1_000_000
				break
			}
		}
	}
	for _, obligation := range record.Envelope.Verification.Obligations {
		if obligation.Command == "" {
			continue
		}
		result.Verification = append(result.Verification, VerificationEvidence{
			ID: obligation.ID, Command: obligation.Command, Passed: obligation.Status == "satisfied", ExecutedAt: completedAt,
		})
	}
	if record.Envelope.Error != nil {
		result.Failure = &Failure{Class: FailureExecution, Message: record.Envelope.Error.Message}
	} else if record.Envelope.Status != execution.StatusSucceeded {
		result.Failure = &Failure{Class: FailureExecution, Message: "Chronos Code terminal status: " + string(record.Envelope.Status)}
	}
	if task.Workspace != "" {
		patch, err := gitDiff(context.Background(), task.Workspace)
		if err != nil {
			return TaskExecutionResult{}, fmt.Errorf("eval: collect Chronos Code patch: %w", err)
		}
		result.Patch = patch
	}
	return result, nil
}

func envelopeUsage(usage execution.Usage, cost int64) Usage {
	return Usage{InputTokens: usage.PromptTokens, OutputTokens: usage.CompletionTokens, CacheTokens: usage.CacheReadTokens + usage.CacheCreationTokens, CostUSD: float64(cost) / 1_000_000}
}

func hasModelCall(calls []Call) bool {
	for _, call := range calls {
		if call.Kind == CallModel {
			return true
		}
	}
	return false
}
