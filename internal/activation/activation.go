// Package activation implements PRD P3-007: spreading activation and
// predictive context loading. When an agent accesses a symbol via a graph
// tool, its graph neighbors (callers, callees, tests) are pre-fetched into
// an LRU buffer. Cache reads validate graph identity and revisions before use.
package activation

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/graph"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

const (
	defaultMaxSize       = 50
	maxPrefetchJobs      = 16 // includes the active job
	prefetchTimeout      = 2 * time.Second
	maxNeighbors         = 20
	maxEntryBytes        = 16 * 1024
	maxPredictiveSymbols = 5
	maxPredictiveBytes   = 4096
	maxSummaryBytes      = 768
)

// GraphReader is the part of a graph backend activation queries; the
// chronos index (graph.IndexScope.Live) satisfies it.
type GraphReader interface {
	FindSymbols(context.Context, string, string) ([]graph.Symbol, error)
	FileHash(context.Context, string) (string, error)
	CallersOf(context.Context, string) ([]string, error)
	CalleesOf(context.Context, string) ([]string, error)
}

type prefetchJob struct {
	ctx   context.Context
	store GraphReader
	name  string
	key   string
	done  chan struct{}
}

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
	ctx     context.Context
	cancel  context.CancelFunc
	jobs    chan *prefetchJob
	pending map[string]*prefetchJob
	worker  sync.WaitGroup
	started bool
	closed  bool
}

