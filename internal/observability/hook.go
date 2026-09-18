package observability

import (
	"context"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
)

// Hook observes model/tool lifecycle metadata only. It never reads Event.Input
// or Event.Output, which may contain prompts, tool arguments, or tool results.
type Hook struct {
	registry *Registry
	mu       sync.Mutex
	started  map[string]time.Time
}

func NewHook(registry *Registry) *Hook {
	return &Hook{registry: registry, started: make(map[string]time.Time)}
}

func (h *Hook) Before(_ context.Context, event *hooks.Event) error {
	if h == nil || h.registry == nil || event == nil || event.Type != hooks.EventModelCallBefore {
		return nil
	}
	id, _ := event.Metadata["correlation_id"].(string)
	if id != "" {
		h.mu.Lock()
		h.started[id] = time.Now()
		h.mu.Unlock()
	}
	return nil
}

func (h *Hook) After(_ context.Context, event *hooks.Event) error {
	if h == nil || h.registry == nil || event == nil {
		return nil
	}
	switch event.Type {
	case hooks.EventToolCallAfter:
		if event.Error != nil {
			h.registry.ToolFailure(event.Name)
		}
	case hooks.EventModelCallAfter:
		id, _ := event.Metadata["correlation_id"].(string)
		started := time.Now()
		h.mu.Lock()
		if value, ok := h.started[id]; ok {
			started = value
			delete(h.started, id)
		}
		h.mu.Unlock()
		retries, _ := event.Metadata["retry_count"].(int)
		if legacy, ok := event.Metadata["retry_attempts"].(int); ok {
			retries += legacy
		}
		h.registry.ProviderCall(event.Name, time.Since(started), event.Error != nil, retries)
	}
	return nil
}
