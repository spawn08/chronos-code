// Package tokencache memoizes local token counts, not model responses or files.
// Keys include the model and exact text, so edits and model switches cannot reuse
// an obsolete count. Provider-reported usage remains the source for billing.
package tokencache

import (
	"container/list"
	"strings"
	"sync"

	"github.com/spawn08/chronos/engine/model"
)

const (
	maxBytes      = 8 << 20
	maxEntries    = 2048
	maxEntryBytes = 1 << 20
)

type key struct{ model, text string }
type entry struct {
	key    key
	tokens int
}

// Cache is bounded and safe for concurrent use. Its zero value is ready to use.
// The owner controls its lifetime (one agent's preflight or tool pipeline).
type Cache struct {
	mu      sync.Mutex
	entries map[key]*list.Element
	order   list.List
	bytes   int
	modelID string
	base    model.TokenCounter
}

// ForModel returns an immutable model binding, including during concurrent model
// overrides. Only one underlying tokenizer is retained per cache.
func (c *Cache) ForModel(modelID string) model.TokenCounter {
	return counter{cache: c, modelID: modelID}
}

type counter struct {
	cache   *Cache
	modelID string
}

func (c counter) CountString(text string) int {
	if text == "" {
		return 0
	}
	cache := c.cache
	cache.mu.Lock()
	defer cache.mu.Unlock()
	k := key{model: c.modelID, text: text}
	if hit := cache.entries[k]; hit != nil {
		cache.order.MoveToFront(hit)
		return hit.Value.(entry).tokens
	}
	if cache.base == nil || cache.modelID != c.modelID {
		cache.base = model.NewTokenCounter(c.modelID)
		cache.modelID = c.modelID
	}
	n := cache.base.CountString(text)
	size := len(text) + len(c.modelID)
	if size > maxEntryBytes {
		return n
	}
	for cache.bytes+size > maxBytes || cache.order.Len() >= maxEntries {
		oldest := cache.order.Back()
		oldKey := oldest.Value.(entry).key
		delete(cache.entries, oldKey)
		cache.bytes -= len(oldKey.text) + len(oldKey.model)
		cache.order.Remove(oldest)
	}
	if cache.entries == nil {
		cache.entries = make(map[key]*list.Element)
	}
	// Substrings must not keep an arbitrarily large original payload alive.
	k = key{model: strings.Clone(c.modelID), text: strings.Clone(text)}
	cache.entries[k] = cache.order.PushFront(entry{key: k, tokens: n})
	cache.bytes += size
	return n
}

// CountTokens follows the SDK TokenCounter framing contract. Parity tests guard
// it against SDK changes; extra attachment/ID accounting lives in the guard.
func (c counter) CountTokens(messages []model.Message) int {
	total := 3
	for _, message := range messages {
		total += 4 + c.CountString(message.Content) + c.CountString(message.Name)
		for _, call := range message.ToolCalls {
			total += c.CountString(call.Name) + c.CountString(call.Arguments)
		}
	}
	return total
}
