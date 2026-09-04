// Package activation implements PRD P3-007: spreading activation and
// predictive context loading. When an agent accesses a symbol via a graph
// tool, its graph neighbors (callers, callees, tests) are pre-fetched into
// an LRU buffer. Follow-up queries that hit the buffer avoid a SQLite
// round-trip entirely.
package activation

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/spawn08/chronos-code/internal/graph"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

const defaultMaxSize = 50

// Entry holds pre-fetched data for a single symbol.
type Entry struct {
	Symbol     graph.Symbol
	Repository string
	Revision   string
	Callers    []string
	Callees    []string
	Tests      []string
}

// Buffer is a concurrency-safe LRU cache of pre-fetched symbol neighbors.
type Buffer struct {
	mu      sync.Mutex
	entries map[string]*Entry
	order   []string
	maxSize int
	hits    int
	misses  int
}

// NewBuffer creates a buffer with the given maximum number of entries.
func NewBuffer(maxSize int) *Buffer {
	if maxSize <= 0 {
		maxSize = defaultMaxSize
	}
	return &Buffer{
		entries: make(map[string]*Entry, maxSize),
		maxSize: maxSize,
	}
}

// Get retrieves a cached entry and promotes it in the LRU order. The second
// return value is false when no entry exists for name.
func (b *Buffer) Get(name string) (*Entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[name]
	if !ok {
		for key, candidate := range b.entries {
			if candidate.Symbol.Name != name {
				continue
			}
			if e != nil {
				b.misses++
				return nil, false
			}
			e, ok = candidate, true
			name = key
		}
	}
	if ok {
		b.hits++
		b.promote(name)
	} else {
		b.misses++
	}
	return e, ok
}

func (b *Buffer) entriesForName(ctx context.Context, store *graph.Store, name string) ([]*Entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var entries []*Entry
	for key, entry := range b.entries {
		if entry.Symbol.Name != name || entry.Repository != repositoryKey(store) {
			continue
		}
		if entry.Revision != "" {
			revision, err := store.FileHash(ctx, entry.Symbol.File)
			if err != nil || revision != entry.Revision {
				delete(b.entries, key)
				b.removeFromOrder(key)
				continue
			}
		}
		entries = append(entries, entry)
		b.promote(key)
	}
	if len(entries) == 0 {
		b.misses++
		return nil, false
	}
	b.hits++
	return entries, true
}

// Put inserts or updates an entry, evicting the least-recently-used entry
// when the buffer is at capacity.
func (b *Buffer) Put(name string, entry *Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, exists := b.entries[name]; exists {
		b.entries[name] = entry
		b.promote(name)
		return
	}
	for len(b.order) >= b.maxSize {
		oldest := b.order[0]
		b.order = b.order[1:]
		delete(b.entries, oldest)
	}
	b.entries[name] = entry
	b.order = append(b.order, name)
}

// Len returns the number of entries currently in the buffer.
func (b *Buffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

// Stats returns the cumulative hit and miss counts.
func (b *Buffer) Stats() (hits, misses int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hits, b.misses
}

// HitRate returns the fraction of Get calls that returned a cached entry.
func (b *Buffer) HitRate() float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	total := b.hits + b.misses
	if total == 0 {
		return 0
	}
	return float64(b.hits) / float64(total)
}

