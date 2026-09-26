package query_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/indexer"
	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/query"
	"github.com/spawn08/chronos-code/indexer/scip"
	"github.com/spawn08/chronos-code/indexer/scip/sciptest"
)

// Two same-named methods: the syntactic resolver cannot tell which run()
// main.ts calls; the SCIP index can.
const (
	scipA = `export class Alpha {
  run(): number { return 1; }
}
`
	scipB = `export class Beta {
  run(): number { return 2; }
}
`
	scipMain = `import { Alpha } from "./a";
import { Beta } from "./b";

export function pick(flag: boolean): number {
  const x: any = flag ? new Alpha() : new Beta();
  return x.run();
}
`
	scipPkg   = "scip-typescript npm app 1.0.0 "
	alphaSym  = scipPkg + "`a.ts`/Alpha#"
	alphaRun  = scipPkg + "`a.ts`/Alpha#run()."
	betaSym   = scipPkg + "`b.ts`/Beta#"
	betaRun   = scipPkg + "`b.ts`/Beta#run()."
	pickSym   = scipPkg + "`main.ts`/pick()."
	alphaCtor = scipPkg + "`a.ts`/Alpha#`<constructor>`()."
)

func scipDocs() []sciptest.Document {
	return []sciptest.Document{
		{Path: "a.ts", Language: "typescript", Occurrences: []sciptest.Occurrence{
			sciptest.At(scipA, "Alpha", 0, alphaSym, scip.RoleDefinition),
			sciptest.At(scipA, "run", 0, alphaRun, scip.RoleDefinition),
		}},
		{Path: "b.ts", Language: "typescript", Occurrences: []sciptest.Occurrence{
			sciptest.At(scipB, "Beta", 0, betaSym, scip.RoleDefinition),
			sciptest.At(scipB, "run", 0, betaRun, scip.RoleDefinition),
		}},
		{Path: "main.ts", Language: "typescript", Occurrences: []sciptest.Occurrence{
			sciptest.At(scipMain, "Alpha", 0, alphaSym, scip.RoleImport),
			sciptest.At(scipMain, "Beta", 0, betaSym, scip.RoleImport),
			sciptest.At(scipMain, "pick", 0, pickSym, scip.RoleDefinition),
			sciptest.At(scipMain, "Alpha", 1, alphaSym, 0), // new Alpha(): the class (no constructor declared)
			sciptest.At(scipMain, "Beta", 1, betaSym, 0),
			sciptest.At(scipMain, "run", 0, betaRun, 0), // what the checker says x.run() calls
		}},
	}
}

