package query_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/query"
)

// resolved maps "callee@line" to "label targets" for the calls made inside
// the declaration named from (qualified identity).
func resolved(t *testing.T, v *query.View, from string) map[string]string {
	t.Helper()
	syms := v.ByQualified([]string{from})[from]
	if len(syms) != 1 {
		t.Fatalf("%s: %d declarations", from, len(syms))
	}
	out := map[string]string{}
	for _, c := range v.Outgoing(syms[0]) {
		targets, label := v.Resolve(c)
		out[fmt.Sprintf("%s@%d", c.Callee, c.Line)] = strings.TrimSpace(label + " " + strings.Join(qualified(targets), ","))
	}
	return out
}

func expect(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q", k, got[k], w)
		}
	}
	if t.Failed() {
		keys := make([]string, 0, len(got))
		for k, g := range got {
			keys = append(keys, k+" -> "+g)
		}
		slices.Sort(keys)
		t.Logf("all: %s", strings.Join(keys, "; "))
	}
}

func TestResolveGo(t *testing.T) {
	v := newFixture(t, shopFiles).view(t)
	expect(t, resolved(t, v, "Handle"), map[string]string{
		"NewRepo@6": "import_resolved NewRepo",
		// r := store.NewRepo() returns Repo: the interface method, not every Save.
		"Save@7":  "type_hinted Repo.Save",
		"Order@7": "import_resolved Order",
	})
	expect(t, resolved(t, v, "memRepo.Save"), map[string]string{
		"helper@20": "type_hinted memRepo.helper",
	})
	helper := v.ByQualified([]string{"memRepo.helper"})["memRepo.helper"][0]
	in := v.IncomingCalls(helper)
	if len(in) != 1 || in[0].Caller.Qualified() != "memRepo.Save" || in[0].Resolution != query.TypeHinted {
		t.Fatalf("incoming calls of helper = %+v", in)
	}
}

func TestResolvePython(t *testing.T) {
	v := newFixture(t, map[string]string{
		"app/models.py": "class Repo:\n    def save(self):\n        pass\n\n\nclass Cache:\n    def get(self):\n        pass\n",
		"app/service.py": `from app.models import Repo
from . import models
import os


class Service:
    def __init__(self, repo: Repo):
        self.repo = repo
        self.cache = models.Cache()

    def run(self):
        self.repo.save()
        self.cache.get()
        os.path.join("a")
        helper()
        Repo()
        self.stop()

    def stop(self):
        pass


def helper():
    pass
`,
	}).view(t)
	got := resolved(t, v, "Service.run")
	expect(t, got, map[string]string{
		"save@12":   "type_hinted Repo.save",
		"get@13":    "type_hinted Cache.get",
		"helper@15": "import_resolved helper",
		"Repo@16":   "import_resolved Repo",
		"stop@17":   "type_hinted Service.stop",
	})
	if g, ok := got["join@14"]; ok && g != "" {
		t.Errorf("os.path.join resolved into the workspace: %q", g)
	}
	save := v.ByQualified([]string{"Repo.save"})["Repo.save"][0]
	if in := v.IncomingCalls(save); len(in) != 1 || in[0].Caller.Qualified() != "Service.run" || in[0].Resolution != query.TypeHinted {
		t.Fatalf("incoming calls of Repo.save = %+v", in)
	}
}

func TestResolveTypeScript(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/repo.ts": "export class Repo {\n  save(): void {}\n}\n\nexport function make(): Repo {\n  return new Repo();\n}\n",
		"src/app.ts": `import { Repo, make } from './repo';
import * as path from 'path';

class App {
  constructor(private repo: Repo) {}

  run(): void {
    this.repo.save();
    const q = new Repo();
    q.save();
    make();
    path.join('a');
  }
}
`,
	}).view(t)
	got := resolved(t, v, "App.run")
	expect(t, got, map[string]string{
		"save@8":  "type_hinted Repo.save",
		"Repo@9":  "import_resolved Repo",
		"save@10": "type_hinted Repo.save",
		"make@11": "import_resolved make",
	})
	if g := got["join@12"]; g != "" {
		t.Errorf("path.join resolved into the workspace: %q", g)
	}
}