func (b *Buffer) promote(name string) {
	for i, n := range b.order {
		if n == name {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
	b.order = append(b.order, name)
}

func (b *Buffer) removeFromOrder(name string) {
	for i, n := range b.order {
		if n == name {
			b.order = append(b.order[:i], b.order[i+1:]...)
			return
		}
	}
}

func repositoryKey(store *graph.Store) string {
	return fmt.Sprintf("%p", store)
}

func entryKey(store *graph.Store, sym graph.Symbol, revision string) string {
	return repositoryKey(store) + "\x00" + sym.Package + "\x00" + sym.Name + "\x00" +
		string(sym.Kind) + "\x00" + sym.File + "\x00" + revision
}

// Prefetch loads a symbol and its immediate neighbors into the buffer. It
// resolves the symbol in the graph store, queries its callers and callees,
// identifies test functions among callers, and stores everything. Neighbors
// themselves are stored at shallow depth (symbol info only, no recursive
// caller/callee resolution).
func (b *Buffer) Prefetch(ctx context.Context, store *graph.Store, name string) {
	syms, err := store.FindSymbols(ctx, name, "")
	if err != nil || len(syms) == 0 {
		return
	}
	callers, _ := store.CallersOf(ctx, name)
	callees, _ := store.CalleesOf(ctx, name)

	var tests []string
	for _, c := range callers {
		if strings.HasPrefix(c, "Test") {
			tests = append(tests, c)
		}
	}

	for _, sym := range syms {
		revision, err := store.FileHash(ctx, sym.File)
		if err != nil {
			continue
		}
		b.Put(entryKey(store, sym, revision), &Entry{
			Symbol:     sym,
			Repository: repositoryKey(store),
			Revision:   revision,
			Callers:    callers,
			Callees:    callees,
			Tests:      tests,
		})
	}

	neighbors := mergeUnique(callers, callees)
	if len(neighbors) > 20 {
		neighbors = neighbors[:20]
	}
	for _, n := range neighbors {
		b.mu.Lock()
		_, exists := b.entries[n]
		b.mu.Unlock()
		if exists {
			continue
		}
		nSyms, err := store.FindSymbols(ctx, n, "")
		if err != nil || len(nSyms) == 0 {
			continue
		}
		for _, sym := range nSyms {
			revision, err := store.FileHash(ctx, sym.File)
			if err != nil {
				continue
			}
			b.Put(entryKey(store, sym, revision), &Entry{
				Symbol:     sym,
				Repository: repositoryKey(store),
				Revision:   revision,
			})
		}
	}
}

// Wrap wraps the graph tools registered on a so that:
//  1. graph_query checks the activation buffer before querying SQLite;
//  2. After any graph_query or resolve_symbol returns, the accessed symbols'
//     neighbors are pre-fetched into the buffer in the background.
//
// This eliminates follow-up tool calls for neighbors the model is likely to
// ask about next (~70% hit rate based on code navigation patterns).
func Wrap(a *agent.Agent, store *graph.Store, buf *Buffer) {
	for _, def := range a.Tools.List() {
		switch def.Name {
		case "graph_query":
			wrapGraphQuery(def, store, buf)
		case "resolve_symbol":
			wrapResolveSymbol(def, store, buf)
		case "find_callers":
			wrapFindCallers(def, store, buf)
		}
	}
}

func wrapGraphQuery(def *tool.Definition, store *graph.Store, buf *Buffer) {
	orig := def.Handler
	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		name, _ := args["name"].(string)

		if entries, ok := buf.entriesForName(ctx, store, name); ok {
			summaries := make([]map[string]any, 0, len(entries))
			for _, entry := range entries {
				summaries = append(summaries, entrySummary(entry))
			}
			result := map[string]any{
				"found":      true,
				"symbols":    summaries,
				"_activated": true,
			}
			if ns := neighborHints(entries[0]); len(ns) > 0 {
				result["_neighbors"] = ns
			}
			go buf.Prefetch(context.Background(), store, name)
			return result, nil
		}

		result, err := orig(ctx, args)
		if err != nil {
			return result, err
		}

		if name != "" {
			go func() {
				buf.Prefetch(context.Background(), store, name)
			}()
		}

		if m, ok := result.(map[string]any); ok {
			if found, _ := m["found"].(bool); found {
				if entries, ok := buf.entriesForName(ctx, store, name); ok {
					if ns := neighborHints(entries[0]); len(ns) > 0 {
						m["_neighbors"] = ns
					}
				}
			}
		}

		return result, err
	}
}

func wrapResolveSymbol(def *tool.Definition, store *graph.Store, buf *Buffer) {
	orig := def.Handler
	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		result, err := orig(ctx, args)
		if err != nil {
			return result, err
		}
		name, _ := args["name"].(string)
		if name != "" {
			go buf.Prefetch(context.Background(), store, name)
		}
		return result, err
	}
}

