package orchestrator

import (
	"context"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
)

type outputTokensProvider struct{ name, model string }

func (p outputTokensProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	return nil, nil
}
func (p outputTokensProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}
func (p outputTokensProvider) Name() string  { return p.name }
func (p outputTokensProvider) Model() string { return p.model }

func TestOutputTokensHookRaisesStreamingAnthropicDefault(t *testing.T) {
	for _, tc := range []struct {
		name      string
		provider  string
		model     string
		stream    bool
		maxTokens int
		want      int
	}{
		{"sonnet 4.6 streaming", "anthropic", "claude-sonnet-4-6", true, 0, 32000},
		{"opus 4 streaming", "anthropic", "claude-opus-4-20250514", true, 0, 32000},
		{"claude 3.7 streaming", "anthropic", "claude-3-7-sonnet-latest", true, 0, 32000},
		{"claude 3.5 streaming", "anthropic", "claude-3-5-sonnet-20241022", true, 0, 8192},
		{"claude 3 haiku keeps default", "anthropic", "claude-3-haiku-20240307", true, 0, 0},
		{"unary keeps default", "anthropic", "claude-sonnet-4-6", false, 0, 0},
		{"explicit cap kept", "anthropic", "claude-sonnet-4-6", true, 2000, 2000},
		{"other provider untouched", "bedrock", "anthropic.claude-sonnet-4-6", true, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := &model.ChatRequest{Model: tc.model, MaxTokens: tc.maxTokens}
			evt := &hooks.Event{Type: hooks.EventModelCallBefore, Input: req, Metadata: map[string]any{
				"provider": outputTokensProvider{name: tc.provider, model: tc.model},
				"stream":   tc.stream,
			}}
			if err := (outputTokensHook{}).Before(context.Background(), evt); err != nil {
				t.Fatal(err)
			}
			if req.MaxTokens != tc.want {
				t.Fatalf("MaxTokens = %d, want %d", req.MaxTokens, tc.want)
			}
		})
	}
}
