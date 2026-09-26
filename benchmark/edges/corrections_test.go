package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReadErrata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "errata.tsv")
	data := "# comment\nrepo\ta/b.kt:12\tverify\tverify(t) passes 1 argument; the target takes none\nother\tx.kt:1\tf\tr\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadCorrections("repo", t.TempDir(), path, "", "clang")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.errata) != 1 || c.errata[siteKey{"a/b.kt", 12, "verify"}] == "" {
		t.Fatalf("errata = %v", c.errata)
	}
	if err := os.WriteFile(path, []byte("repo\ta.kt:1\tf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCorrections("repo", t.TempDir(), path, "", "clang"); err == nil {
		t.Fatal("an entry without a reason was accepted")
	}
}

func TestSplitCommand(t *testing.T) {
	got := splitCommand(`c++ -DNAME="a b" -I'x y' -c  src/a.cpp -o a\ b.o`)
	want := `[c++ -DNAME=a b -Ix y -c src/a.cpp -o a b.o]`
	if fmt.Sprint(got) != want {
		t.Fatalf("splitCommand = %q", got)
	}
}

// TestClangOracle parses a small C++ project with clang and checks that a
// call's callee groups the header declaration with the definition.
func TestClangOracle(t *testing.T) {
	if _, err := exec.LookPath("clang"); err != nil {
		t.Skip("clang is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"lib.h":    "struct Doc {\n    int Parse(const char* s, int n = -1);\n    void Parse();\n};\n",
		"lib.cpp":  "#include \"lib.h\"\nint Doc::Parse(const char* s, int n) { return n; }\nvoid Doc::Parse() {}\n",
		"main.cpp": "#include \"lib.h\"\nint main() {\n    Doc d;\n    return d.Parse(\"x\");\n}\n",
	}
	for name, text := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var cmds []compileCommand
	for _, f := range []string{"lib.cpp", "main.cpp"} {
		cmds = append(cmds, compileCommand{Directory: root, File: filepath.Join(root, f), Command: "c++ -std=c++11 -c " + f + " -o " + f + ".o"})
	}
	data, _ := json.Marshal(cmds)
	compdb := filepath.Join(root, "compile_commands.json")
	if err := os.WriteFile(compdb, data, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := loadCorrections("x", root, "", compdb, "clang")
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprint(c.oracle[siteKey{"main.cpp", 4, "Parse"}])
	if got != "[{lib.cpp 2} {lib.h 2}]" {
		t.Fatalf("oracle for d.Parse(\"x\") = %s (all: %v)", got, c.oracle)
	}
}
