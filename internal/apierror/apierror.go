package apierror

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spawn08/chronos/engine/model"
)

// Category classifies an LLM API error into an actionable bucket.
type Category int

const (
	CategoryUnknown         Category = iota
	CategoryOverloaded               // 529 or "overloaded" in body
	CategoryRateLimited              // 429
	CategoryRequestTooLarge          // 413 or "request_too_large"
	CategoryContextLength            // 400 with context_length / token limit messages
	CategoryAuth                     // 401, 403
	CategoryNotFound                 // 404
	CategoryInvalidRequest           // 400 (generic)
	CategoryServerError              // 500, 502, 503, 504
	CategoryTimeout                  // request timeout, 408
	CategoryNetworkError             // connection refused, DNS, etc.
	CategoryCircuitOpen              // circuit breaker tripped
	CategoryContentFilter            // content policy / safety filter
)

func (c Category) String() string {
	switch c {
	case CategoryOverloaded:
		return "overloaded"
	case CategoryRateLimited:
		return "rate_limited"
	case CategoryRequestTooLarge:
		return "request_too_large"
	case CategoryContextLength:
		return "context_length"
	case CategoryAuth:
		return "auth_error"
	case CategoryNotFound:
		return "not_found"
	case CategoryInvalidRequest:
		return "invalid_request"
	case CategoryServerError:
		return "server_error"
	case CategoryTimeout:
		return "timeout"
	case CategoryNetworkError:
		return "network_error"
	case CategoryCircuitOpen:
		return "circuit_open"
	case CategoryContentFilter:
		return "content_filter"
	default:
		return "unknown"
	}
}

// Classified wraps an original error with its classification, a
// user-friendly message, and recovery metadata.
type Classified struct {
	Category   Category
	Message    string
	Retryable  bool
	RetryAfter time.Duration
	Original   error
}

func (c *Classified) Error() string { return c.Message }
func (c *Classified) Unwrap() error { return c.Original }

// Classify inspects err and returns a Classified error with a user-friendly
// message. If err is nil, returns nil.
func Classify(err error) *Classified {
	if err == nil {
		return nil
	}

	if errors.Is(err, model.ErrCircuitOpen) {
		return &Classified{
			Category:  CategoryCircuitOpen,
			Message:   "Provider is temporarily unavailable (circuit breaker open). Will retry shortly.",
			Retryable: true,
			Original:  err,
		}
	}

	var apiErr *model.APIError
	if errors.As(err, &apiErr) {
		return classifyAPIError(apiErr, err)
	}

	return classifyByMessage(err)
}

func classifyAPIError(apiErr *model.APIError, original error) *Classified {
	body := strings.ToLower(apiErr.Body)

	switch {
	case apiErr.StatusCode == 529 || strings.Contains(body, "overloaded"):
		return &Classified{
			Category:   CategoryOverloaded,
			Message:    "The model is currently overloaded. Retrying automatically...",
			Retryable:  true,
			RetryAfter: coalesce(apiErr.RetryAfter, 10*time.Second),
			Original:   original,
		}

	case apiErr.StatusCode == 429:
		var msg string
		if apiErr.RetryAfter > 0 {
			msg = fmt.Sprintf("Rate limited by the API. Retrying in %s...", apiErr.RetryAfter.Round(time.Second))
		} else {
			msg = "Rate limited by the API. Retrying automatically..."
		}
		return &Classified{
			Category:   CategoryRateLimited,
			Message:    msg,
			Retryable:  true,
			RetryAfter: coalesce(apiErr.RetryAfter, 5*time.Second),
			Original:   original,
		}

	case apiErr.StatusCode == 413 || strings.Contains(body, "request_too_large"):
		return &Classified{
			Category:  CategoryRequestTooLarge,
			Message:   "Request too large — the conversation exceeds the model's maximum input size. Compacting session...",
			Retryable: false,
			Original:  original,
		}

	case apiErr.StatusCode == 400 && isContextLengthError(body):
		return &Classified{
			Category:  CategoryContextLength,
			Message:   "Context length exceeded — the conversation is too long for this model. Compacting session...",
			Retryable: false,
			Original:  original,
		}

	case apiErr.StatusCode == 400 && isContentFilterError(body):
		return &Classified{
			Category:  CategoryContentFilter,
			Message:   "Request blocked by content safety filter.",
			Retryable: false,
			Original:  original,
		}

	case apiErr.StatusCode == 400:
		detail := extractErrorMessage(apiErr.Body)
		msg := "Invalid request sent to the API."
		if detail != "" {
			msg = fmt.Sprintf("Invalid request: %s", detail)
		}
		return &Classified{
			Category:  CategoryInvalidRequest,
			Message:   msg,
			Retryable: false,
			Original:  original,
		}

	case apiErr.StatusCode == 401 || apiErr.StatusCode == 403:
		msg := "Authentication failed. Check your API key or run /login <provider> <key>."
		if apiErr.StatusCode == 403 {
			detail := extractErrorMessage(apiErr.Body)
			if detail == "" {
				detail = strings.TrimSpace(apiErr.Body)
			}
			if detail != "" {
				msg = fmt.Sprintf("Access denied: %s", detail)
			} else {
				msg = "Access denied. Your API key may lack the required permissions."
			}
		}
		return &Classified{
			Category:  CategoryAuth,
			Message:   msg,
			Retryable: false,
			Original:  original,
		}

	case apiErr.StatusCode == 404:
		detail := extractErrorMessage(apiErr.Body)
		msg := "Model or endpoint not found."
		if detail != "" {
			msg = fmt.Sprintf("Not found: %s", detail)
		}
		return &Classified{
			Category:  CategoryNotFound,
			Message:   msg,
			Retryable: false,
			Original:  original,
		}

	case apiErr.StatusCode == 408:
		return &Classified{
			Category:   CategoryTimeout,
			Message:    "Request timed out. Retrying...",
			Retryable:  true,
			RetryAfter: 2 * time.Second,
			Original:   original,
		}

	case apiErr.StatusCode >= 500:
		name := serverErrorName(apiErr.StatusCode)
		return &Classified{
			Category:   CategoryServerError,
			Message:    fmt.Sprintf("Server error (%s). Retrying automatically...", name),
			Retryable:  true,
			RetryAfter: coalesce(apiErr.RetryAfter, 5*time.Second),
			Original:   original,
		}
	}

	return &Classified{
		Category:  CategoryUnknown,
		Message:   apiErr.Error(),
		Retryable: false,
		Original:  original,
	}
}