func TestResolveJava(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/com/ex/model/Repo.java": "package com.ex.model;\n\npublic class Repo {\n    public void save() {}\n\n    public static Repo create() { return new Repo(); }\n}\n",
		"src/com/ex/app/App.java": `package com.ex.app;

import com.ex.model.Repo;

public class App {
    private Repo repo;

    void run() {
        repo.save();
        Repo r = Repo.create();
        r.save();
        helper();
    }

    void helper() {}
}
`,
	}).view(t)
	expect(t, resolved(t, v, "App.run"), map[string]string{
		"save@9":    "type_hinted Repo.save",
		"create@10": "import_resolved Repo.create",
		"save@11":   "type_hinted Repo.save",
		"helper@12": "import_resolved App.helper",
	})
}

// TestResolvePythonSuperOptionalAndNearest covers super() calls, Optional
// and forward-reference annotations, and a name defined twice in one file.
func TestResolvePythonSuperOptionalAndNearest(t *testing.T) {
	v := newFixture(t, map[string]string{
		"pkg/core.py": `import typing as t


class Base:
    def __init__(self):
        pass


class Context:
    def close(self):
        pass


class Child(Base):
    def __init__(self, ctx: t.Optional["Context"]):
        super().__init__()
        ctx.close()


def first():
    class Foo:
        pass
    return Foo()


def second():
    class Foo:
        pass
    return Foo()
`,
	}).view(t)
	expect(t, resolved(t, v, "Child.__init__"), map[string]string{
		"__init__@16": "type_hinted Base.__init__",
		"close@17":    "type_hinted Context.close",
	})
	got := resolved(t, v, "second")
	if !strings.HasPrefix(got["Foo@29"], "import_resolved Foo") {
		t.Fatalf("Foo in second = %q", got["Foo@29"])
	}
	foo := v.Outgoing(v.ByQualified([]string{"second"})["second"][0])
	targets, _ := v.Resolve(foo[0])
	if len(targets) != 2 || targets[0].Line != 27 {
		t.Fatalf("nearest Foo first: %+v", targets)
	}
}

// edgeList renders edges as "caller->callee label" in order.
func edgeList(edges []query.CallEdge) []string {
	out := make([]string, len(edges))
	for i, e := range edges {
		out[i] = e.Caller.Qualified() + "->" + e.Callee.Qualified() + " " + e.Resolution
	}
	return out
}

