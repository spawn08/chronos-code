// Package federation answers queries over several repositories' indexes
// (M9). Each repository keeps its own index; a Workspace is a set of
// query views, one snapshot each, and queries fan out across them.
//
// Relationships cross repositories two ways:
//
//   - Imports: a reference whose import names a module another member
//     owns (a Go module path, an npm or Cargo package name, a Java
//     package, a Python package) resolves into that member's index
//     (query.ExternalRef). Such edges are import_resolved.
//   - Contracts: a use of a route, RPC, message, topic or table joins the
//     contract nodes of the same key in the other members. RPCs and
//     messages declared in a contract file (proto, Thrift, GraphQL) also
//     carry their package: when both sides know it, it must match. A use
//     joining nodes in one other member is import_resolved; in several,
//     ambiguous.
//
// A Workspace with one member answers exactly as that member's view.
package federation

import (
	"cmp"
	"errors"
	"fmt"
	"slices"

	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/query"
)

// Edge provenance.
const (
	ViaLocal    = ""         // inside one repository
	ViaImport   = "import"   // an import of another repository's module
	ViaContract = "contract" // a contract joined by key across repositories
)

// Member is one repository's index.
type Member struct {
	Name string // unique, non-empty; identifies the repository in results
	View *query.View
}

// Workspace is a set of member indexes. Members are ordered; the first is
// the primary repository. It must not be used after the members' snapshots
// are released.
type Workspace struct {
	members []Member
	index   map[string]int
}

// New returns a workspace over members.
func New(members ...Member) (*Workspace, error) {
	if len(members) == 0 {
		return nil, errors.New("federation: no members")
	}
	w := &Workspace{members: members, index: make(map[string]int, len(members))}
	for i, m := range members {
		if m.Name == "" || m.View == nil {
			return nil, fmt.Errorf("federation: member %d needs a name and a view", i)
		}
		if _, dup := w.index[m.Name]; dup {
			return nil, fmt.Errorf("federation: duplicate member %q", m.Name)
		}
		w.index[m.Name] = i
	}
	return w, nil
}

// Members returns the members in order.
func (w *Workspace) Members() []Member { return w.members }

// Symbol is a declaration of one member.
type Symbol struct {
	Repo string
	query.Symbol
}

// Edge is a call, instantiation or contract use from one declaration to
// another, possibly across repositories.
type Edge struct {
	Caller, Callee Symbol
	Line           int    // site line in Caller.File
	Resolution     string // as query.CallEdge
	Candidates     int
	Via            string // ViaLocal, ViaImport or ViaContract
}

func (w *Workspace) tag(repo string, syms []query.Symbol) []Symbol {
	out := make([]Symbol, len(syms))
	for i, s := range syms {
		out[i] = Symbol{Repo: repo, Symbol: s}
	}
	return out
}

// Symbols returns the declarations named name in every member, in member
// order (each member's own order within).
func (w *Workspace) Symbols(name, kind string) []Symbol {
	var out []Symbol
	for _, m := range w.members {
		out = append(out, w.tag(m.Name, m.View.Symbols(name, kind))...)
	}
	return out
}

// Fuzzy returns every member's fuzzy matches, in member order.
func (w *Workspace) Fuzzy(substr string) []Symbol {
	var out []Symbol
	for _, m := range w.members {
		out = append(out, w.tag(m.Name, m.View.Fuzzy(substr))...)
	}
	return out
}

// Scored is a search hit of one member.
type Scored struct {
	Symbol
	Score float64
}

// Search returns the topK best hits across members. Each member scores
// its own corpus (BM25 statistics are per index), so scores compare only
// roughly across members; ties keep member order.
func (w *Workspace) Search(q string, topK int) []Scored {
	var out []Scored
	for _, m := range w.members {
		for _, h := range m.View.Search(q, topK) {
			out = append(out, Scored{Symbol: Symbol{Repo: m.Name, Symbol: h.Symbol}, Score: h.Score})
		}
	}
	slices.SortStableFunc(out, func(a, b Scored) int { return cmp.Compare(b.Score, a.Score) })
	if topK > 0 && len(out) > topK {
		out = out[:topK]
	}
	return out
}

// CallerEdges returns the edges into every declaration named name in any
// member. When no member declares it, each member's unresolved call sites
// of that name are returned (query.View.CallerEdges).
func (w *Workspace) CallerEdges(name string) []Edge {
	if targets := w.Symbols(name, ""); len(targets) > 0 {
		return w.IncomingEdges(targets)
	}
	var out []Edge
	for _, m := range w.members {
		out = append(out, w.local(m.Name, m.Name, m.View.CallerEdges(name))...)
	}
	return out
}