func classifyByMessage(err error) *Classified {
	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "overloaded"):
		return &Classified{
			Category:   CategoryOverloaded,
			Message:    "The model is currently overloaded. Retrying automatically...",
			Retryable:  true,
			RetryAfter: 10 * time.Second,
			Original:   err,
		}
	case strings.Contains(msg, "rate limit") || strings.Contains(msg, "rate_limit") || strings.Contains(msg, "too many requests"):
		return &Classified{
			Category:   CategoryRateLimited,
			Message:    "Rate limited by the API. Retrying automatically...",
			Retryable:  true,
			RetryAfter: 5 * time.Second,
			Original:   err,
		}
	case strings.Contains(msg, "request_too_large") || strings.Contains(msg, "request entity too large") || strings.Contains(msg, "413"):
		return &Classified{
			Category:  CategoryRequestTooLarge,
			Message:   "Request too large — the conversation exceeds the model's maximum input size. Compacting session...",
			Retryable: false,
			Original:  err,
		}
	case isContextLengthError(msg):
		return &Classified{
			Category:  CategoryContextLength,
			Message:   "Context length exceeded — the conversation is too long for this model. Compacting session...",
			Retryable: false,
			Original:  err,
		}
	case strings.Contains(msg, "authentication") || strings.Contains(msg, "unauthorized") || strings.Contains(msg, "invalid.*api.key"):
		return &Classified{
			Category:  CategoryAuth,
			Message:   "Authentication failed. Check your API key or run /login <provider> <key>.",
			Retryable: false,
			Original:  err,
		}
	case strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable") || strings.Contains(msg, "dns"):
		return &Classified{
			Category:   CategoryNetworkError,
			Message:    "Network error — cannot reach the API. Check your connection.",
			Retryable:  true,
			RetryAfter: 5 * time.Second,
			Original:   err,
		}
	case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded"):
		return &Classified{
			Category:   CategoryTimeout,
			Message:    "Request timed out. Retrying...",
			Retryable:  true,
			RetryAfter: 2 * time.Second,
			Original:   err,
		}
	case strings.Contains(msg, "content filter") || strings.Contains(msg, "content_policy") || strings.Contains(msg, "safety"):
		return &Classified{
			Category:  CategoryContentFilter,
			Message:   "Request blocked by content safety filter.",
			Retryable: false,
			Original:  err,
		}
	}

	// Unknown errors are not retried at the orchestrator level. The
	// httpClient already retries transport-level failures; retrying here
	// would multiply attempts unnecessarily.
	return &Classified{
		Category:  CategoryUnknown,
		Message:   err.Error(),
		Retryable: false,
		Original:  err,
	}
}

// IsCompactable returns true if the error category indicates the session
// should be compacted and retried (request too large or context length).
func IsCompactable(c *Classified) bool {
	return c != nil && (c.Category == CategoryRequestTooLarge || c.Category == CategoryContextLength)
}

func isContextLengthError(body string) bool {
	return strings.Contains(body, "context_length") ||
		strings.Contains(body, "context window") ||
		strings.Contains(body, "maximum context") ||
		strings.Contains(body, "token limit") ||
		strings.Contains(body, "max_tokens") && strings.Contains(body, "exceed") ||
		strings.Contains(body, "too many tokens")
}

func isContentFilterError(body string) bool {
	return strings.Contains(body, "content_filter") ||
		strings.Contains(body, "content_policy") ||
		strings.Contains(body, "safety") && strings.Contains(body, "block")
}

func extractErrorMessage(body string) string {
	body = strings.TrimSpace(body)
	for _, prefix := range []string{`"message":"`, `"message": "`} {
		if idx := strings.Index(strings.ToLower(body), strings.ToLower(prefix)); idx >= 0 {
			start := idx + len(prefix)
			if end := strings.Index(body[start:], `"`); end >= 0 {
				return body[start : start+end]
			}
		}
	}
	return ""
}

func serverErrorName(code int) string {
	switch code {
	case 500:
		return "500 Internal Server Error"
	case 502:
		return "502 Bad Gateway"
	case 503:
		return "503 Service Unavailable"
	case 504:
		return "504 Gateway Timeout"
	default:
		return fmt.Sprintf("%d", code)
	}
}

func coalesce(a, b time.Duration) time.Duration {
	if a > 0 {
		return a
	}
	return b
}
