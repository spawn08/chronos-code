package orchestrator

import (
	"errors"
	"sort"

	"github.com/spawn08/chronos-code/internal/apierror"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/engine/model"
)

type EnvelopeMetadata struct {
	TenantID      string
	CorrelationID string
}

func ExecutionEnvelope(result ExecutionResult, err error, metadata EnvelopeMetadata) execution.ExecutionEnvelope {
	reason := result.StopReason
	if reason == "" || reason == execution.StopSuccess && err != nil {
		reason = execution.StopReasonForError(err)
	}
	usage := result.Usage
	content := ""
	if result.Response != nil {
		content = result.Response.Content
		if zeroUsage(usage) {
			usage = result.Response.Usage
		}
	}
	paths := append([]string{}, result.ChangedPaths...)
	sort.Strings(paths)
	envelope := execution.ExecutionEnvelope{
		SchemaVersion: execution.SchemaVersionV1, TaskID: result.TaskID, SessionID: result.SessionID,
		TenantID: metadata.TenantID, AgentID: result.AgentID, CorrelationID: metadata.CorrelationID,
		Status: statusFor(reason, err), StopReason: reason, Content: content,
		Usage: usageEnvelope(usage), CostMicrodollars: result.CostMicrodollars,
		ChangedPaths: paths, Verification: verificationEnvelope(result.Verification),
	}
	if envelope.Status != execution.StatusSucceeded {
		if err == nil {
			err = errors.New(string(reason))
		}
		envelope.Error = errorEnvelope(reason, err)
	}
	return envelope
}

func ApplyCompletion(result ExecutionResult, completion ExecutionCompletion) ExecutionResult {
	result.Verification = completion.Verification
	result.Budget = completion.Budget
	result.StopReason = completion.StopReason
	result.ChangedPaths = append([]string(nil), completion.ChangedPaths...)
	result.EvidenceIDs = append([]execution.EvidenceID(nil), completion.EvidenceIDs...)
	result.Usage = completion.Usage
	result.CostMicrodollars = completion.CostMicrodollars
	return result
}

func statusFor(reason execution.StopReason, err error) execution.Status {
	if err == nil && (reason == "" || reason == execution.StopSuccess) {
		return execution.StatusSucceeded
	}
	var classified *apierror.Classified
	if errors.As(err, &classified) {
		switch classified.Category {
		case apierror.CategoryInvalidRequest, apierror.CategoryRequestTooLarge, apierror.CategoryContextLength, apierror.CategoryNotFound, apierror.CategoryContentFilter:
			return execution.StatusInvalidRequest
		}
	}
	switch reason {
	case execution.StopInvalidRequest:
		return execution.StatusInvalidRequest
	case execution.StopPolicyDenied:
		return execution.StatusApprovalBlocked
	case execution.StopProviderRetryable:
		return execution.StatusRetryableProvider
	case execution.StopTimeout:
		return execution.StatusTimedOut
	case execution.StopBudgetExhausted:
		return execution.StatusBudgetExhausted
	case execution.StopVerificationFailed, execution.StopRepeatedFailure:
		return execution.StatusVerificationFailed
	case execution.StopCancelled:
		return execution.StatusCancelled
	default:
		return execution.StatusFailed
	}
}

func errorEnvelope(reason execution.StopReason, err error) *execution.Error {
	result := &execution.Error{Code: execution.ErrorInternal, Category: execution.ErrorCategoryInternal, Message: err.Error()}
	var terminal *execution.TerminalError
	if errors.As(err, &terminal) {
		result.Retryable = terminal.Retryable
	}
	var classified *apierror.Classified
	if errors.As(err, &classified) {
		result.Retryable = classified.Retryable
		switch classified.Category {
		case apierror.CategoryInvalidRequest, apierror.CategoryRequestTooLarge, apierror.CategoryContextLength, apierror.CategoryNotFound, apierror.CategoryContentFilter:
			result.Code, result.Category = execution.ErrorInvalidRequest, execution.ErrorCategoryRequest
			return result
		}
	}
	switch reason {
	case execution.StopInvalidRequest:
		result.Code, result.Category = execution.ErrorInvalidRequest, execution.ErrorCategoryRequest
	case execution.StopPolicyDenied:
		result.Code, result.Category = execution.ErrorApprovalRequired, execution.ErrorCategoryPolicy
	case execution.StopProviderRetryable:
		result.Code, result.Category, result.Retryable = execution.ErrorProviderRetryable, execution.ErrorCategoryProvider, true
	case execution.StopTimeout:
		result.Code, result.Category, result.Retryable = execution.ErrorTimeout, execution.ErrorCategoryTimeout, true
	case execution.StopBudgetExhausted:
		result.Code, result.Category = execution.ErrorBudgetExhausted, execution.ErrorCategoryBudget
	case execution.StopVerificationFailed, execution.StopRepeatedFailure:
		result.Code, result.Category = execution.ErrorVerificationFailed, execution.ErrorCategoryVerification
	case execution.StopAuthentication:
		result.Code, result.Category = execution.ErrorAuthentication, execution.ErrorCategoryAuthentication
	case execution.StopCancelled:
		result.Code, result.Category = execution.ErrorCancelled, execution.ErrorCategoryCancellation
	}
	return result
}

func usageEnvelope(usage model.Usage) execution.Usage {
	return execution.Usage{
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		CacheReadTokens: usage.CacheReadTokens, CacheCreationTokens: usage.CacheCreationTokens,
	}
}

func verificationEnvelope(decision verification.Decision) execution.EnvelopeVerification {
	status := execution.VerificationSatisfied
	if decision.Disagreement {
		status = execution.VerificationFailed
	} else if !decision.Allowed {
		status = execution.VerificationPending
	}
	result := execution.EnvelopeVerification{Status: status, Allowed: decision.Allowed, Obligations: make([]execution.VerificationObligation, 0, len(decision.Obligations))}
	for _, obligation := range decision.Obligations {
		result.Obligations = append(result.Obligations, execution.VerificationObligation{
			ID: obligation.ID, Kind: string(obligation.Kind), Status: string(obligation.Status),
			Command: obligation.Command, Paths: append([]string{}, obligation.Paths...),
		})
	}
	return result
}

func zeroUsage(usage model.Usage) bool {
	return usage.PromptTokens == 0 && usage.CompletionTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0
}
