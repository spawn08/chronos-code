package packs

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestLoadEmbedded(t *testing.T) {
	r, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(r.Packs()); n != 17 {
		t.Fatalf("packs = %d, want 17", n)
	}
	for path, want := range map[string]string{
		"src/app.ts": "typescript", "web/App.tsx": "tsx", "lib/x.MJS": "javascript",
		"a/b.py": "python", "Main.java": "java", "build.gradle.kts": "kotlin",
		"x.scala": "scala", "k.c": "c", "k.h": "cpp", "k.hpp": "cpp", "v.m": "objc",
		"P.cs": "csharp", "lib.rs": "rust", "V.swift": "swift", "r.rb": "ruby",
		"i.php": "php", "m.dart": "dart", "run.sh": "bash",
	} {
		p := r.ForPath(path)
		if p == nil || p.ID != want {
			t.Errorf("ForPath(%q) = %v, want %s", path, p, want)
		}
	}
	for _, path := range []string{"main.go", "README.md", "Makefile", "x."} {
		if p := r.ForPath(path); p != nil {
			t.Errorf("ForPath(%q) = %s, want nil", path, p.ID)
		}
	}
	if r.ForPath("web/App.tsx").Language != "typescript" {
		t.Error("tsx pack should record language typescript")
	}
}

func TestLoadRejectsBadPacks(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"duplicate extension": {
			"a/pack.yaml": {Data: []byte("language: a\ngrammar: a\nextensions: [.x]\n")},
			"b/pack.yaml": {Data: []byte("language: b\ngrammar: b\nextensions: [.x]\n")},
		},
		"upper-case extension": {"a/pack.yaml": {Data: []byte("language: a\ngrammar: a\nextensions: [.X]\n")}},
		"missing grammar":      {"a/pack.yaml": {Data: []byte("language: a\nextensions: [.x]\n")}},
		"no dot":               {"a/pack.yaml": {Data: []byte("language: a\ngrammar: a\nextensions: [x]\n")}},
	} {
		if _, err := load(fsys); err == nil {
			t.Errorf("%s: load succeeded", name)
		} else if !strings.Contains(err.Error(), "pack") && !strings.Contains(err.Error(), "extension") {
			t.Errorf("%s: unhelpful error %v", name, err)
		}
	}
}