// TestCallerEdgesResolved checks that callers come from resolution, not
// the callee's name: a same-named method on another type is not a caller,
// each edge carries its label, a constructor is reached through new T(),
// and a name with no declaration falls back to its call sites, labelled
// unresolved.
func TestCallerEdgesResolved(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/shop/Repo.java": `package shop;

public class Repo {
    public Repo() {}
    public void save() {}
}
`,
		"src/main/java/shop/Cache.java": `package shop;

public class Cache {
    public void save() {}
}
`,
		"src/main/java/shop/Service.java": `package shop;

public class Service {
    private Repo repo = new Repo();
    private Cache cache;

    public void run() {
        repo.save();
        cache.save();
        System.out.println("x");
    }

    public void again() {
        repo.save();
        repo.save();
    }

    public Repo make() {
        return new Repo();
    }
}
`,
	}).view(t)
	repoSave := v.ByQualified([]string{"Repo.save"})["Repo.save"]
	if len(repoSave) != 1 {
		t.Fatalf("Repo.save = %+v", repoSave)
	}
	got := edgeList(v.IncomingEdges(repoSave))
	want := []string{"Service.run->Repo.save type_hinted", "Service.again->Repo.save type_hinted"}
	if !slices.Equal(got, want) {
		t.Fatalf("IncomingEdges(Repo.save) = %q, want %q", got, want)
	}
	// By name: both save methods are declarations, each with its own caller.
	got = edgeList(v.CallerEdges("save"))
	slices.Sort(got)
	want = []string{"Service.again->Repo.save type_hinted", "Service.run->Cache.save type_hinted", "Service.run->Repo.save type_hinted"}
	if !slices.Equal(got, want) {
		t.Fatalf("CallerEdges(save) = %q, want %q", got, want)
	}
	// new Repo() reaches the constructor; the class has no call edges.
	got = edgeList(v.CallerEdges("Repo"))
	if !slices.Contains(got, "Service.make->Repo.Repo import_resolved") || slices.ContainsFunc(got, func(e string) bool { return strings.HasSuffix(e, "->Repo import_resolved") }) {
		t.Fatalf("CallerEdges(Repo) = %q", got)
	}
	if got := edgeList(v.CallerEdges("println")); len(got) != 1 || got[0] != "Service.run->println unresolved" {
		t.Fatalf("CallerEdges(println) = %q", got)
	}
	if got := v.Callees("Service.run"); !slices.Equal(got, []string{"Cache.save", "Repo.save"}) {
		t.Fatalf("Callees(Service.run) = %v", got)
	}
}

func TestImplementationsDeclared(t *testing.T) {
	v := newFixture(t, map[string]string{
		"web/store.ts": `export interface Store {
  get(key: string): string;
}

export interface CachedStore extends Store {
  flush(): void;
}
`,
		"web/impl.ts": `import { Store, CachedStore } from "./store";

export class MemStore implements Store {
  get(key: string): string { return key; }
}

export class LruStore implements CachedStore {
  get(key: string): string { return key; }
  flush(): void {}
}

export class TinyLru extends LruStore {}

class Unrelated {
  get(key: string): string { return key; }
}
`,
	}).view(t)
	if got := v.Implementations("Store"); !slices.Equal(got, []string{"LruStore", "MemStore", "TinyLru"}) {
		t.Fatalf("Implementations(Store) = %v", got)
	}
	store := v.Symbols("Store", "interface")
	if len(store) != 1 {
		t.Fatalf("Store = %+v", store)
	}
	subs := v.Subtypes(store[0])
	if len(subs) != 2 || subs[0].Resolution != query.ImportResolved {
		t.Fatalf("Subtypes(Store) = %+v", subs)
	}
}

func TestIsTestAcrossLanguages(t *testing.T) {
	v := newFixture(t, map[string]string{
		"app/calc.py":                 "def add(a, b):\n    return a + b\n",
		"tests/test_calc.py":          "from app.calc import add\n\n\ndef test_add():\n    assert add(1, 2) == 3\n\n\ndef helper():\n    pass\n",
		"src/test/java/CalcTest.java": "import org.junit.Test;\n\npublic class CalcTest {\n    @Test\n    public void adds() {}\n\n    void util() {}\n}\n",
	}).view(t)
	for name, want := range map[string]bool{"test_add": true, "helper": false, "adds": true, "util": false, "add": false} {
		syms := v.Symbols(name, "")
		if len(syms) != 1 {
			t.Fatalf("%s: %+v", name, syms)
		}
		if got := query.IsTest(syms[0]); got != want {
			t.Errorf("IsTest(%s) = %v, want %v", name, got, want)
		}
		if name != "add" && !syms[0].TestFile {
			t.Errorf("%s: TestFile = false", name)
		}
	}
	add := v.Symbols("add", "")[0]
	if in := v.IncomingEdges([]query.Symbol{add}); len(in) != 1 || !query.IsTest(in[0].Caller) || in[0].Resolution != query.ImportResolved {
		t.Fatalf("IncomingEdges(add) = %+v", in)
	}
}
