package query_test

import (
	"testing"

	"github.com/spawn08/chronos-code/indexer/query"
)

// outCall is a call made inside a declaration with its targets' files in
// resolution order (first target first).
type outCall struct {
	name  string
	line  int
	files []string
}

func outgoing(t *testing.T, v *query.View, from string) []outCall {
	t.Helper()
	syms := v.ByQualified([]string{from})[from]
	if len(syms) != 1 {
		t.Fatalf("%s: %d declarations", from, len(syms))
	}
	var out []outCall
	for _, c := range v.Outgoing(syms[0]) {
		targets, _ := v.Resolve(c)
		oc := outCall{name: c.Callee, line: c.Line}
		for _, s := range targets {
			oc.files = append(oc.files, s.File)
		}
		out = append(out, oc)
	}
	return out
}

// Resolver cases found by the SCIP edge baseline (benchmark/edges), one
// fixture per language.

// TestResolveRustCrateAndBuilders: use crate::X resolves from the crate
// root (the directory of lib.rs), and builder chains T::new(a).m(b).n()
// follow each call's declared result, Self included. A call prefers the
// method over a field of the same name.
func TestResolveRustCrateAndBuilders(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/lib.rs": `pub struct WalkDir { depth: usize }

impl WalkDir {
    pub fn new(root: &str) -> Self { WalkDir { depth: 0 } }
    pub fn follow_links(mut self, yes: bool) -> Self { self }
    pub fn max_depth(mut self, n: usize) -> WalkDir { self }
    pub fn depth(&self) -> usize { self.depth }
}
`,
		"src/tests/recursive.rs": `use crate::WalkDir;

fn builder() {
    let wd = WalkDir::new("a").follow_links(true).max_depth(2);
    wd.depth();
}
`,
	}).view(t)
	expect(t, resolved(t, v, "builder"), map[string]string{
		"new@4":          "import_resolved WalkDir.new",
		"follow_links@4": "type_hinted WalkDir.follow_links",
		"max_depth@4":    "type_hinted WalkDir.max_depth",
		"depth@5":        "type_hinted WalkDir.depth",
	})
}

// TestResolveJavaImportsAndOverloads: a single-type import shadows a
// same-package class of that name, a static import brings a method into
// scope, a cast gives the receiver's type, and a supertype's overload with
// other parameters stays a candidate (and is the target when the argument
// count selects it).
func TestResolveJavaImportsAndOverloads(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/org/x/nodes/Comment.java": "package org.x.nodes;\n\npublic class Comment {\n}\n",
		"src/main/java/org/x/nodes/Node.java": `package org.x.nodes;

public class Node {
    public String text() { return ""; }
}
`,
		"src/main/java/org/x/nodes/Doc.java": `package org.x.nodes;

public class Doc extends Node {
    public Doc text(String s) { return this; }
}
`,
		"src/main/java/org/x/util/Strings.java": "package org.x.util;\n\npublic class Strings {\n    public static boolean inSorted(String s) { return true; }\n}\n",
		"src/main/java/org/x/parser/Token.java": "package org.x.parser;\n\nclass Token {\n    static class Comment {\n    }\n}\n",
		"src/main/java/org/x/parser/Builder.java": `package org.x.parser;

import org.x.nodes.Comment;
import org.x.nodes.Doc;
import org.x.nodes.Node;
import static org.x.util.Strings.inSorted;

class Builder {
    void run(Object o, Doc d) {
        new Comment();
        inSorted("a");
        ((Node) o).text();
        d.text();
    }
}
`,
	}).view(t)
	expect(t, resolved(t, v, "Builder.run"), map[string]string{
		"Comment@10":  "import_resolved Comment",
		"inSorted@11": "import_resolved Strings.inSorted",
		"text@12":     "type_hinted Node.text",
		"text@13":     "type_hinted Node.text",
	})
	if got := v.Symbols("Comment", "class"); len(got) != 2 {
		t.Fatalf("Comment classes = %d, want 2", len(got))
	}
}

// TestResolveCSharpNamespaceAndExtensions: the enclosing namespace's class
// wins over one reached by a using directive, and an extension method
// resolves on the type of its this parameter.
func TestResolveCSharpNamespaceAndExtensions(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/Pipeline/Ping.cs":  "namespace App.Pipeline;\n\npublic class Ping : IRequest<Pong> { }\n\npublic class Zing : IRequest<Zong> { }\n",
		"src/Tests/Handlers.cs": "namespace App.Tests;\n\npublic class Ping : IRequest<Pong> { }\n\npublic class Zing : IRequest<Zong> { }\n",
		"src/Ext/Registration.cs": `namespace App.Ext;

public interface IServices { }

public static class Registration
{
    public static IServices AddApp(this IServices services) { return services; }
}
`,
		"src/Tests/Tests.cs": `namespace App.Tests;

using App.Pipeline;
using App.Ext;

public class Tests
{
    public void Run(IServices services)
    {
        var p = new Ping();
        services.AddApp();
    }
}
`,
	}).view(t)
	got := resolved(t, v, "Tests.Run")
	expect(t, got, map[string]string{
		"AddApp@11": "type_hinted Registration.AddApp",
	})
	if got["Ping@10"] != "import_resolved Ping" {
		t.Fatalf("Ping@10 = %q", got["Ping@10"])
	}
	for _, c := range outgoing(t, v, "Tests.Run") {
		if c.name == "Ping" && (len(c.files) != 1 || c.files[0] != "src/Tests/Handlers.cs") {
			t.Fatalf("new Ping() resolved to %v, want the enclosing namespace's class", c.files)
		}
	}
}

// TestResolveCppDeclarationsAndPreprocessor: a member declared in a header
// and defined out of line resolves to the definition, and a function whose
// string literal is split by #if keeps its body (and so its callers).
func TestResolveCppDeclarationsAndPreprocessor(t *testing.T) {
	v := newFixture(t, map[string]string{
		"lib.h": `#ifndef LIB_H
#define LIB_H
#   // a null directive
namespace lib {
class LIB_API Doc {
public:
    void Print() const;
};
}
#endif
`,
		"lib.cpp": "#include \"lib.h\"\nnamespace lib {\nvoid Doc::Print() const {}\n}\n",
		"main.cpp": `#include "lib.h"
using namespace lib;

int main() {
    printf("a"
#if defined(_MSC_VER)
           "b"
#endif
    );
    Doc doc;
    doc.Print();
    return 0;
}
`,
	}).view(t)
	expect(t, resolved(t, v, "main"), map[string]string{
		"Print@11": "type_hinted Doc.Print",
	})
	for _, c := range outgoing(t, v, "main") {
		if c.name == "Print" && (len(c.files) != 1 || c.files[0] != "lib.cpp") {
			t.Fatalf("doc.Print() resolved to %v, want the definition in lib.cpp", c.files)
		}
	}
}

// TestResolveKotlinReceivers: a generic factory's result (mock<T>()) is
// unknown rather than external, so its methods are matched by name, and a
// call inside a lambda with a receiver finds methods by name; a same-file
// interface wins over one reached by a wildcard import.
func TestResolveKotlinReceivers(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/kotlin/org/m/Mocking.kt": "package org.m\n\nfun <T : Any> mock(block: T.() -> Unit = {}): T = TODO()\n",
		"src/test/kotlin/org/m/Other.kt":   "package org.m\n\ninterface Api {\n    fun call(): Int\n}\n",
		"src/test/kotlin/test/ApiTest.kt": `package test

import org.m.*

class ApiTest {
    fun run() {
        val m = mock<Api> {
            call()
        }
        m.call()
    }
}

interface Api {
    fun call(): Int
}
`,
	}).view(t)
	got := resolved(t, v, "ApiTest.run")
	for _, k := range []string{"call@8", "call@10"} {
		if got[k] == "" {
			t.Errorf("%s unresolved", k)
		}
	}
	for _, c := range outgoing(t, v, "ApiTest.run") {
		if c.name == "call" && c.line == 10 && (len(c.files) == 0 || c.files[0] != "src/test/kotlin/test/ApiTest.kt") {
			t.Fatalf("m.call() first target %v, want the same-file interface", c.files)
		}
	}
}
