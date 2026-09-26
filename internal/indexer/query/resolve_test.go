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
