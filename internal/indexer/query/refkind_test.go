package query_test

import (
	"slices"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/query"
	"github.com/spawn08/chronos-code/internal/indexer/store"
)

// TestCallQueriesIgnoreOtherRefKinds checks that type uses, instantiations
// and other non-call references never appear as call sites, callers or
// callees (Outgoing, for graph expansion, also follows instantiations),
// while Stats counts every reference.
func TestCallQueriesIgnoreOtherRefKinds(t *testing.T) {
	st, err := store.Open(t.TempDir(), "/root", "test")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	f := &facts.File{
		Path: "web/a.ts", Lang: "typescript", Package: "web",
		Symbols: []facts.Symbol{
			{Name: "Repo", Kind: facts.KindClass, Line: 1, EndLine: 3, Exported: true, Visibility: facts.VisPublic},
			{Name: "use", Kind: facts.KindFunc, Line: 5, EndLine: 9},
		},
		Refs: []facts.Ref{
			{Kind: facts.RefTypeUse, Enclosing: 1, Name: "Repo", Line: 5, Col: 14},
			{Kind: facts.RefInstantiate, Enclosing: 1, Name: "Repo", Line: 6, Col: 13},
			{Kind: facts.RefDecorator, Enclosing: 1, Name: "trace", Line: 4, Col: 2},
			{Kind: facts.RefCall, Enclosing: 1, Name: "save", Qualifier: "r", QualKind: facts.QualExpr, Line: 7, Col: 5},
		},
	}
	if err := st.Publish([]*facts.File{f}, true); err != nil {
		t.Fatal(err)
	}
	sn := st.Snapshot()
	defer sn.Release()
	v := query.NewView(sn, query.NewCache())

	if got := v.CallSites("Repo"); len(got) != 0 {
		t.Errorf("CallSites(Repo) = %+v, want none", got)
	}
	if got := v.Callers([]string{"Repo", "trace"}); len(got) != 0 {
		t.Errorf("Callers = %v, want none", got)
	}
	if got := v.Callees("use"); !slices.Equal(got, []string{"save"}) {
		t.Errorf("Callees(use) = %v, want [save]", got)
	}
	use := v.Symbols("use", "")
	if len(use) != 1 {
		t.Fatalf("Symbols(use) = %+v", use)
	}
	// Outgoing follows calls and instantiations, not type uses or decorators.
	if got := v.Outgoing(use[0]); len(got) != 2 || got[0].Callee != "Repo" || got[1].Callee != "save" {
		t.Errorf("Outgoing(use) = %+v", got)
	}
	if got := v.CallSites("save"); len(got) != 1 || got[0].Caller == nil || got[0].Caller.Name != "use" {
		t.Errorf("CallSites(save) = %+v", got)
	}
	if st := v.Stats(); st.Refs != 4 {
		t.Errorf("Stats.Refs = %d, want 4", st.Refs)
	}
}
