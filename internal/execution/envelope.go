package execution

import (
	"encoding/json"
	"fmt"
	"time"
)

const SchemaVersionV1 = "chronos.execution.v1"

type Status string

const (
	StatusSucceeded          Status = "succeeded"
	StatusApprovalBlocked    Status = "approval_blocked"
	StatusInvalidRequest     Status = "invalid_request"
	StatusRetryableProvider  Status = "retryable_provider_error"
	StatusTimedOut           Status = "timed_out"
	StatusBudgetExhausted    Status = "budget_exhausted"
	StatusVerificationFailed Status = "verification_failed"
	StatusCancelled          Status = "cancelled"
	StatusFailed             Status = "failed"
)

type ErrorCode string

const (
	ErrorApprovalRequired   ErrorCode = "approval_required"
	ErrorInvalidRequest     ErrorCode = "invalid_request"
	ErrorProviderRetryable  ErrorCode = "provider_retryable"
	ErrorTimeout            ErrorCode = "timeout"
	ErrorBudgetExhausted    ErrorCode = "budget_exhausted"
	ErrorVerificationFailed ErrorCode = "verification_failed"
	ErrorAuthentication     ErrorCode = "authentication_failed"
	ErrorCancelled          ErrorCode = "cancelled"
	ErrorInternal           ErrorCode = "internal_error"
)

type ErrorCategory string

const (
	ErrorCategoryPolicy         ErrorCategory = "policy"
	ErrorCategoryRequest        ErrorCategory = "request"
	ErrorCategoryProvider       ErrorCategory = "provider"
	ErrorCategoryTimeout        ErrorCategory = "timeout"
	ErrorCategoryBudget         ErrorCategory = "budget"
	ErrorCategoryVerification   ErrorCategory = "verification"
	ErrorCategoryAuthentication ErrorCategory = "authentication"
	ErrorCategoryCancellation   ErrorCategory = "cancellation"
	ErrorCategoryInternal       ErrorCategory = "internal"
)

type Usage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	CacheReadTokens     int `json:"cache_read_tokens"`
	CacheCreationTokens int `json:"cache_creation_tokens"`
}

type VerificationStatus string

const (
	VerificationSatisfied VerificationStatus = "satisfied"
	VerificationFailed    VerificationStatus = "failed"
	VerificationPending   VerificationStatus = "pending"
)

type EnvelopeVerification struct {
	Status      VerificationStatus       `json:"status"`
	Allowed     bool                     `json:"allowed"`
	Obligations []VerificationObligation `json:"obligations"`
}

type VerificationObligation struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Status  string   `json:"status"`
	Command string   `json:"command,omitempty"`
	Paths   []string `json:"paths"`
}

type Error struct {
	Code      ErrorCode     `json:"code"`
	Category  ErrorCategory `json:"category"`
	Retryable bool          `json:"retryable"`
	Message   string        `json:"message"`
}

// ExecutionEnvelope is the transport-neutral terminal result contract.
type ExecutionEnvelope struct {
	SchemaVersion    string               `json:"schema_version"`
	TaskID           string               `json:"task_id"`
	SessionID        string               `json:"session_id"`
	TenantID         string               `json:"tenant_id"`
	AgentID          string               `json:"agent_id"`
	CorrelationID    string               `json:"correlation_id"`
	Status           Status               `json:"status"`
	StopReason       StopReason           `json:"stop_reason"`
	Content          string               `json:"content"`
	Usage            Usage                `json:"usage"`
	CostMicrodollars int64                `json:"cost_microdollars"`
	ChangedPaths     []string             `json:"changed_paths"`
	Verification     EnvelopeVerification `json:"verification"`
	Error            *Error               `json:"error,omitempty"`
}

type EnvelopeEventType string

const (
	EventContent            EnvelopeEventType = "content"
	EventTool               EnvelopeEventType = "tool"
	EventSubagent           EnvelopeEventType = "subagent"
	EventRetry              EnvelopeEventType = "retry"
	EventUsage              EnvelopeEventType = "usage"
	EventVerificationResult EnvelopeEventType = "verification"
	EventError              EnvelopeEventType = "error"
	EventCompletion         EnvelopeEventType = "completion"
)

// EventEnvelope is one ordered JSONL/SSE record. Error and completion are
// terminal event types; a stream must contain exactly one of them.
type EventEnvelope struct {
	SchemaVersion string            `json:"schema_version"`
	Sequence      uint64            `json:"sequence"`
	Timestamp     time.Time         `json:"timestamp"`
	Type          EnvelopeEventType `json:"type"`
	TaskID        string            `json:"task_id"`
	Payload       json.RawMessage   `json:"payload"`
}

type ContentPayload struct {
	Content string `json:"content"`
	Delta   bool   `json:"delta"`
}

type ToolPayload struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type SubagentPayload struct {
	AgentID string `json:"agent_id"`
	Status  string `json:"status"`
	Content string `json:"content,omitempty"`
}

type RetryPayload struct {
	Attempts int        `json:"attempts"`
	Reason   StopReason `json:"reason,omitempty"`
}

func NewEvent(sequence uint64, timestamp time.Time, eventType EnvelopeEventType, taskID string, payload any) (EventEnvelope, error) {
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return EventEnvelope{}, fmt.Errorf("marshal %s event payload: %w", eventType, err)
	}
	return EventEnvelope{
		SchemaVersion: SchemaVersionV1,
		Sequence:      sequence,
		Timestamp:     timestamp.UTC(),
		Type:          eventType,
		TaskID:        taskID,
		Payload:       encoded,
	}, nil
}

func (e EventEnvelope) Terminal() bool {
	return e.Type == EventError || e.Type == EventCompletion
}

// ValidateEventOrder enforces contiguous ordering and exactly one final
// terminal event for persisted JSONL streams and adapter parity tests.
func ValidateEventOrder(events []EventEnvelope) error {
	terminal := 0
	for i, event := range events {
		if event.SchemaVersion != SchemaVersionV1 {
			return fmt.Errorf("event %d has schema version %q", i, event.SchemaVersion)
		}
		if event.Sequence != uint64(i+1) {
			return fmt.Errorf("event %d has sequence %d, want %d", i, event.Sequence, i+1)
		}
		if event.Timestamp.IsZero() {
			return fmt.Errorf("event %d has no timestamp", i)
		}
		if event.Terminal() {
			terminal++
			if i != len(events)-1 {
				return fmt.Errorf("terminal event at sequence %d is not final", event.Sequence)
			}
		}
	}
	if terminal != 1 {
		return fmt.Errorf("event stream has %d terminal events, want 1", terminal)
	}
	return nil
}