// NewBuffer creates a buffer with the given maximum number of entries.
func NewBuffer(maxSize int) *Buffer {
	if maxSize <= 0 {
		maxSize = defaultMaxSize
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Buffer{
		entries: make(map[string]*Entry, maxSize),
		maxSize: maxSize,
		ctx:     ctx,
		cancel:  cancel,
		jobs:    make(chan *prefetchJob, maxPrefetchJobs),
		pending: make(map[string]*prefetchJob),
	}
}

// Enqueue schedules best-effort prefetch without blocking. False means the
// buffer is closed, the request is canceled/invalid, or all 16 job slots are
// occupied. Duplicate store/name requests share the first request's context.
// A single lazy worker handles all jobs; each active job has a two-second limit.
func (b *Buffer) Enqueue(ctx context.Context, store GraphReader, name string) bool {
	if store == nil {
		return false
	}
	return b.enqueue(ctx, store, name) != nil
}

func (b *Buffer) enqueue(ctx context.Context, store GraphReader, name string) *prefetchJob {
	if ctx.Err() != nil || name == "" || len(name) > maxEntryBytes {
		return nil
	}
	key := repositoryKey(store) + "\x00" + name
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	if job := b.pending[key]; job != nil {
		return job
	}
	if len(b.pending) >= maxPrefetchJobs {
		return nil
	}
	job := &prefetchJob{ctx: ctx, store: store, name: name, key: key, done: make(chan struct{})}
	b.pending[key] = job
	b.jobs <- job
	if !b.started {
		b.started = true
		b.worker.Add(1)
		go b.run()
	}
	return job
}

func (b *Buffer) run() {
	defer b.worker.Done()
	for {
		select {
		case <-b.ctx.Done():
			return
		case job := <-b.jobs:
			if job.ctx.Err() == nil && b.ctx.Err() == nil {
				ctx, cancel := context.WithTimeout(job.ctx, prefetchTimeout)
				stop := context.AfterFunc(b.ctx, cancel)
				b.prefetch(ctx, job.store, job.name)
				stop()
				cancel()
			}
			b.mu.Lock()
			delete(b.pending, job.key)
			close(job.done)
			b.mu.Unlock()
		}
	}
}

// Close cancels and joins all prefetch work and rejects future jobs. It is safe
// to call concurrently or repeatedly. The owner must call Close BEFORE closing
// any graph store used by this buffer. Foreground tool calls must also finish
// before their graph store closes. Close does not close the graph store.
func (b *Buffer) Close() error {
	b.mu.Lock()
	b.closed = true
	b.cancel()
	b.mu.Unlock()
	b.worker.Wait()
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(b.jobs) > 0 {
		job := <-b.jobs
		delete(b.pending, job.key)
		close(job.done)
	}
	return nil
}

// Get retrieves a cached entry and promotes it in the LRU order. The second
// return value is false when no entry exists for name.
func (b *Buffer) Get(name string) (*Entry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.entries[name]
	queryName := name
	if !ok {
		for key, candidate := range b.entries {
			if candidate.Symbol.Name != queryName {
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

func (b *Buffer) entriesForName(ctx context.Context, store GraphReader, name, kind string) ([]*Entry, bool) {
	// Snapshot under the mutex; all SQL runs after releasing it. Validate the
	// complete live result set as eviction or newly indexed declarations may
	// otherwise turn an ambiguous name into a misleading partial cache hit.
	b.mu.Lock()
	candidates := make(map[string]*Entry)
	for key, entry := range b.entries {
		if entry.Symbol.Name == name && entry.Repository == repositoryKey(store) &&
			(kind == "" || string(entry.Symbol.Kind) == kind) {
			candidates[key] = entry
		}
	}
	b.mu.Unlock()
	var entries []*Entry
	var keys []string
	stale := make(map[string]*Entry)
	for key, entry := range candidates {
		revision, err := store.FileHash(ctx, entry.Symbol.File)
		if err != nil || revision != entry.Revision {
			stale[key] = entry
			delete(candidates, key)
		}
	}
	if len(candidates) > 0 {
		syms, err := store.FindSymbols(ctx, name, kind)
		if err == nil {
			for _, sym := range syms {
				var match *Entry
				var matchKey string
				for key, entry := range candidates {
					if entry.Symbol == sym {
						match, matchKey = entry, key
						break
					}
				}
				if match == nil {
					entries = nil
					break
				}
				entries = append(entries, match)
				keys = append(keys, matchKey)
			}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for key, entry := range stale {
		if b.entries[key] == entry {
			delete(b.entries, key)
			b.removeFromOrder(key)
		}
	}
	if len(entries) == 0 || ctx.Err() != nil {
		b.misses++
		return nil, false
	}
	for _, key := range keys {
		if _, ok := b.entries[key]; ok {
			b.promote(key)
		}
	}
	b.hits++
	return entries, true
}

// Put inserts or updates an entry, evicting the least-recently-used entry
// when the buffer is at capacity.
func (b *Buffer) Put(name string, entry *Entry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
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

func repositoryKey(store GraphReader) string {
	return fmt.Sprintf("%p", store)
}

func entryKey(store GraphReader, sym graph.Symbol, revision string) string {
	return repositoryKey(store) + "\x00" + sym.Package + "\x00" + sym.Name + "\x00" +
		string(sym.Kind) + "\x00" + sym.File + "\x00" + sym.Receiver + "\x00" + itoa(sym.Line) + "\x00" + revision
}

// Prefetch loads a symbol and its immediate neighbors into the buffer. It
// resolves the symbol in the graph store, queries its callers and callees,
// identifies test functions among callers, and stores everything. Neighbors
// themselves are stored at shallow depth (symbol info only, no recursive
// caller/callee resolution). This waits on the same bounded queue as Enqueue;
// when full, prefetch is skipped. Cancellation may return before worker cleanup;
// Close is the join barrier.
func (b *Buffer) Prefetch(ctx context.Context, store GraphReader, name string) {
	if store == nil {
		return
	}
	job := b.enqueue(ctx, store, name)
	if job == nil {
		return
	}
	select {
	case <-job.done:
	case <-ctx.Done():
	case <-b.ctx.Done():
	}
}

func (b *Buffer) prefetch(ctx context.Context, store GraphReader, name string) {
	if ctx.Err() != nil {
		return
	}
	syms, err := store.FindSymbols(ctx, name, "")
	if err != nil || len(syms) == 0 {
		return
	}
	callers, err := store.CallersOf(ctx, name)
	if err != nil {
		return
	}
	callees, err := store.CalleesOf(ctx, name)
	if err != nil {
		return
	}
	callers = boundedNeighbors(callers)
	callees = boundedNeighbors(callees)

	var tests []string
	for _, c := range callers {
		if strings.HasPrefix(c, "Test") {
			tests = append(tests, c)
		}
	}

	sortSymbols(syms)
	remaining := b.maxSize
	for _, sym := range syms {
		if remaining == 0 || ctx.Err() != nil {
			return
		}
		remaining--
		if symbolBytes(sym) > maxEntryBytes {
			continue
		}
		revision, err := store.FileHash(ctx, sym.File)
		if err != nil || ctx.Err() != nil {
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
	sort.Strings(neighbors)
	if len(neighbors) > maxNeighbors {
		neighbors = neighbors[:maxNeighbors]
	}
	for _, n := range neighbors {
		if remaining == 0 || ctx.Err() != nil {
			return
		}
		if _, exists := b.entriesForName(ctx, store, n, ""); exists {
			continue
		}
		nSyms, err := store.FindSymbols(ctx, n, "")
		if err != nil || len(nSyms) == 0 {
			continue
		}
		sortSymbols(nSyms)
		for _, sym := range nSyms {
			if remaining == 0 || ctx.Err() != nil {
				return
			}
			remaining--
			if symbolBytes(sym) > maxEntryBytes {
				continue
			}
			revision, err := store.FileHash(ctx, sym.File)
			if err != nil || ctx.Err() != nil {
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
//  1. graph_query checks and validates the activation buffer;
//  2. After any graph_query or resolve_symbol returns, the accessed symbols'
//     neighbors are pre-fetched into the buffer in the background.
//
// The buffer owner must Close it before closing store.
func Wrap(a *agent.Agent, store GraphReader, buf *Buffer) {
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

func wrapGraphQuery(def *tool.Definition, store GraphReader, buf *Buffer) {
	orig := def.Handler
	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		name, _ := args["name"].(string)
		kind, _ := args["kind"].(string)

		if entries, ok := buf.entriesForName(ctx, store, name, kind); ok {
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
			buf.Enqueue(ctx, store, name)
			return result, nil
		}

		result, err := orig(ctx, args)
		if err != nil {
			return result, err
		}

		if name != "" {
			buf.Enqueue(ctx, store, name)
		}

		if m, ok := result.(map[string]any); ok {
			if found, _ := m["found"].(bool); found {
				if entries, ok := buf.entriesForName(ctx, store, name, kind); ok {
					if ns := neighborHints(entries[0]); len(ns) > 0 {
						m["_neighbors"] = ns
					}
				}
			}
		}

		return result, err
	}
}

func wrapResolveSymbol(def *tool.Definition, store GraphReader, buf *Buffer) {
	orig := def.Handler
	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		result, err := orig(ctx, args)
		if err != nil {
			return result, err
		}
		name, _ := args["name"].(string)
		if name != "" {
			buf.Enqueue(ctx, store, name)
		}
		return result, err
	}
}

func wrapFindCallers(def *tool.Definition, store GraphReader, buf *Buffer) {
	orig := def.Handler
	def.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		// A callee's file hash cannot validate edges owned by other files.
		// Cached, capped neighbors are hints only; authoritative caller results
		// (including all depth argument forms) come from the original tool.
		result, err := orig(ctx, args)
		if err == nil {
			name, _ := args["name"].(string)
			buf.Enqueue(ctx, store, name)
		}
		return result, err
	}
}

// PredictiveContext extracts symbol-like identifiers from a user message,
// resolves them in the graph, and returns pre-loaded L2 summaries as a
// context block. This lets the model's first turn start with relevant code
// context instead of spending 2-3 turns reading files.
func PredictiveContext(ctx context.Context, store GraphReader, buf *Buffer, message string) string {
	if store == nil || ctx.Err() != nil {
		return ""
	}
	names := extractIdentifiers(message)
	if len(names) == 0 {
		return ""
	}

	var parts []string
	bytes := len("[Pre-loaded context]\n")
	seen := make(map[string]bool)
	for _, name := range names {
		if ctx.Err() != nil {
			return ""
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		syms, err := store.FindSymbols(ctx, name, "")
		if err != nil || len(syms) == 0 {
			continue
		}
		sortSymbols(syms)
		callers, err := store.CallersOf(ctx, name)
		if err != nil {
			continue
		}
		callees, err := store.CalleesOf(ctx, name)
		if err != nil {
			continue
		}
		for _, sym := range syms {
			if len(parts) == maxPredictiveSymbols || ctx.Err() != nil {
				break
			}
			part := limitBytes(formatL2(sym, len(callers), len(callees)), maxSummaryBytes)
			if bytes+len(part)+1 > maxPredictiveBytes {
				break
			}
			parts = append(parts, part)
			bytes += len(part) + 1
		}
		if buf != nil {
			buf.Enqueue(ctx, store, name)
		}
		if len(parts) >= maxPredictiveSymbols {
			break
		}
	}
	if len(parts) == 0 || ctx.Err() != nil {
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
	parts := []string{summaryField(sym.Name)}
	if sym.Kind != "" {
		parts = append(parts, summaryField(string(sym.Kind)))
	}
	if sym.File != "" {
		loc := summaryField(sym.File)
		if sym.Line > 0 {
			loc += ":" + itoa(sym.Line)
		}
		parts = append(parts, loc)
	}
	if sym.Signature != "" {
		parts = append(parts, summaryField(sym.Signature))
	}
	return "  " + strings.Join(parts, " | ") +
		" | callers=" + itoa(callerCount) + " callees=" + itoa(calleeCount)
}

func summaryField(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(limitBytes(s, maxSummaryBytes))
}

func limitBytes(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "…"
}

func symbolBytes(sym graph.Symbol) int {
	return len(sym.Name) + len(sym.Kind) + len(sym.Package) + len(sym.File) +
		len(sym.Signature) + len(sym.Doc) + len(sym.Receiver)
}

func boundedNeighbors(names []string) []string {
	sort.Strings(names)
	out := make([]string, 0, maxNeighbors)
	for _, name := range names {
		if len(name) > 1024 {
			continue
		}
		out = append(out, name)
		if len(out) == maxNeighbors {
			break
		}
	}
	return out
}

func sortSymbols(syms []graph.Symbol) {
	sort.Slice(syms, func(i, j int) bool {
		a, b := syms[i], syms[j]
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Receiver != b.Receiver {
			return a.Receiver < b.Receiver
		}
		return a.ID < b.ID
	})
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
