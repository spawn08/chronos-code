//go:build treesitter

package graph

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestIndexNonGoFilePython(t *testing.T) {
	dir := t.TempDir()
	src := "def foo(x):\n    return x\n\nclass Bar:\n    def method(self):\n        pass\n"
	path := filepath.Join(dir, "mod.py")
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "mod.py")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 2 {
		t.Fatalf("expected at least 2 symbols (foo, Bar), got %d", symbols)
	}

	foo, err := store.FindSymbols(context.Background(), "foo", "")
	if err != nil || len(foo) == 0 {
		t.Fatalf("expected to find symbol foo, err=%v", err)
	}
	if foo[0].Kind != KindFunc {
		t.Errorf("foo kind = %s, want %s", foo[0].Kind, KindFunc)
	}

	bar, err := store.FindSymbols(context.Background(), "Bar", "")
	if err != nil || len(bar) == 0 {
		t.Fatalf("expected to find symbol Bar, err=%v", err)
	}
	if bar[0].Kind != KindStruct {
		t.Errorf("Bar kind = %s, want %s", bar[0].Kind, KindStruct)
	}
}

func TestSupportedTreeSitterExtensionsCount(t *testing.T) {
	exts := SupportedTreeSitterExtensions()
	// 5 original (ts, tsx, py, rs, java) + 18 new = 23
	if len(exts) < 23 {
		t.Fatalf("expected at least 23 extensions, got %d: %v", len(exts), exts)
	}
}

func TestTsLangForAllExtensions(t *testing.T) {
	for _, ext := range SupportedTreeSitterExtensions() {
		spec := tsLangFor(ext)
		if spec == nil {
			t.Errorf("tsLangFor(%q) returned nil", ext)
			continue
		}
		if spec.lang == nil {
			t.Errorf("tsLangFor(%q).lang is nil", ext)
		}
	}
}

func TestIndexNonGoFileC(t *testing.T) {
	dir := t.TempDir()
	src := "#include <stdio.h>\n\nstruct Point {\n    int x;\n    int y;\n};\n\nint add(int a, int b) {\n    return a + b;\n}\n"
	writeTestFile(t, dir, "sample.c", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.c")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 1 {
		t.Fatalf("expected at least 1 symbol (add), got %d", symbols)
	}
	assertSymbol(t, store, "add", KindFunc)
}

func TestIndexNonGoFileCpp(t *testing.T) {
	dir := t.TempDir()
	src := "#include <iostream>\n\nclass Calculator {\npublic:\n    int add(int a, int b) { return a + b; }\n};\n\nint multiply(int a, int b) {\n    return a * b;\n}\n"
	writeTestFile(t, dir, "sample.cpp", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.cpp")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 1 {
		t.Fatalf("expected at least 1 symbol, got %d", symbols)
	}
	assertSymbol(t, store, "multiply", KindFunc)
}

func TestIndexNonGoFileCSharp(t *testing.T) {
	dir := t.TempDir()
	src := "using System;\n\nnamespace Sample {\n    public class Greeter {\n        public string Hello(string name) {\n            return \"Hello, \" + name;\n        }\n    }\n\n    public interface IService {\n        void Execute();\n    }\n}\n"
	writeTestFile(t, dir, "sample.cs", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.cs")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 2 {
		t.Fatalf("expected at least 2 symbols (Greeter, IService), got %d", symbols)
	}
	assertSymbol(t, store, "Greeter", KindStruct)
	assertSymbol(t, store, "IService", KindInterface)
}

func TestIndexNonGoFileRuby(t *testing.T) {
	dir := t.TempDir()
	src := "module Utils\n  class Parser\n    def parse(input)\n      input.split(\",\")\n    end\n  end\nend\n"
	writeTestFile(t, dir, "sample.rb", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.rb")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 2 {
		t.Fatalf("expected at least 2 symbols (Utils, Parser), got %d", symbols)
	}
	assertSymbol(t, store, "Parser", KindStruct)
	assertSymbol(t, store, "Utils", KindType)
}

func TestIndexNonGoFilePHP(t *testing.T) {
	dir := t.TempDir()
	src := "<?php\n\nfunction helper(): string {\n    return \"ok\";\n}\n\nclass UserService {\n    public function findUser(int $id) {\n        return null;\n    }\n}\n"
	writeTestFile(t, dir, "sample.php", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.php")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 2 {
		t.Fatalf("expected at least 2 symbols (helper, UserService), got %d", symbols)
	}
	assertSymbol(t, store, "helper", KindFunc)
	assertSymbol(t, store, "UserService", KindStruct)
}