func (w *Workspace) local(callerRepo, calleeRepo string, in []query.CallEdge) []Edge {
	out := make([]Edge, len(in))
	for i, e := range in {
		out[i] = Edge{
			Caller: Symbol{Repo: callerRepo, Symbol: e.Caller}, Callee: Symbol{Repo: calleeRepo, Symbol: e.Callee},
			Line: e.Line, Resolution: e.Resolution, Candidates: e.Candidates,
		}
	}
	return out
}

// IncomingEdges returns the edges into targets: each target member's own
// (query.View.IncomingEdges), then calls from other members through
// imports, then uses of contract targets from other members. One edge per
// (caller, callee) pair, ordered by callee in targets order, then caller
// member, file and line.
func (w *Workspace) IncomingEdges(targets []Symbol) []Edge {
	byRepo, repos := w.group(targets)
	var out []Edge
	for _, repo := range repos {
		b := w.members[w.index[repo]]
		ts := byRepo[repo]
		out = append(out, w.local(repo, repo, b.View.IncomingEdges(ts))...)
		var specs []string
		for _, t := range ts {
			specs = append(specs, b.View.ImportSpecs(t)...)
		}
		slices.Sort(specs)
		specs = slices.Compact(specs)
		for _, a := range w.members {
			if a.Name == repo {
				continue
			}
			if refs := a.View.ExternalRefsTo(ts, specs); len(refs) > 0 {
				for _, e := range b.View.ExternalIncoming(ts, refs) {
					out = append(out, Edge{
						Caller: Symbol{Repo: a.Name, Symbol: e.Caller}, Callee: Symbol{Repo: repo, Symbol: e.Callee},
						Line: e.Line, Resolution: e.Resolution, Candidates: e.Candidates, Via: ViaImport,
					})
				}
			}
			for _, t := range ts {
				if !facts.ContractKinds[t.Kind] {
					continue
				}
				for _, use := range a.View.ContractUses(t.Kind, t.Name) {
					joined := w.contractJoin(a.Name, use.Kind, use.Key, use.Nodes)
					if !containsNode(joined, repo, t) {
						continue
					}
					label, n := joinLabel(joined)
					out = append(out, Edge{
						Caller: Symbol{Repo: a.Name, Symbol: use.Caller}, Callee: Symbol{Repo: repo, Symbol: t},
						Line: use.Line, Resolution: label, Candidates: n, Via: ViaContract,
					})
				}
			}
		}
	}
	return w.sorted(dedupe(out), targets)
}

// OutgoingEdges returns the edges from callers: each caller member's own
// calls (query.View.OutgoingEdges), then calls into other members through
// imports, then contract uses joined in other members. One edge per
// (caller, callee) pair, in callers order and then line order.
func (w *Workspace) OutgoingEdges(callers []Symbol) []Edge {
	var out []Edge
	for _, c := range callers {
		ai, ok := w.index[c.Repo]
		if !ok {
			continue
		}
		a := w.members[ai]
		first := len(out)
		out = append(out, w.local(a.Name, a.Name, a.View.OutgoingEdges([]query.Symbol{c.Symbol}))...)
		refs := a.View.ExternalRefsFrom([]query.Symbol{c.Symbol})
		for _, ref := range refs {
			for _, b := range w.members {
				if b.Name == a.Name {
					continue
				}
				targets, label := b.View.ResolveExternal(ref)
				for _, t := range targets {
					out = append(out, Edge{
						Caller: c, Callee: Symbol{Repo: b.Name, Symbol: t},
						Line: ref.Line, Resolution: label, Candidates: len(targets), Via: ViaImport,
					})
				}
			}
		}
		for _, call := range a.View.Outgoing(c.Symbol) {
			if call.Kind != facts.RefContract {
				continue
			}
			local, _ := a.View.Resolve(call)
			joined := w.contractJoin(a.Name, call.Qualifier, call.Callee, local)
			label, n := joinLabel(joined)
			for _, j := range joined {
				out = append(out, Edge{Caller: c, Callee: j, Line: call.Line, Resolution: label, Candidates: n, Via: ViaContract})
			}
		}
		kept := dedupe(out[first:])
		slices.SortStableFunc(kept, func(x, y Edge) int { return cmp.Compare(x.Line, y.Line) })
		out = append(out[:first], kept...)
	}
	return out
}

// Callees returns the declarations called by the declarations named by
// qualified identity q in every member (as OutgoingEdges resolves them).
func (w *Workspace) Callees(q string) []Symbol {
	var decls []Symbol
	for _, m := range w.members {
		for _, s := range m.View.ByQualified([]string{q})[q] {
			switch s.Kind {
			case facts.KindFunc, facts.KindMethod, facts.KindVar, facts.KindConstructor:
				decls = append(decls, Symbol{Repo: m.Name, Symbol: s})
			}
		}
	}
	seen := map[[2]any]bool{}
	var out []Symbol
	for _, e := range w.OutgoingEdges(decls) {
		k := [2]any{e.Callee.Repo, e.Callee.ID}
		if !seen[k] {
			seen[k] = true
			out = append(out, e.Callee)
		}
	}
	return out
}

