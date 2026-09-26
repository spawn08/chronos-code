package treesitter

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/indexer/extract/packs"
)

// samples holds one small, valid file per pack.
var samples = map[string]string{
	"typescript": "import { x } from './x';\nexport class A implements B { run(n: number): string { return x(n); } }\n",
	"tsx":        "export function App(p: { n: number }) { return <div>{p.n}</div>; }\n",
	"javascript": "import x from './x.js';\nexport function f(a) { return x(a); }\nclass C { m() { f(1); } }\n",
	"python":     "import os\nfrom .a import b as c\n\nclass K(Base):\n    def run(self, n):\n        return c(n)\n",
	"java":       "package p;\nimport java.util.List;\npublic class A implements B { public int f(List<String> x) { return g(x); } }\n",
	"kotlin":     "package p\nimport a.B\nclass A : B() { fun f(x: Int): Int = g(x) }\n",
	"scala":      "package p\nimport a.B\nclass A extends B { def f(x: Int): Int = g(x) }\n",
	"c":          "#include <stdio.h>\nstatic int add(int a, int b) { return a + b; }\nint main(void) { return add(1, 2); }\n",
	"cpp":        "#include <vector>\nnamespace n { class A : public B { public: int f(int x) { return g(x); } }; }\n",
	"objc":       "#import <Foundation/Foundation.h>\n@interface A : NSObject\n- (int)f:(int)x;\n@end\n@implementation A\n- (int)f:(int)x { return x; }\n@end\n",
	"csharp":     "using System;\nnamespace N { public class A : IB { public int F(int x) { return G(x); } } }\n",
	"rust":       "use crate::a::B;\npub struct S;\nimpl B for S { fn f(&self, x: i32) -> i32 { g(x) } }\n",
	"swift":      "import Foundation\nclass A: B {\n  func f(_ x: Int) -> Int { return g(x) }\n}\n",
	"ruby":       "require 'x'\nmodule M\n  class A < B\n    def f(x)\n      g(x)\n    end\n  end\nend\n",
	"php":        "<?php\nnamespace N;\nuse A\\B;\nclass C extends B { public function f($x) { return g($x); } }\n",
	"dart":       "import 'package:a/b.dart';\nclass A extends B {\n  int f(int x) => g(x);\n}\n",
	"bash":       "#!/bin/sh\ngreet() {\n  echo \"hi $1\"\n}\ngreet world\n",
}

func TestEveryPackParses(t *testing.T) {
	reg, err := packs.Load()
	if err != nil {
		t.Fatal(err)
	}
	rt := New(Options{})
	for _, p := range reg.Packs() {
		src, ok := samples[p.ID]
		if !ok {
			t.Errorf("no sample for pack %s", p.ID)
			continue
		}
		tree, err := rt.Parse(p.Grammar, []byte(src))
		if err != nil {
			t.Errorf("%s: %v", p.ID, err)
			continue
		}
		if tree.Root() == nil || tree.Root().HasError() {
			t.Errorf("%s: syntax errors in a valid sample: %s", p.ID, tree.Root().SExpr(tree.Lang))
		}
		tree.Release()
	}
}

func TestDeadlineStopsParse(t *testing.T) {
	rt := New(Options{Timeout: time.Microsecond})
	src := []byte(strings.Repeat("def f(x):\n    return [g(y) for y in x if y]\n\n", 20000))
	tree, err := rt.Parse("python", src)
	if !errors.Is(err, ErrIncomplete) {
		if tree != nil {
			tree.Release()
		}
		t.Fatalf("err = %v, want ErrIncomplete", err)
	}
	if tree != nil {
		t.Fatal("incomplete parse returned a tree")
	}
}

// TestFailedRecoveryKeepsPartialTree pins gotreesitter v0.55.0 behaviour:
// Flow annotations are invalid JavaScript, and instead of wrapping them in
// an ERROR node (as the C runtime does) the parser stops. The prefix it
// parsed is kept. If an upgrade recovers here, the tree is complete instead;
// update the test and the "M6 parser runtime spike" notes.
func TestFailedRecoveryKeepsPartialTree(t *testing.T) {
	src := []byte("export function f(a) { return a; }\nexport interface I {\n  +x?: boolean;\n}\nexport function g() { return 1; }\n")
	tree, err := New(Options{}).Parse("javascript", src)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Release()
	if !tree.Partial || tree.Covered <= 0 || tree.Covered >= len(src) {
		t.Fatalf("partial=%v covered=%d/%d, want a partial prefix", tree.Partial, tree.Covered, len(src))
	}
	first := tree.Root().NamedChild(0)
	if first == nil || first.HasError() || first.Type(tree.Lang) != "export_statement" {
		t.Fatalf("first declaration not intact: %s", tree.Root().SExpr(tree.Lang))
	}
}

func TestFullParseCoversSource(t *testing.T) {
	src := []byte(samples["python"])
	tree, err := New(Options{}).Parse("python", src)
	if err != nil {
		t.Fatal(err)
	}
	defer tree.Release()
	if tree.Partial || tree.Covered != len(src) {
		t.Fatalf("partial=%v covered=%d/%d", tree.Partial, tree.Covered, len(src))
	}
}

func TestUnsupportedGrammar(t *testing.T) {
	rt := New(Options{})
	if _, err := rt.Parse("no-such-grammar", []byte("x")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if _, err := rt.Language("no-such-grammar"); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Language err = %v, want ErrUnsupported", err)
	}
}

func TestConcurrentParse(t *testing.T) {
	rt := New(Options{})
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			grammar, src := "python", samples["python"]
			if i%2 == 1 {
				grammar, src = "java", samples["java"]
			}
			tree, err := rt.Parse(grammar, []byte(src))
			if err != nil {
				errs <- err
				return
			}
			if tree.Root().HasError() {
				errs <- errors.New(grammar + ": syntax errors")
			}
			tree.Release()
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}