func newSCIPFixture(t *testing.T) (root string, eng *indexer.Engine, cache *query.Cache) {
	t.Helper()
	root = t.TempDir()
	old := time.Now().Add(-time.Hour)
	for name, src := range map[string]string{"a.ts": scipA, "b.ts": scipB, "main.ts": scipMain} {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = os.Chtimes(p, old, old)
	}
	if err := sciptest.Write(filepath.Join(root, "index.scip"), "file:///ci/app", scipDocs()...); err != nil {
		t.Fatal(err)
	}
	eng, err := indexer.Open(indexer.Options{Root: root, Dir: t.TempDir(), SCIP: true, SCIPPoll: time.Hour, SCIPQuiet: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()
	if _, err := eng.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.RunSCIP(ctx); err != nil {
		t.Fatal(err)
	}
	return root, eng, query.NewCache().SetSCIP(eng.SCIP())
}

func refsNamed(t *testing.T, eng *indexer.Engine, cache *query.Cache, file, name string) []query.ResolvedRef {
	t.Helper()
	sn := eng.Snapshot()
	defer sn.Release()
	v := query.NewView(sn, cache)
	var out []query.ResolvedRef
	v.EachReference([]uint8{facts.RefCall, facts.RefInstantiate}, func(r query.ResolvedRef) {
		if r.File == file && r.Name == name {
			out = append(out, r)
		}
	})
	return out
}

func TestSCIPResolvesTypeChecked(t *testing.T) {
	root, eng, cache := newSCIPFixture(t)
	if st := eng.SCIPStatus(); st.State != indexer.SCIPReady || st.Docs != 3 || st.Rejected != 0 || st.Indexes != 1 {
		t.Fatalf("status %+v", st)
	}
	runs := refsNamed(t, eng, cache, "main.ts", "run")
	if len(runs) != 1 {
		t.Fatalf("run refs %+v", runs)
	}
	r := runs[0]
	if r.Resolution != query.TypeChecked || len(r.Targets) != 1 || r.Targets[0].File != "b.ts" || r.Targets[0].Name != "run" {
		t.Fatalf("x.run(): %s %+v", r.Resolution, r.Targets)
	}
	alpha := refsNamed(t, eng, cache, "main.ts", "Alpha")
	if len(alpha) != 1 || alpha[0].Resolution != query.TypeChecked || len(alpha[0].Targets) != 1 || alpha[0].Targets[0].File != "a.ts" {
		t.Fatalf("new Alpha(): %+v", alpha)
	}
	// Without the facts the same site is syntactic.
	if r := refsNamed(t, eng, query.NewCache(), "main.ts", "run")[0]; r.Resolution == query.TypeChecked {
		t.Fatalf("syntactic view: %s", r.Resolution)
	}

	// Editing main.ts makes its facts stale: syntactic again, until a
	// new index is imported.
	ctx := context.Background()
	edited := "// edited\n" + scipMain
	if err := os.WriteFile(filepath.Join(root, "main.ts"), []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Update(ctx, []string{"main.ts"}); err != nil {
		t.Fatal(err)
	}
	if r := refsNamed(t, eng, cache, "main.ts", "run")[0]; r.Resolution == query.TypeChecked {
		t.Fatalf("stale facts used: %s %+v", r.Resolution, r.Targets)
	}

	// Editing the target's file moves the definition line: located by a
	// unique name in that file, or not at all.
	if err := os.WriteFile(filepath.Join(root, "main.ts"), []byte(scipMain), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.ts"), []byte("// moved\n\n"+scipB), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Update(ctx, []string{"main.ts", "b.ts"}); err != nil {
		t.Fatal(err)
	}
	r = refsNamed(t, eng, cache, "main.ts", "run")[0]
	if r.Resolution != query.TypeChecked || len(r.Targets) != 1 || r.Targets[0].File != "b.ts" || r.Targets[0].Line != 4 {
		t.Fatalf("after moving the target: %s %+v", r.Resolution, r.Targets)
	}
}

// A C++ call of a member overloaded with another parameter count keeps
// its syntactic answer (selected by argument count), whatever scip-clang
// says; a call of a member without such overloads uses SCIP's.
func TestSCIPCppOverloadsKeepSyntacticAnswer(t *testing.T) {
	src := `struct Doc {
  int Parse();
  int Parse(const char* xml);
  int Save(const char* path);
};
int Doc::Parse() { return 0; }
int Doc::Parse(const char* xml) { return 1; }
int Doc::Save(const char* path) { return 2; }
int use(Doc& d) { return d.Parse("x") + d.Save("y"); }
`
	root := t.TempDir()
	p := filepath.Join(root, "doc.cpp")
	if err := os.WriteFile(p, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	_ = os.Chtimes(p, old, old)
	parse0 := "cxx . . $ Doc#Parse(aaaa)."
	parse1 := "cxx . . $ Doc#Parse(bbbb)."
	save := "cxx . . $ Doc#Save(cccc)."
	// The wrong overload for d.Parse("x"), as scip-clang sometimes picks.
	err := sciptest.Write(filepath.Join(root, "index.scip"), "file:///ci", sciptest.Document{Path: "doc.cpp", Occurrences: []sciptest.Occurrence{
		sciptest.At(src, "Parse", 2, parse0, scip.RoleDefinition),
		sciptest.At(src, "Parse", 3, parse1, scip.RoleDefinition),
		sciptest.At(src, "Save", 1, save, scip.RoleDefinition),
		sciptest.At(src, "Parse", 4, parse0, 0),
		sciptest.At(src, "Save", 2, save, 0),
	}})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := indexer.Open(indexer.Options{Root: root, Dir: t.TempDir(), SCIP: true, SCIPPoll: time.Hour, SCIPQuiet: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	ctx := context.Background()
	if _, err := eng.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := eng.RunSCIP(ctx); err != nil {
		t.Fatal(err)
	}
	cache := query.NewCache().SetSCIP(eng.SCIP())
	parse := refsNamed(t, eng, cache, "doc.cpp", "Parse")
	if len(parse) != 1 || parse[0].Resolution == query.TypeChecked || len(parse[0].Targets) == 0 || parse[0].Targets[0].Line != 7 {
		t.Fatalf("d.Parse(\"x\"): %+v", parse)
	}
	saves := refsNamed(t, eng, cache, "doc.cpp", "Save")
	if len(saves) != 1 || saves[0].Resolution != query.TypeChecked || saves[0].Targets[0].Line != 8 {
		t.Fatalf("d.Save(\"y\"): %+v", saves)
	}
}