func TestIndexNonGoFileBash(t *testing.T) {
	dir := t.TempDir()
	src := "#!/bin/bash\n\ngreet() {\n    echo \"Hello, $1\"\n}\n\nbuild_project() {\n    make build\n}\n"
	writeTestFile(t, dir, "sample.sh", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.sh")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 2 {
		t.Fatalf("expected at least 2 symbols (greet, build_project), got %d", symbols)
	}
	assertSymbol(t, store, "greet", KindFunc)
	assertSymbol(t, store, "build_project", KindFunc)
}

func TestIndexNonGoFileKotlin(t *testing.T) {
	dir := t.TempDir()
	src := "interface Shape {\n    fun area(): Double\n}\n\nclass Circle(val radius: Double) : Shape {\n    override fun area(): Double = Math.PI * radius * radius\n}\n\nfun distance(x: Double, y: Double): Double {\n    return x + y\n}\n"
	writeTestFile(t, dir, "sample.kt", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.kt")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 2 {
		t.Fatalf("expected at least 2 symbols, got %d", symbols)
	}
	assertSymbol(t, store, "Circle", KindStruct)
	assertSymbol(t, store, "Shape", KindInterface)
}

func TestIndexNonGoFileSwift(t *testing.T) {
	dir := t.TempDir()
	src := "import Foundation\n\nprotocol Printable {\n    func description() -> String\n}\n\nclass Animal {\n    var name: String\n    init(name: String) { self.name = name }\n}\n\nfunc greet(name: String) -> String {\n    return \"Hello\"\n}\n"
	writeTestFile(t, dir, "sample.swift", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.swift")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols < 2 {
		t.Fatalf("expected at least 2 symbols, got %d", symbols)
	}
	assertSymbol(t, store, "Animal", KindStruct)
	assertSymbol(t, store, "Printable", KindInterface)
}

func TestIndexNonGoFileSQL(t *testing.T) {
	dir := t.TempDir()
	src := "CREATE TABLE users (id INT PRIMARY KEY, name TEXT);\n"
	writeTestFile(t, dir, "sample.sql", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.sql")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	// SQL has no declKinds, so 0 symbols expected — file still gets indexed.
	if symbols != 0 {
		t.Fatalf("expected 0 symbols for SQL (structural only), got %d", symbols)
	}
}

func TestIndexNonGoFileYAML(t *testing.T) {
	dir := t.TempDir()
	src := "name: test\nversion: 1.0\n"
	writeTestFile(t, dir, "sample.yaml", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.yaml")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols != 0 {
		t.Fatalf("expected 0 symbols for YAML (structural only), got %d", symbols)
	}
}

func TestIndexNonGoFileHTML(t *testing.T) {
	dir := t.TempDir()
	src := "<!DOCTYPE html>\n<html><body><h1>Test</h1></body></html>\n"
	writeTestFile(t, dir, "sample.html", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.html")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols != 0 {
		t.Fatalf("expected 0 symbols for HTML (structural only), got %d", symbols)
	}
}

func TestIndexNonGoFileCSS(t *testing.T) {
	dir := t.TempDir()
	src := "body { margin: 0; }\n.container { display: flex; }\n"
	writeTestFile(t, dir, "sample.css", src)

	store := openTestStore(t, dir)
	defer store.Close()

	symbols, _, err := IndexNonGoFile(context.Background(), store, dir, "sample.css")
	if err != nil {
		t.Fatalf("IndexNonGoFile: %v", err)
	}
	if symbols != 0 {
		t.Fatalf("expected 0 symbols for CSS (structural only), got %d", symbols)
	}
}

// writeTestFile writes content to dir/name and fatals on error.
func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// openTestStore opens a fresh graph.db in dir and fatals on error.
func openTestStore(t *testing.T, dir string) *Store {
	t.Helper()
	store, err := OpenStore(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// assertSymbol checks that a symbol with the given name and kind exists in store.
func assertSymbol(t *testing.T, store *Store, name string, kind SymbolKind) {
	t.Helper()
	syms, err := store.FindSymbols(context.Background(), name, "")
	if err != nil || len(syms) == 0 {
		t.Fatalf("expected to find symbol %q, err=%v, count=%d", name, err, len(syms))
	}
	if syms[0].Kind != kind {
		t.Errorf("symbol %q kind = %s, want %s", name, syms[0].Kind, kind)
	}
}

func TestIndexNonGoFileUnsupportedExtension(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	symbols, edges, err := IndexNonGoFile(context.Background(), store, dir, "readme.md")
	if err != nil || symbols != 0 || edges != 0 {
		t.Fatalf("expected (0, 0, nil) for unsupported extension, got (%d, %d, %v)", symbols, edges, err)
	}
}
