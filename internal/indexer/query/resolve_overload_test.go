package query_test

import (
	"fmt"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/query"
)

// Overload selection by argument count (M7), one fixture per language with
// a call rule. A call keeps the overloads that accept its argument count;
// when none does (a miscount, an external overload), all stay.

func TestResolveOverloadsJava(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/org/j/Jsoup.java": `package org.j;

public class Jsoup {
    public static Doc parse(String html) { return null; }
    public static Doc parse(String html, String base) { return null; }
    public static Doc parse(String html, String base, Parser p) { return null; }
    public static Doc clean(String html, String... tags) { return null; }
    public static Doc clean(String html, int level) { return null; }
}
`,
		"src/main/java/org/j/Doc.java": `package org.j;

public class Doc {
    public String text() { return ""; }
    public Doc text(String s) { return this; }
    public Doc append(String a) { return this; }
    public Doc append(String a, String b) { return this; }
}
`,
		"src/main/java/org/app/Use.java": `package org.app;

import org.j.Jsoup;
import org.j.Doc;

class Use {
    void run(Doc d) {
        Jsoup.parse("a");
        Jsoup.parse("a", "b");
        Jsoup.clean("a");
        Jsoup.clean("a", "b", "c");
        d.text();
        d.text("x");
        d.append("a", "b");
        d.append("a", "b", "c");
    }
}
`,
	}).view(t)
	expect(t, resolved(t, v, "Use.run"), map[string]string{
		"parse@8":   "import_resolved Jsoup.parse",
		"parse@9":   "import_resolved Jsoup.parse",
		"clean@10":  "import_resolved Jsoup.clean",
		"clean@11":  "import_resolved Jsoup.clean",
		"text@12":   "type_hinted Doc.text",
		"text@13":   "type_hinted Doc.text",
		"append@14": "type_hinted Doc.append",
		// No overload takes three arguments: both stay.
		"append@15": "ambiguous Doc.append,Doc.append",
	})
	// The right overload is the target, not just one of them.
	lines := map[int]int{8: 4, 9: 5, 10: 7, 11: 7, 12: 4, 13: 5, 14: 7}
	for _, c := range outgoingSymbols(t, v, "Use.run") {
		if want, ok := lines[c.line]; ok && (len(c.targets) != 1 || c.targets[0] != want) {
			t.Errorf("%s@%d targets lines %v, want [%d]", c.name, c.line, c.targets, want)
		}
	}
}

func TestResolveOverloadsKotlin(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/kotlin/org/k/Api.kt": `package org.k

class Api {
    fun call(a: Int): Int = a
    fun call(a: Int, b: Int, c: Int = 0): Int = a
    fun run(name: String, block: () -> Unit) {}
    fun run(block: () -> Unit) {}
    fun log(vararg parts: String) {}
    fun log(level: Int, msg: String) {}
}
`,
		"src/main/kotlin/org/k/Use.kt": `package org.k

class Use {
    fun go(api: Api) {
        api.call(1)
        api.call(1, 2)
        api.run("x") { }
        api.run { }
        api.log()
    }
}
`,
	}).view(t)
	got := resolved(t, v, "Use.go")
	expect(t, got, map[string]string{
		"call@5": "type_hinted Api.call",
		"call@6": "type_hinted Api.call",
		"run@7":  "type_hinted Api.run",
		"run@8":  "type_hinted Api.run",
		"log@9":  "type_hinted Api.log",
	})
	lines := map[int]int{5: 4, 6: 5, 7: 6, 8: 7, 9: 8}
	for _, c := range outgoingSymbols(t, v, "Use.go") {
		if want, ok := lines[c.line]; ok && (len(c.targets) != 1 || c.targets[0] != want) {
			t.Errorf("%s@%d targets lines %v, want [%d]", c.name, c.line, c.targets, want)
		}
	}
}

func TestResolveOverloadsCSharp(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/Lib/Mediator.cs": `namespace App.Lib;

public interface IServices { }

public class Mediator
{
    public int Send(object request) { return 0; }
    public int Send(object request, int timeout, bool wait = false) { return 0; }
}

public static class Extensions
{
    public static IServices Add(this IServices s, string name) { return s; }
    public static IServices Add(this IServices s, params string[] names) { return s; }
    public static IServices Add(this IServices s) { return s; }
}
`,
		"src/App/Use.cs": `namespace App.Main;

using App.Lib;

public class Use
{
    public void Run(Mediator m, IServices s)
    {
        m.Send(1);
        m.Send(1, 2);
        s.Add();
    }
}
`,
	}).view(t)
	got := resolved(t, v, "Use.Run")
	expect(t, got, map[string]string{
		"Send@9":  "type_hinted Mediator.Send",
		"Send@10": "type_hinted Mediator.Send",
	})
	// s.Add() passes no argument besides the receiver: Add(this s) and the
	// params overload fit, Add(this s, name) does not.
	if got["Add@11"] != "ambiguous Extensions.Add,Extensions.Add" {
		t.Errorf("Add@11 = %q", got["Add@11"])
	}
	for _, c := range outgoingSymbols(t, v, "Use.Run") {
		switch {
		case c.line == 9 && (len(c.targets) != 1 || c.targets[0] != 7),
			c.line == 10 && (len(c.targets) != 1 || c.targets[0] != 8):
			t.Errorf("%s@%d targets lines %v", c.name, c.line, c.targets)
		case c.line == 11 && (len(c.targets) != 2 || c.targets[0] != 14 || c.targets[1] != 15):
			t.Errorf("Add@11 targets lines %v, want [14 15]", c.targets)
		}
	}
}

func TestResolveOverloadsCpp(t *testing.T) {
	v := newFixture(t, map[string]string{
		"xml.h": `namespace xml {
class Doc {
public:
    int Parse(const char* s);
    int Parse(const char* s, int len, bool strict = false);
    void Print(...);
};
bool Test(const char* name, int a, int b);
bool Test(const char* name, const char* a, const char* b, bool echo = true);
bool Test(const char* name, bool expected);
}
`,
		"xml.cpp": `#include "xml.h"
namespace xml {
int Doc::Parse(const char* s) { return 0; }
int Doc::Parse(const char* s, int len, bool strict) { return 0; }
void Doc::Print(...) {}
bool Test(const char* name, int a, int b) { return true; }
bool Test(const char* name, const char* a, const char* b, bool echo) { return true; }
bool Test(const char* name, bool expected) { return true; }
}
`,
		"main.cpp": `#include "xml.h"
using namespace xml;

int main() {
    Doc doc;
    doc.Parse("a");
    doc.Parse("a", 1);
    Test("n", true);
    Test("n", "a", "b", false);
    Test("n", "a", "b");
    return 0;
}
`,
	}).view(t)
	expect(t, resolved(t, v, "main"), map[string]string{
		"Parse@6": "type_hinted Doc.Parse",
		"Parse@7": "type_hinted Doc.Parse",
		"Test@8":  "import_resolved Test",
		"Test@9":  "import_resolved Test",
		"Test@10": "import_resolved Test",
	})
	// Defaults are on the header declarations; the calls still reach the
	// out-of-line definitions. Three arguments fit two overloads; the
	// string literals rule out Test(const char*, int, int).
	want := map[int]string{6: "[3]", 7: "[4]", 8: "[8]", 9: "[7]", 10: "[7]"}
	for _, c := range outgoingSymbols(t, v, "main") {
		w, ok := want[c.line]
		if !ok {
			continue
		}
		if got := fmt.Sprint(c.targets); got != w || c.files[0] != "xml.cpp" {
			t.Errorf("%s@%d targets %v %v, want xml.cpp %s", c.name, c.line, c.files, got, w)
		}
	}
}

// A language without overloading ignores argument counts: Python's
// defaults and *args make a count unreliable anyway.
func TestResolveArityIgnoredWithoutOverloading(t *testing.T) {
	v := newFixture(t, map[string]string{
		"a/x.py": "class A:\n    def save(self, a):\n        pass\n",
		"b/y.py": "class B:\n    def save(self):\n        pass\n",
		"c/use.py": `def run(obj):
    obj.save(1)
`,
	}).view(t)
	expect(t, resolved(t, v, "run"), map[string]string{
		"save@2": "ambiguous A.save,B.save",
	})
}

type outSym struct {
	name    string
	line    int
	files   []string
	targets []int // target declaration lines, first target first
}

func outgoingSymbols(t *testing.T, v *query.View, from string) []outSym {
	t.Helper()
	syms := v.ByQualified([]string{from})[from]
	if len(syms) != 1 {
		t.Fatalf("%s: %d declarations", from, len(syms))
	}
	var out []outSym
	for _, c := range v.Outgoing(syms[0]) {
		targets, _ := v.Resolve(c)
		o := outSym{name: c.Callee, line: c.Line}
		for _, s := range targets {
			o.files = append(o.files, s.File)
			o.targets = append(o.targets, s.Line)
		}
		out = append(out, o)
	}
	return out
}

// TestResolveConstructorOverloads: new T(args) reaches only the
// constructors that accept its arguments, in find_callers edges.
func TestResolveConstructorOverloads(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/org/c/Point.java": `package org.c;

public class Point {
    public Point(int x) { }
    public Point(int x, int y) { }
}
`,
		"src/main/java/org/c/Use.java": `package org.c;

class Use {
    void one() { new Point(1); }
    void two() { new Point(1, 2); }
    void none() { new Point(1, 2, 3); }
}
`,
	}).view(t)
	ctors := v.Symbols("Point", "constructor")
	if len(ctors) != 2 {
		t.Fatalf("constructors = %d", len(ctors))
	}
	callers := map[int][]string{}
	for _, e := range v.IncomingEdges(ctors) {
		callers[e.Callee.Line] = append(callers[e.Callee.Line], e.Caller.Name)
	}
	// No constructor takes three arguments: both keep that caller.
	if got := fmt.Sprint(callers); got != "map[4:[one none] 5:[two none]]" {
		t.Fatalf("constructor callers = %s", got)
	}
	for _, c := range ctors {
		var names []string
		for _, in := range v.IncomingCalls(c) {
			names = append(names, in.Caller.Name)
		}
		if want := fmt.Sprint(callers[c.Line]); fmt.Sprint(names) != want {
			t.Errorf("IncomingCalls(%d) = %v, want %s", c.Line, names, want)
		}
	}
}

// TestResolveChainOverloads: a receiver chain picks the overload whose
// arity fits before taking its result type.
func TestResolveChainOverloads(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/org/b/Builder.java": `package org.b;

public class Builder {
    public Small with(int a) { return null; }
    public Large with(int a, int b) { return null; }
}
`,
		"src/main/java/org/b/Small.java": "package org.b;\n\npublic class Small {\n    public void done() { }\n}\n",
		"src/main/java/org/b/Large.java": "package org.b;\n\npublic class Large {\n    public void done() { }\n}\n",
		"src/main/java/org/b/Use.java": `package org.b;

class Use {
    void run(Builder b) {
        b.with(1).done();
        b.with(1, 2).done();
    }
}
`,
	}).view(t)
	expect(t, resolved(t, v, "Use.run"), map[string]string{
		"done@5": "type_hinted Small.done",
		"done@6": "type_hinted Large.done",
	})
}

// TestResolveOverloadsByArgumentTypes: overloads with the same arity are
// told apart by literal classes, binding hints and enum constants.
func TestResolveOverloadsByArgumentTypes(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/org/t/Jsoup.java": `package org.t;

import java.io.File;

public class Jsoup {
    public static Doc parse(File file, String charset, String base) { return null; }
    public static Doc parse(String html, String base, Parser parser) { return null; }
    public static Doc parse(String html, int timeout, boolean strict) { return null; }
}
`,
		"src/main/java/org/t/Use.java": `package org.t;

import java.io.File;

class Use {
    void run(File in, Parser p) {
        Jsoup.parse(in, null, "http://x");
        Jsoup.parse("<p>", "http://x", p);
        Jsoup.parse("<p>", 10, true);
    }
}
`,
		"xmltest.cpp": `enum XMLError { XML_SUCCESS, XML_ERROR };

bool XMLTest(const char* testString, const char* expected, const char* found, bool echo = true) { return true; }
bool XMLTest(const char* testString, XMLError expected, XMLError found) { return true; }
bool XMLTest(const char* testString, bool expected, bool found) { return true; }

int main() {
    XMLTest("a", "b", "c");
    XMLTest("a", XML_SUCCESS, XML_ERROR);
    XMLTest("a", true, false);
    return 0;
}
`,
	}).view(t)
	lines := map[int]int{7: 6, 8: 7, 9: 8}
	for _, c := range outgoingSymbols(t, v, "Use.run") {
		if want, ok := lines[c.line]; ok && fmt.Sprint(c.targets) != fmt.Sprintf("[%d]", want) {
			t.Errorf("parse@%d targets %v, want [%d]", c.line, c.targets, want)
		}
	}
	lines = map[int]int{8: 3, 9: 4, 10: 5}
	for _, c := range outgoingSymbols(t, v, "main") {
		if want, ok := lines[c.line]; ok && fmt.Sprint(c.targets) != fmt.Sprintf("[%d]", want) {
			t.Errorf("XMLTest@%d targets %v, want [%d]", c.line, c.targets, want)
		}
	}
}

// TestResolveKotlinLambdasAndNamedArguments: a trailing lambda binds the
// last parameter, named arguments bind by name, a function-typed variable
// is a function, a constructor call has its class's type, and an
// unqualified call prefers functions over extension functions.
func TestResolveKotlinLambdasAndNamedArguments(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/kotlin/org/m/Mocking.kt": `package org.m

fun <T : Any> mock(name: String? = null, lenient: Boolean = false): T = TODO()
fun <T : Any> mock(name: String? = null, stubbing: T.() -> Unit): T = TODO()
fun <T : Any> mock(a: Answer): T = TODO()
fun <T : Any> argThat(predicate: T.() -> Boolean): T = TODO()
fun <T : Any> argThat(matcher: Matcher<T>): T = TODO()
fun <T> spy(stubbing: T.() -> Unit): T = TODO()
fun <T> spy(value: T): T = TODO()
fun inOrder(vararg mocks: Any): Unit = TODO()
fun <T> T.inOrder(block: T.() -> Unit): Unit = TODO()
class Answer
interface Matcher<T>
class Some
`,
		"src/main/kotlin/org/m/Use.kt": `package org.m

fun run(p: Some.() -> Boolean) {
    mock<Some> { }
    val a: Some = mock(name = "x")
    val b: Some = mock(lenient = true)
    argThat(p)
    spy(Some())
    inOrder(Some())
}
`,
	}).view(t)
	want := map[string]int{"mock@4": 4, "mock@5": 3, "mock@6": 3, "argThat@7": 6, "spy@8": 9, "inOrder@9": 10}
	seen := 0
	for _, c := range outgoingSymbols(t, v, "run") {
		if w, ok := want[fmt.Sprintf("%s@%d", c.name, c.line)]; ok {
			seen++
			if fmt.Sprint(c.targets) != fmt.Sprintf("[%d]", w) {
				t.Errorf("%s@%d targets %v, want [%d]", c.name, c.line, c.targets, w)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("checked %d of %d calls", seen, len(want))
	}
}

// TestResolveKotlinLambdaReceivers: inside a lambda passed as R.() -> Unit,
// an unqualified call reaches R's members before top-level functions.
func TestResolveKotlinLambdaReceivers(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/kotlin/org/r/Order.kt": `package org.r

interface KInOrder {
    fun verify(mock: Any)
}

class OnType<T>(val t: T) {
    fun check() {}
}

fun verify(mock: Any) {}
fun check() {}
fun inOrder(vararg mocks: Any, evaluation: KInOrder.() -> Unit) {}
fun <T> T.onType(block: OnType<T>.() -> Unit) {}
`,
		"src/main/kotlin/org/r/Use.kt": `package org.r

fun run(a: Any) {
    inOrder(a) {
        verify(a)
    }
    a.onType {
        check()
    }
    verify(a)
}
`,
	}).view(t)
	expect(t, resolved(t, v, "run"), map[string]string{
		"verify@5":  "type_hinted KInOrder.verify",
		"check@8":   "type_hinted OnType.check",
		"verify@10": "import_resolved verify",
	})
}
