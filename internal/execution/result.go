package execution

import (
	"context"
	"errors"
	"fmt"
)

type StopReason string

const (
	StopSuccess            StopReason = "success"
	StopVerificationFailed StopReason = "verification_failed"
	StopRepeatedFailure    StopReason = "repeated_failure"
	StopPolicyDenied       StopReason = "policy_denied"
	StopAuthentication     StopReason = "authentication_failed"
	StopInvalidRequest     StopReason = "invalid_request"
	StopProviderRetryable  StopReason = "provider_retryable"
	StopTimeout            StopReason = "timeout"
	StopCancelled          StopReason = "cancelled"
	StopBudgetExhausted    StopReason = "budget_exhausted"
	StopInternalError      StopReason = "internal_error"
)

type TerminalError struct {
	Reason    StopReason
	Retryable bool
	Err       error
}

func (e *TerminalError) Error() string {
	if e.Err == nil {
		return string(e.Reason)
	}
	return fmt.Sprintf("%s: %v", e.Reason, e.Err)
}

func (e *TerminalError) Unwrap() error { return e.Err }

func StopReasonForError(err error) StopReason {
	if err == nil {
		return StopSuccess
	}
	var terminal *TerminalError
	if errors.As(err, &terminal) {
		return terminal.Reason
	}
	if errors.Is(err, ErrTaskBudgetExceeded) {
		return StopBudgetExhausted
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return StopTimeout
	}
	if errors.Is(err, context.Canceled) {
		return StopCancelled
	}
	var reasoner interface{ ExecutionStopReason() string }
	if errors.As(err, &reasoner) {
		return StopReason(reasoner.ExecutionStopReason())
	}
	return StopInternalError
}
