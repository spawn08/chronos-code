package query

import (
	"path"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/contracts"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/segment"
)

// Contract and document references (M8).
//
// A RefContract names a contract's global key, so it resolves by exact key
// to the contract nodes of its kind, wherever they are declared (a .proto
// file, an OpenAPI document, the file serving a route): import_resolved,
// the contract analogue of an import. A route whose method is unknown on
// either side ("ANY") joins every method of its path.
//
// A RefMention is a document section mentioning code. Backticked and bare
// identifiers resolve by name (name_matched, or ambiguous when several
// declarations match); a file path to the file's top-level declarations
// (import_resolved); a route like a RefContract; a ticket id to the other
// sections mentioning it (name_matched).

// contractKindSets holds one kind set per contract kind (named caches by
// set identity).
var contractKindSets = map[string]map[string]bool{
	facts.KindRoute: {facts.KindRoute: true}, facts.KindRPC: {facts.KindRPC: true}, facts.KindMessage: {facts.KindMessage: true},
	facts.KindTopic: {facts.KindTopic: true}, facts.KindTable: {facts.KindTable: true},
}

func (v *View) resolveContract(kind, key string) ([]Symbol, string) {
	kinds := contractKindSets[kind]
	if kinds == nil || key == "" {
		return nil, ""
	}
	out := v.named(key, kinds)
	if kind == facts.KindRoute {
		out = append(out[:len(out):len(out)], v.routeVariants(key, kinds)...)
	}
	if len(out) == 0 {
		return nil, ""
	}
	return capped(out), ImportResolved
}

// routeVariants returns the routes of key's path under the other methods it
// joins: ANY for a concrete method, every method for ANY.
func (v *View) routeVariants(key string, kinds map[string]bool) []Symbol {
	method, p := contracts.SplitRoute(key)
	if method != contracts.AnyMethod {
		return v.named(contracts.AnyMethod+" "+p, kinds)
	}
	var out []Symbol
	for _, m := range contracts.Methods {
		out = append(out, v.named(m+" "+p, kinds)...)
	}
	return out
}

// routeKeys returns the reference names that can target route key.
func routeKeys(key string) []string {
	method, p := contracts.SplitRoute(key)
	if method != contracts.AnyMethod {
		return []string{key, contracts.AnyMethod + " " + p}
	}
	out := []string{key}
	for _, m := range contracts.Methods {
		out = append(out, m+" "+p)
	}
	return out
}

func (v *View) resolveMention(fc *fileCtx, rec segment.RefRec) ([]Symbol, string) {
	switch rec.Qualifier {
	case facts.MentionRoute:
		return v.resolveContract(facts.KindRoute, rec.Name)
	case facts.MentionPath:
		for _, p := range mentionPaths(fc.meta.Path, rec.Name) {
			if syms := v.topLevel(p); len(syms) > 0 {
				return capped(syms), ImportResolved
			}
		}
		return nil, ""
	case facts.MentionTicket:
		var out []Symbol
		for i := 0; i < v.sn.NumSegments(); i++ {
			seg := v.sn.Segment(i)
			lo, hi := seg.RefsTo(rec.Name)
			for r := lo; r < hi; r++ {
				if seg.RefKind(r) != facts.RefMention || !v.sn.Live(i, seg.RefFile(r)) {
					continue
				}
				if k := seg.RefEnclosing(r); k >= 0 {
					if s := v.symbol(i, k); s.File != fc.meta.Path {
						out = append(out, s)
					}
				}
			}
		}
		if len(out) == 0 {
			return nil, ""
		}
		return capped(out), NameMatched
	default: // symbol, ident
		syms := filter(v.Symbols(rec.Name, ""), func(s Symbol) bool { return s.Kind != facts.KindSection })
		switch len(syms) {
		case 0:
			return nil, ""
		case 1:
			return syms, NameMatched
		}
		return capped(definitionsFirst(syms)), Ambiguous
	}
}

// mentionPaths returns the root-relative paths a document at doc can mean
// by p: as written (root-relative), then relative to the document.
func mentionPaths(doc, p string) []string {
	p = strings.TrimPrefix(p, "./")
	out := []string{path.Clean(strings.TrimPrefix(p, "/"))}
	if rel := path.Join(path.Dir(doc), p); rel != out[0] && !strings.HasPrefix(rel, "../") {
		out = append(out, rel)
	}
	return out
}

// topLevel returns a file's top-level declarations, or nil when the file
// is not indexed.
func (v *View) topLevel(p string) []Symbol {
	var out []Symbol
	for _, s := range v.FileSymbols(p) {
		if s.Parent == "" {
			out = append(out, s)
		}
	}
	return out
}

// DocLink is one mention of code in a document section.
type DocLink struct {
	Section    Symbol // the mentioning section
	Kind       string // facts.Mention*: symbol, path, route, ticket, ident
	Text       string // as mentioned (a route as its key)
	Line       int
	File       string   // path mentions: the file, when indexed
	Targets    []Symbol // what the mention resolves to
	Resolution string
}

// Mentions returns what document section s mentions, in line order.
func (v *View) Mentions(s Symbol) []DocLink {
	i, k, ok := v.locate(s)
	if !ok {
		return nil
	}
	seg := v.sn.Segment(i)
	var out []DocLink
	for _, r := range seg.RefsFrom(k) {
		if seg.RefKind(r) != facts.RefMention {
			continue
		}
		out = append(out, v.docLink(i, r, s))
	}
	return out
}

func (v *View) docLink(i, r int, section Symbol) DocLink {
	rec := v.sn.Segment(i).Ref(r)
	targets, label := v.resolveRef(i, r)
	l := DocLink{Section: section, Kind: rec.Qualifier, Text: rec.Name, Line: rec.Line, Targets: targets, Resolution: label}
	if rec.Qualifier == facts.MentionPath && len(targets) > 0 {
		l.File = targets[0].File
	}
	return l
}

// MentionedBy returns the document mentions that resolve to declaration s:
// by its name, its qualified name (Type.Method) or its file's path.
func (v *View) MentionedBy(s Symbol) []DocLink {
	names := []string{s.Name}
	if q := s.Qualified(); q != s.Name {
		names = append(names, q)
	}
	if s.Kind == facts.KindRoute {
		names = routeKeys(s.Name)
	}
	names = append(names, s.File)
	seen := map[[2]int]bool{}
	var out []DocLink
	for i := 0; i < v.sn.NumSegments(); i++ {
		seg := v.sn.Segment(i)
		for _, name := range names {
			lo, hi := seg.RefsTo(name)
			for r := lo; r < hi; r++ {
				if seg.RefKind(r) != facts.RefMention || !v.sn.Live(i, seg.RefFile(r)) || seen[[2]int{i, r}] {
					continue
				}
				k := seg.RefEnclosing(r)
				if k < 0 {
					continue
				}
				targets, _ := v.resolveRef(i, r)
				if !targetsInclude(targets, s) && !(seg.Ref(r).Qualifier == facts.MentionPath && len(targets) > 0 && targets[0].File == s.File) {
					continue
				}
				seen[[2]int{i, r}] = true
				out = append(out, v.docLink(i, r, v.symbol(i, k)))
			}
		}
	}
	return out
}