// contractJoin returns the contract nodes of the members other than from
// that a use of kind/key joins. local are the nodes the use resolves to
// in its own member: when they carry a package (query.ContractPackage),
// nodes that carry a different one do not join.
func (w *Workspace) contractJoin(from, kind, key string, local []query.Symbol) []Symbol {
	want := map[string]bool{}
	for _, n := range local {
		if p := query.ContractPackage(n); p != "" {
			want[p] = true
		}
	}
	var out []Symbol
	for _, m := range w.members {
		if m.Name == from {
			continue
		}
		nodes := m.View.ContractNodes(kind, key)
		if len(want) > 0 {
			// A member's nodes join when one of its package-carrying nodes
			// matches, or when none carries a package (recogniser nodes).
			has, match := false, false
			for _, n := range nodes {
				if p := query.ContractPackage(n); p != "" {
					has = true
					match = match || want[p]
				}
			}
			if has && !match {
				continue
			}
			nodes = slices.DeleteFunc(slices.Clone(nodes), func(n query.Symbol) bool {
				p := query.ContractPackage(n)
				return p != "" && !want[p]
			})
		}
		out = append(out, w.tag(m.Name, nodes)...)
	}
	return out
}

// joinLabel labels a contract join: import_resolved when the nodes are in
// one member, ambiguous across several; and the node count.
func joinLabel(joined []Symbol) (string, int) {
	repos := map[string]bool{}
	for _, j := range joined {
		repos[j.Repo] = true
	}
	if len(repos) > 1 {
		return query.Ambiguous, len(joined)
	}
	return query.ImportResolved, max(1, len(joined))
}

func containsNode(joined []Symbol, repo string, t query.Symbol) bool {
	for _, j := range joined {
		if j.Repo == repo && j.ID == t.ID {
			return true
		}
	}
	return false
}

// group splits targets by member, keeping first-seen member order.
// Targets of unknown members are dropped.
func (w *Workspace) group(targets []Symbol) (map[string][]query.Symbol, []string) {
	by := map[string][]query.Symbol{}
	var repos []string
	for _, t := range targets {
		if _, ok := w.index[t.Repo]; !ok {
			continue
		}
		if _, ok := by[t.Repo]; !ok {
			repos = append(repos, t.Repo)
		}
		by[t.Repo] = append(by[t.Repo], t.Symbol)
	}
	return by, repos
}

type edgeKey struct {
	callerRepo, calleeRepo string
	caller, callee         uint64
}

var labelRank = map[string]int{
	query.TypeChecked: 0, query.ImportResolved: 1, query.TypeHinted: 2, query.NameMatched: 3, query.Ambiguous: 4, query.Unresolved: 5,
}

// dedupe keeps one edge per (caller, callee): the strongest label, then
// the earliest line. First-seen order is kept.
func dedupe(in []Edge) []Edge {
	at := map[edgeKey]int{}
	out := make([]Edge, 0, len(in))
	for _, e := range in {
		k := edgeKey{e.Caller.Repo, e.Callee.Repo, e.Caller.ID, e.Callee.ID}
		if j, ok := at[k]; ok {
			old := out[j]
			if a, b := labelRank[e.Resolution], labelRank[old.Resolution]; a < b || a == b && e.Line < old.Line {
				out[j] = e
			}
			continue
		}
		at[k] = len(out)
		out = append(out, e)
	}
	return out
}

// sorted orders edges by callee in targets order, then caller member, file
// and line.
func (w *Workspace) sorted(edges []Edge, targets []Symbol) []Edge {
	type tk struct {
		repo string
		id   uint64
	}
	order := map[tk]int{}
	for i, t := range targets {
		if _, ok := order[tk{t.Repo, t.ID}]; !ok {
			order[tk{t.Repo, t.ID}] = i
		}
	}
	slices.SortStableFunc(edges, func(a, b Edge) int {
		if c := cmp.Compare(order[tk{a.Callee.Repo, a.Callee.ID}], order[tk{b.Callee.Repo, b.Callee.ID}]); c != 0 {
			return c
		}
		if c := cmp.Compare(w.index[a.Caller.Repo], w.index[b.Caller.Repo]); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Caller.File, b.Caller.File); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Line, b.Line); c != 0 {
			return c
		}
		return cmp.Compare(a.Caller.Name, b.Caller.Name)
	})
	return edges
}
