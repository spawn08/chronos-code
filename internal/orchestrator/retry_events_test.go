package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/model"
	chronosstream "github.com/spawn08/chronos/engine/stream"
)

func TestRetryEventsPublishAPIRetryToSession(t *testing.T) {
	orch := &Orchestrator{broker: chronosstream.NewBroker()}
	sub, err := orch.broker.SubscribeTopic("session-1")
	if err != nil {
		t.Fatal(err)
	}
	ctx := orch.withRetryEvents(context.Background(), "coder", "session-1")
	model.NotifyRetry(ctx, model.RetryInfo{Attempt: 1, Delay: 2 * time.Second, StatusCode: 429})
	select {
	case evt := <-sub.C:
		data, _ := evt.Data.(map[string]any)
		if evt.Type != chronosstream.EventCustom || data["type"] != "api_retry" || data["agent"] != "coder" {
			t.Fatalf("event = %#v", evt)
		}
		if msg := data["message"]; msg != "model API rate limited · retrying in 2s (retry 1)" {
			t.Fatalf("message = %q", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("no api_retry event published")
	}
}
