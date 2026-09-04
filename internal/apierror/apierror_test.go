package apierror

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
)

func TestClassifyNil(t *testing.T) {
	if got := Classify(nil); got != nil {
		t.Fatalf("Classify(nil) = %v, want nil", got)
	}
}

func TestClassifyAPIError(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantCat    Category
		retryable  bool
		compactable bool
	}{
		{
			name:      "429 rate limited",
			err:       fmt.Errorf("anthropic chat: %w", &model.APIError{StatusCode: 429, Status: "429 Too Many Requests", RetryAfter: 10 * time.Second}),
			wantCat:   CategoryRateLimited,
			retryable: true,
		},
		{
			name:      "529 overloaded",
			err:       fmt.Errorf("anthropic stream: %w", &model.APIError{StatusCode: 529, Status: "529 Overloaded", Body: `{"error":{"type":"overloaded_error","message":"Overloaded"}}`}),
			wantCat:   CategoryOverloaded,
			retryable: true,
		},
		{
			name:        "413 request too large",
			err:         fmt.Errorf("agent \"planner\" stream: %w", fmt.Errorf("anthropic stream: %w", &model.APIError{StatusCode: 413, Status: "413 Request Entity Too Large", Body: `{"error":{"type":"request_too_large","message":"Request exceeds the maximum size"}}`})),
			wantCat:     CategoryRequestTooLarge,
			retryable:   false,
			compactable: true,
		},
		{
			name:      "401 auth error",
			err:       fmt.Errorf("anthropic chat: %w", &model.APIError{StatusCode: 401, Status: "401 Unauthorized"}),
			wantCat:   CategoryAuth,
			retryable: false,
		},
		{
			name:      "403 forbidden",
			err:       fmt.Errorf("openai chat: %w", &model.APIError{StatusCode: 403, Status: "403 Forbidden"}),
			wantCat:   CategoryAuth,
			retryable: false,
		},
		{
			name:      "404 not found",
			err:       fmt.Errorf("anthropic chat: %w", &model.APIError{StatusCode: 404, Status: "404 Not Found", Body: `{"error":{"message":"model not found"}}`}),
			wantCat:   CategoryNotFound,
			retryable: false,
		},
		{
			name:        "400 context length",
			err:         fmt.Errorf("openai chat: %w", &model.APIError{StatusCode: 400, Body: `{"error":{"message":"This model's maximum context length is 128000 tokens"}}`}),
			wantCat:     CategoryContextLength,
			retryable:   false,
			compactable: true,
		},
		{
			name:      "400 generic",
			err:       fmt.Errorf("anthropic chat: %w", &model.APIError{StatusCode: 400, Body: `{"error":{"message":"invalid temperature value"}}`}),
			wantCat:   CategoryInvalidRequest,
			retryable: false,
		},
		{
			name:      "400 content filter",
			err:       fmt.Errorf("openai chat: %w", &model.APIError{StatusCode: 400, Body: `{"error":{"message":"content_policy_violation"}}`}),
			wantCat:   CategoryContentFilter,
			retryable: false,
		},
		{
			name:      "500 server error",
			err:       fmt.Errorf("anthropic chat: %w", &model.APIError{StatusCode: 500, Status: "500 Internal Server Error"}),
			wantCat:   CategoryServerError,
			retryable: true,
		},
		{
			name:      "502 bad gateway",
			err:       fmt.Errorf("anthropic chat: %w", &model.APIError{StatusCode: 502, Status: "502 Bad Gateway"}),
			wantCat:   CategoryServerError,
			retryable: true,
		},
		{
			name:      "503 service unavailable",
			err:       fmt.Errorf("anthropic chat: %w", &model.APIError{StatusCode: 503, Status: "503 Service Unavailable"}),
			wantCat:   CategoryServerError,
			retryable: true,
		},
		{
			name:      "408 timeout",
			err:       fmt.Errorf("anthropic chat: %w", &model.APIError{StatusCode: 408, Status: "408 Request Timeout"}),
			wantCat:   CategoryTimeout,
			retryable: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.err)
			if got == nil {
				t.Fatal("Classify returned nil")
			}
			if got.Category != tt.wantCat {
				t.Errorf("Category = %v, want %v", got.Category, tt.wantCat)
			}
			if got.Retryable != tt.retryable {
				t.Errorf("Retryable = %v, want %v", got.Retryable, tt.retryable)
			}
			if IsCompactable(got) != tt.compactable {
				t.Errorf("IsCompactable = %v, want %v", IsCompactable(got), tt.compactable)
			}
			if got.Message == "" {
				t.Error("Message is empty")
			}
		})
	}
}

func TestClassifyCircuitOpen(t *testing.T) {
	got := Classify(fmt.Errorf("http request: %w", model.ErrCircuitOpen))
	if got == nil {
		t.Fatal("Classify returned nil")
	}
	if got.Category != CategoryCircuitOpen {
		t.Errorf("Category = %v, want %v", got.Category, CategoryCircuitOpen)
	}
	if !got.Retryable {
		t.Error("circuit open should be retryable")
	}
}

func TestClassifyByMessage(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		wantCat Category
	}{
		{
			name:    "overloaded in message",
			err:     errors.New("anthropic stream: server overloaded, please try again"),
			wantCat: CategoryOverloaded,
		},
		{
			name:    "rate limit in message",
			err:     errors.New("openai: rate limit exceeded"),
			wantCat: CategoryRateLimited,
		},
		{
			name:    "request_too_large in message",
			err:     errors.New("anthropic stream: 413 Request Entity Too Large: request_too_large"),
			wantCat: CategoryRequestTooLarge,
		},
		{
			name:    "context length in message",
			err:     errors.New("this model's maximum context length is 128000 tokens"),
			wantCat: CategoryContextLength,
		},
		{
			name:    "connection refused",
			err:     errors.New("connection refused"),
			wantCat: CategoryNetworkError,
		},
		{
			name:    "timeout",
			err:     errors.New("context deadline exceeded"),
			wantCat: CategoryTimeout,
		},
		{
			name:    "unknown error",
			err:     errors.New("something unexpected happened"),
			wantCat: CategoryUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.err)
			if got == nil {
				t.Fatal("Classify returned nil")
			}
			if got.Category != tt.wantCat {
				t.Errorf("Category = %v, want %v", got.Category, tt.wantCat)
			}
		})
	}
}

func TestClassifiedUnwrap(t *testing.T) {
	orig := &model.APIError{StatusCode: 429, Status: "429 Too Many Requests"}
	wrapped := fmt.Errorf("anthropic chat: %w", orig)
	got := Classify(wrapped)
	if got == nil {
		t.Fatal("Classify returned nil")
	}
	if !errors.Is(got, wrapped) {
		t.Error("Unwrap chain broken: cannot find original error")
	}
}

func TestCategoryString(t *testing.T) {
	if s := CategoryOverloaded.String(); s != "overloaded" {
		t.Errorf("CategoryOverloaded.String() = %q, want %q", s, "overloaded")
	}
	if s := Category(999).String(); s != "unknown" {
		t.Errorf("Category(999).String() = %q, want %q", s, "unknown")
	}
}
