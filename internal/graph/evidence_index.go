package graph

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/retrieve"
)

// Evidence packing estimates for the JSON rendering: each item's keys and
// location cost about itemOverhead tokens, the envelope about
// envelopeTokens, and fitEvidence reserves 10% + 32 for its exact check.
const (
	evidenceItemOverhead   = 70
	evidenceEnvelopeTokens = 160
)

// indexedOf returns the chronos-index backend behind store, if any.
func indexedOf(store Backend) *indexBackend {
	switch b := store.(type) {
	case *indexBackend:
		return b
	case *mergedBackend:
		return indexedOf(b.primary)
	}
	return nil
}

// collectIndexedEvidence answers codebase_context from the index with graph
// retrieval: seeds, call-graph expansion, diversification and packing by
// estimated tokens, then reads only the excerpts it kept. fitEvidence still
// certifies the budget with one exact count of the serialized result.
func collectIndexedEvidence(ctx context.Context, b *indexBackend, root string, req evidenceRequest) (*evidenceResult, error) {
	result := &evidenceResult{Revision: evidenceRevision(ctx, root), MaxTokens: req.maxTokens, Items: []evidenceItem{}, Omitted: map[string]int{}}
	if result.Revision == "unavailable" {
		result.Omitted["revision_unavailable"]++
	}
	if labelled := labelResult(b, map[string]any{}, false, false, ""); labelled["index"] != nil {
		result.Index = labelled["index"].(map[string]any)
	}
	rreq := retrieve.Request{
		Query: req.query, Symbols: req.symbols,
		Callers: req.include["callers"], Tests: req.include["tests"],
		Callees:  req.include["definitions"],
		Excerpts: req.include["excerpts"],
		Budget:   max(0, (req.maxTokens-32)*9/10-evidenceEnvelopeTokens), ItemOverhead: evidenceItemOverhead, LineTokens: 20,
		Seen: b.seen,
	}
	for _, r := range req.ranges {
		rreq.Ranges = append(rreq.Ranges, retrieve.Range{File: filepath.ToSlash(r.File), Start: r.StartLine, End: r.EndLine})
		result.Items = append(result.Items, evidenceItem{Role: "range", File: filepath.ToSlash(r.File), StartLine: r.StartLine, EndLine: r.EndLine, Zoom: retrieve.Excerpt.String()})
	}
	sel := retrieve.Retrieve(b.view, rreq)
	for reason, n := range sel.Omitted {
		result.Omitted[reason] += n
	}
	if len(sel.Misses) > 0 {
		result.Omitted["not_found"] += len(sel.Misses)
		result.Misses = sel.Misses
	}
	if sel.Seeds == 0 && req.query != "" && len(req.symbols) == 0 && len(req.ranges) == 0 && len(sel.Misses) == 0 {
		result.Omitted["not_found"]++
	}
	result.Invalidated = sel.Invalidated
	type window struct{ start, end int }
	windows := map[int]window{}
	for _, it := range sel.Items {
		if it.Role == "definition" && !req.include["definitions"] && !req.include["excerpts"] {
			continue
		}
		if len(result.Items) >= evidenceItemLimit {
			result.Omitted["item_limit"]++
			continue
		}
		s := it.Symbol
		item := evidenceItem{
			Role: it.Role, Name: s.Name, Target: it.Target, Kind: SymbolKind(s.Kind), Package: s.Package,
			File: s.File, StartLine: s.Line, EndLine: max(s.Line, s.EndLine), Why: it.Why, Zoom: it.Zoom.String(),
		}
		if it.Zoom >= retrieve.Signature {
			item.Signature = s.Signature
			if len(item.Signature) > 512 {
				item.Signature = evidenceClip(item.Signature, 512)
				result.Omitted["metadata_limit"]++
			}
		}
		if m, ok := b.view.FileMeta(s.File); ok && m.Hash != 0 {
			item.IndexedHash = fmt.Sprintf("%016x", m.Hash)
		} else {
			result.Omitted["unindexed_graph"]++
		}
		if it.Zoom == retrieve.Excerpt {
			windows[len(result.Items)] = window{it.Start, it.End}
		}
		result.Items = append(result.Items, item)
	}
	if req.include["excerpts"] && len(result.Items) > 0 {
		fs, err := os.OpenRoot(root)
		if err != nil {
			return nil, fmt.Errorf("open evidence root: %w", err)
		}
		defer fs.Close()
		remaining := evidenceReadLimit
		for i := range result.Items {
			item := &result.Items[i]
			w, ok := windows[i]
			if item.Role == "range" {
				w, ok = window{item.StartLine, item.EndLine}, true
			}
			if !ok {
				continue
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			source, reason, err := readEvidenceSource(ctx, b, fs, root, item.File, max(1, w.start), max(1, w.end), &remaining)
			if err != nil {
				return nil, err
			}
			if w.end < item.EndLine && item.Role != "range" {
				source.Truncated = true
			}
			item.Source = source
			if reason != "" {
				result.Omitted[reason]++
			}
		}
		result.IOBytes = evidenceReadLimit - remaining
	}
	result.Truncated = len(result.Omitted) > 0
	return result, ctx.Err()
}

// recordSeen remembers the verified excerpts actually delivered.
func (b *indexBackend) recordSeen(items []evidenceItem) {
	if b.seen == nil {
		return
	}
	for _, it := range items {
		if it.Source == nil || it.Source.Text == "" || !deliverable(it.Source) {
			continue
		}
		hash, err := strconv.ParseUint(it.Source.IndexedHash, 16, 64)
		if err != nil {
			continue
		}
		b.seen.Add(it.File, hash, it.Source.StartLine, it.Source.EndLine)
	}
}

// Prefetch returns task-ranked repository context for message within
// maxTokens (exact count), rendered as plain text for the first model call
// of a turn, or "" when the task names nothing the index can anchor on.
// Verified excerpts delivered here count as seen for the session.
func (s *IndexScope) Prefetch(ctx context.Context, message string, maxTokens int) (string, error) {
	if maxTokens <= 0 || strings.TrimSpace(message) == "" {
		return "", nil
	}
	store, root, release, err := s.backend(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	b := indexedOf(store)
	if b == nil {
		return "", nil
	}
	sel := retrieve.Retrieve(b.view, retrieve.Request{
		Query: message, Callers: true, Callees: true, Excerpts: true,
		Budget: maxTokens * 8 / 10, ItemOverhead: 20, Seen: b.seen,
	})
	if sel.Seeds == 0 || len(sel.Items) == 0 {
		return "", nil
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return "", fmt.Errorf("open prefetch root: %w", err)
	}
	defer fs.Close()
	report := b.IndexReport()
	header := fmt.Sprintf("[Repository context] Selected from the code index for this task (%s; relationships matched by name). Use graph tools or codebase_context with these names for more.\n", describeReport(report))
	var blocks []string
	var delivered []evidenceItem
	remaining := evidenceReadLimit
	for _, it := range sel.Items {
		sym := it.Symbol
		var line strings.Builder
		fmt.Fprintf(&line, "- %s %s — %s:%d-%d — %s\n", sym.Kind, sym.Qualified(), sym.File, sym.Line, max(sym.Line, sym.EndLine), it.Why)
		if it.Zoom >= retrieve.Signature && sym.Signature != "" {
			fmt.Fprintf(&line, "  %s\n", evidenceClip(sym.Signature, 300))
		}
		if it.Zoom == retrieve.Excerpt {
			src, _, err := readEvidenceSource(ctx, b, fs, root, sym.File, it.Start, it.End, &remaining)
			if err != nil {
				return "", err
			}
			if deliverable(src) && src.Text != "" {
				fmt.Fprintf(&line, "  ```go\n%s  ```\n", indentBlock(src.Text))
				delivered = append(delivered, evidenceItem{File: sym.File, Source: src})
			}
		}
		blocks = append(blocks, line.String())
	}
	if len(sel.Misses) > 0 {
		blocks = append(blocks, "Not in the index: "+strings.Join(sel.Misses, ", ")+"\n")
	}
	counter, _ := evidenceCounter()
	evidenceCounterMu.Lock()
	defer evidenceCounterMu.Unlock()
	for len(blocks) > 0 {
		text := header + strings.Join(blocks, "")
		// Every token covers at least one byte, so a short text fits untokenized.
		if len(text) <= maxTokens || counter.CountString(text) <= maxTokens {
			kept := strings.Join(blocks, "")
			for _, d := range delivered {
				if strings.Contains(kept, d.Source.Text) {
					b.recordSeen([]evidenceItem{d})
				}
			}
			return text, nil
		}
		blocks = blocks[:len(blocks)-1]
	}
	return "", nil
}

// deliverable reports whether an excerpt matches the indexed file: its hash
// was verified, or (for a partial read) the file's mtime matches the index.
func deliverable(src *evidenceSource) bool {
	return src.Freshness == "verified" || src.Freshness == "unverified"
}

func indentBlock(text string) string {
	lines := strings.SplitAfter(text, "\n")
	var out strings.Builder
	for _, l := range lines {
		if l != "" {
			out.WriteString("  " + l)
		}
	}
	if !strings.HasSuffix(text, "\n") {
		out.WriteByte('\n')
	}
	return out.String()
}