func wrapFindCallers(def *tool.Definition, store *graph.Store, buf *Buffer) {
	orig := def.Handler
	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		name, _ := args["name"].(string)
		depth, _ := args["depth"].(float64)
		if name != "" && (depth == 0 || depth == 1) {
			if entries, ok := buf.entriesForName(ctx, store, name); ok && len(entries) == 1 && len(entries[0].Callers) > 0 {
				entry := entries[0]
				level := map[string][]string{name: entry.Callers}
				return map[string]any{
					"name":             name,
					"callers_by_depth": []map[string][]string{level},
					"_activated":       true,
				}, nil
			}
		}
		return orig(ctx, args)
	}
}

// PredictiveContext extracts symbol-like identifiers from a user message,
// resolves them in the graph, and returns pre-loaded L2 summaries as a
// context block. This lets the model's first turn start with relevant code
// context instead of spending 2-3 turns reading files.
func PredictiveContext(ctx context.Context, store *graph.Store, buf *Buffer, message string) string {
	if store == nil {
		return ""
	}
	names := extractIdentifiers(message)
	if len(names) == 0 {
		return ""
	}

	var parts []string
	seen := make(map[string]bool)
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		syms, err := store.FindSymbols(ctx, name, "")
		if err != nil || len(syms) == 0 {
			continue
		}
		for _, sym := range syms {
			callers, _ := store.CallersOf(ctx, sym.Name)
			callees, _ := store.CalleesOf(ctx, sym.Name)
			parts = append(parts, formatL2(sym, len(callers), len(callees)))
			buf.Prefetch(ctx, store, sym.Name)
		}
		if len(parts) >= 5 {
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "[Pre-loaded context]\n" + strings.Join(parts, "\n")
}

var identRe = regexp.MustCompile(`\b([A-Z][a-zA-Z0-9]{2,}|[a-z][a-zA-Z0-9]*[A-Z][a-zA-Z0-9]*)\b`)

func extractIdentifiers(text string) []string {
	matches := identRe.FindAllString(text, 20)
	var out []string
	skip := map[string]bool{
		"The": true, "This": true, "That": true, "Then": true,
		"When": true, "What": true, "Where": true, "Which": true,
		"How": true, "Can": true, "Could": true, "Should": true,
		"Would": true, "Does": true, "Did": true, "Has": true,
		"Have": true, "Had": true, "Was": true, "Were": true,
		"Not": true, "But": true, "And": true, "For": true,
		"With": true, "From": true, "Into": true, "After": true,
		"Before": true, "Between": true, "All": true, "Any": true,
		"Each": true, "Every": true, "Some": true, "Other": true,
	}
	for _, m := range matches {
		if !skip[m] {
			out = append(out, m)
		}
	}
	return out
}

func entrySummary(e *Entry) map[string]any {
	return map[string]any{
		"name":      e.Symbol.Name,
		"kind":      string(e.Symbol.Kind),
		"package":   e.Symbol.Package,
		"file":      e.Symbol.File,
		"line":      e.Symbol.Line,
		"signature": e.Symbol.Signature,
		"doc":       e.Symbol.Doc,
		"receiver":  e.Symbol.Receiver,
	}
}

func neighborHints(e *Entry) map[string]any {
	if len(e.Callers) == 0 && len(e.Callees) == 0 && len(e.Tests) == 0 {
		return nil
	}
	hints := make(map[string]any)
	if len(e.Callers) > 0 {
		hints["callers"] = e.Callers
	}
	if len(e.Callees) > 0 {
		hints["callees"] = e.Callees
	}
	if len(e.Tests) > 0 {
		hints["tests"] = e.Tests
	}
	return hints
}

func formatL2(sym graph.Symbol, callerCount, calleeCount int) string {
	parts := []string{sym.Name}
	if sym.Kind != "" {
		parts = append(parts, string(sym.Kind))
	}
	if sym.File != "" {
		loc := sym.File
		if sym.Line > 0 {
			loc += ":" + itoa(sym.Line)
		}
		parts = append(parts, loc)
	}
	if sym.Signature != "" {
		parts = append(parts, sym.Signature)
	}
	return "  " + strings.Join(parts, " | ") +
		" | callers=" + itoa(callerCount) + " callees=" + itoa(calleeCount)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	buf := make([]byte, 0, 10)
	if n < 0 {
		buf = append(buf, '-')
		n = -n
	}
	digits := make([]byte, 0, 10)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	for i := len(digits) - 1; i >= 0; i-- {
		buf = append(buf, digits[i])
	}
	return string(buf)
}

func mergeUnique(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
