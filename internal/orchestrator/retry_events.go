package orchestrator

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/spawn08/chronos/engine/model"
	chronosstream "github.com/spawn08/chronos/engine/stream"
)

// withRetryEvents reports provider retries as api_retry activity so a long
// backoff (e.g. a rate limit) reads as a retry instead of a stalled turn.
func (o *Orchestrator) withRetryEvents(ctx context.Context, agentID, sessionID string) context.Context {
	if o == nil || o.broker == nil {
		return ctx
	}
	broker := o.broker
	return model.WithRetryObserver(ctx, func(info model.RetryInfo) {
		event := chronosstream.Event{Type: chronosstream.EventCustom, Data: map[string]any{
			"type": "api_retry", "agent": agentID, "message": retryMessage(info),
		}}
		if sessionID != "" {
			broker.PublishTopic(sessionID, event)
		} else {
			broker.Publish(event)
		}
	})
}

func retryMessage(info model.RetryInfo) string {
	cause := "connection error"
	switch {
	case info.StatusCode == http.StatusTooManyRequests:
		cause = "rate limited"
	case info.StatusCode >= 500:
		cause = fmt.Sprintf("server error %d", info.StatusCode)
	case info.StatusCode != 0:
		cause = fmt.Sprintf("HTTP %d", info.StatusCode)
	}
	return fmt.Sprintf("model API %s · retrying in %s (retry %d)", cause, info.Delay.Round(100*time.Millisecond), info.Attempt)
}
