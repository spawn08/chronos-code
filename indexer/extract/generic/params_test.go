package generic

import (
	"fmt"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/extract/packs"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

func arityString(a facts.Arity) string {
	switch {
	case !a.Known:
		return "?"
	case a.Max == facts.VarArgs:
		return fmt.Sprintf("%d..", a.Min)
	}
	return fmt.Sprintf("%d..%d", a.Min, a.Max)
}

// TestParseArity covers the parameter-list shapes of the pack languages.
// The text is what follows a definition's name.
func TestParseArity(t *testing.T) {
	self := packs.Params{Receiver: []string{"self", "cls"}}
	kotlin := packs.Params{Variadic: []string{"vararg"}}
	csharp := packs.Params{Variadic: []string{"params"}}
	dart := packs.Params{OptionalGroups: true}
	for _, c := range []struct {
		text   string
		method bool
		rules  packs.Params
		want   string
	}{
		{"()", false, packs.Params{}, "0..0"},
		{"(String a, int b) {", false, packs.Params{}, "2..2"},
		{"(Map<String, List<Integer>> m, int b)", false, packs.Params{}, "2..2"},
		{"(String... xs)", false, packs.Params{}, "0.."},
		{"(int a, int b = 2, ...)", false, packs.Params{}, "1.."},
		{"(void)", false, packs.Params{}, "0..0"},
		{"(const char* s = \"a,b\", char c = ',')", false, packs.Params{}, "0..2"},
		{"(a: Int = 1, vararg b: String)", false, kotlin, "0.."},
		{"(block: T.() -> Unit = {}): T", false, kotlin, "0..1"},
		{"(f: (Int, Int) -> Boolean, x: Int)", false, kotlin, "2..2"},
		{"(@Ann(x = 1) a: Int)", false, kotlin, "1..1"},
		{"(this IServices s, params int[] xs)", false, csharp, "1.."},
		{"(int a, bool b = false)", false, csharp, "1..2"},
		{"<T extends Base<X>>(a: T, b?: string): T", false, packs.Params{}, "1..2"},
		{"(cb: (x: number) => void, ...rest: any[])", false, packs.Params{}, "1.."},
		{"(self, a, b=1, *args, **kw)", true, self, "1.."},
		{"(cls, a, /, b)", true, self, "2..2"},
		{"(self)", false, self, "1..1"}, // not a method: self is a parameter
		{"<'a>(&'a self, key: &str) -> Option<&'a str>", true, packs.Params{Receiver: []string{"self"}}, "1..1"},
		{"(&mut self, key: &str)", true, packs.Params{Receiver: []string{"self"}}, "1..1"},
		{"(self: Box<Self>)", true, packs.Params{Receiver: []string{"self"}}, "0..0"},
		{"(int a, [int b = 0, int c])", false, dart, "1..3"},
		{"(int a, {required int b, int c})", false, dart, "2..3"},
		{"(a, b,)", false, packs.Params{}, "2..2"},
		{"(int a, // first, one\n int b)", false, packs.Params{}, "2..2"},
		{"(int a /* x, y */, int b)", false, packs.Params{}, "2..2"},
		{"(a: Int)(b: Int)", false, packs.Params{}, "?"}, // curried
		{" = (a, b) =>", false, packs.Params{}, "?"},
		{"(int a", false, packs.Params{}, "?"},
		{"", false, packs.Params{}, "?"},
	} {
		if got := arityString(parseArity([]byte(c.text), c.method, c.rules)); got != c.want {
			t.Errorf("parseArity(%q) = %s, want %s", c.text, got, c.want)
		}
	}
}

func TestAccepts(t *testing.T) {
	a := facts.Arity{Min: 1, Max: 2, Known: true}
	if a.Accepts(0) || !a.Accepts(1) || !a.Accepts(2) || a.Accepts(3) {
		t.Fatal("1..2")
	}
	v := facts.Arity{Min: 1, Max: facts.VarArgs, Known: true}
	if v.Accepts(0) || !v.Accepts(9) {
		t.Fatal("1..")
	}
	if !(facts.Arity{}).Accepts(7) {
		t.Fatal("unknown arity must accept any count")
	}
}

// TestArgCounts extracts calls in each language with a call rule.
func TestArgCounts(t *testing.T) {
	for _, c := range []struct {
		path, src string
		want      []string // name=args in source order; "?" when not counted
	}{
		{"A.java", "class A { void f() { g(1, h(2)); a.b(x).c(); new A(1, /* c */ 2); } }",
			[]string{"g=2", "h=1", "b=1", "c=0", "A=2"}},
		{"a.kt", "class A { fun f() { g(1, 2) { it }; run { }; a.b(x).c(); foo<Int>(1) } }",
			[]string{"g=3", "run=1", "b=1", "c=0", "foo=1"}},
		{"a.cpp", "void f() { g(1, 2); a->b(x).c(); new T(1, 2); new U{1}; ns::h<int>(3); }",
			[]string{"g=2", "b=1", "c=0", "T=2", "U=1", "h=1"}},
		{"A.cs", "class A { void F() { G(1, 2); a.B(x).C(); new A(1); new A { X = 1 }; H<int>(named: 3); } }",
			[]string{"G=2", "B=1", "C=0", "A=1", "A=?", "H=1"}},
		{"a.scala", "object A { def f(): Unit = { g(1, 2); a.b(x).c(); new A(1); h[Int](3); k { 1 } } }",
			[]string{"g=2", "b=1", "c=0", "A=1", "h=1", "k=1"}},
		{"a.swift", "func f() { g(1, b: 2); a.b(x).c(); A(1); run { }; h(1) { }; m(1)(2) }",
			[]string{"g=2", "b=1", "c=0", "A=1", "run=1", "h=2", "m=1"}},
		{"a.dart", "void f() { g(1, b: 2); a.b(x).c(); A(1); }",
			[]string{"g=2", "b=1", "c=0", "A=1"}},
		{"a.py", "def f():\n    g(1, 2)\n", []string{"g=?"}},
	} {
		pk := packs.Default().ForPath(c.path)
		f := &facts.File{Path: c.path}
		shared.Extract(f, pk, []byte(c.src))
		var got []string
		for _, r := range f.Refs {
			if r.Kind != facts.RefCall && r.Kind != facts.RefInstantiate {
				continue
			}
			n, ok := r.NArgs()
			if ok {
				got = append(got, fmt.Sprintf("%s=%d", r.Name, n))
			} else {
				got = append(got, r.Name+"=?")
			}
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: args %v, want %v", c.path, got, c.want)
		}
	}
}

// TestArgTypes records literal classes, identifiers and constructed types.
func TestArgTypes(t *testing.T) {
	for _, c := range []struct{ path, src, want string }{
		{"A.java", `class A { void f() { g("s", 1, 2.5, true, null, 'c', x, new B(), h(), a.b(1), y -> y, A::m, c[0]); } }`, "#s,#n,#n,#b,#0,#c,x,@B,()h,()b,#f,#f,"},
		{"a.cpp", `void f() { g("s", 1, true, nullptr, XML_OK, 'c'); }`, "#s,#n,#b,#0,XML_OK,#c"},
		{"A.cs", `class A { void F() { G(name: "s", 1, x, y => y); } }`, "name=#s,#n,x,#f"},
		{"a.kt", `fun f() { g(1, "s", flag = true, { x -> x }, Foo()) { } }`, "#n,#s,flag=#b,#f,()Foo,#t"},
		{"a.swift", `func f() { g(1, label: "s") { } }`, "#n,label=#s,#t"},
		{"a.dart", `void f() { g(1, a: "s"); }`, "#n,a=#s"},
		{"A.java", `class A { void f() { g(c[0], -1); } }`, ""},
	} {
		f := &facts.File{Path: c.path}
		shared.Extract(f, packs.Default().ForPath(c.path), []byte(c.src))
		got := "?"
		for _, r := range f.Refs {
			if r.Name == "g" || r.Name == "G" {
				got = r.ArgTypes
			}
		}
		if got != c.want {
			t.Errorf("%s: ArgTypes %q, want %q", c.path, got, c.want)
		}
	}
}
