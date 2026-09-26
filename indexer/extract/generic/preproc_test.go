package generic

import "testing"

func TestNormalizePreproc(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{
			name: "null directives lose their hash",
			in:   "#if A\n#   // note\n#\n#   define X 1\n#endif\n",
			want: "#if A\n    // note\n \n#   define X 1\n#endif\n",
		},
		{
			name: "group inside an expression is blanked",
			in:   "  printf(\"a\"\r\n#if defined(W)\r\n    \"b\"\r\n#else\r\n    \"c\"\r\n#endif\r\n  );\r\n",
			want: "  printf(\"a\"\r\n              \r\n    \"b\"\r\n     \r\n    \"c\"\r\n      \r\n  );\r\n",
		},
		{
			name: "the whole group follows its #if",
			in:   "foo(a,\n#ifdef X\n  b);\n#else\n  c);\n#endif\n",
			want: "foo(a,\n        \n  b);\n     \n  c);\n      \n",
		},
		{
			name: "groups at statement or declaration level stay",
			in:   "int x;\n#ifdef X\nint y;\n#endif\nvoid f() {\n#if A\n  g();\n#endif\n}\nclass C {\npublic:\n#if A\n  int z;\n#endif\n};\n",
		},
		{
			name: "export macros",
			in:   "class TINYXML2_LIB StrPair\r\n{\nclass FOO_API Bar : public Base {\nstruct X_EXPORT Y final {\nstruct POINT p;\nclass A::B C {\n",
			want: "class              StrPair\r\n{\nclass         Bar : public Base {\nstruct          Y final {\nstruct POINT p;\nclass A::B C {\n",
		},
		{
			name: "comments, macros and nested groups",
			in:   "#define M(a) \\\n  f(a,\n#ifdef X\nint y; /* a\n  comment, */\n#if B\n#endif\n#endif\nint z = 1 + // x;\n#if C\n 2\n#endif\n;\n",
			want: "#define M(a) \\\n  f(a,\n#ifdef X\nint y; /* a\n  comment, */\n#if B\n#endif\n#endif\nint z = 1 + // x;\n     \n 2\n      \n;\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			want := tc.want
			if want == "" {
				want = tc.in
			}
			got := string(normalizePreproc([]byte(tc.in)))
			if got != want {
				t.Errorf("got  %q\nwant %q", got, want)
			}
			if len(got) != len(tc.in) {
				t.Errorf("length changed: %d -> %d", len(tc.in), len(got))
			}
		})
	}
}

func TestStripArgs(t *testing.T) {
	for in, want := range map[string]string{
		"WalkDir::new(dir.path()).max_open(1).into_iter": "WalkDir::new().max_open().into_iter",
		"a.b(x, f(y)).c":                 "a.b().c",
		"make<Foo>":                      "make",
		"items.iter().collect::<Vec<_>>": "items.iter().collect",
		"self.repo.get":                  "self.repo.get",
		"broken(":                        "",
	} {
		if got := stripArgs(in); got != want {
			t.Errorf("stripArgs(%q) = %q, want %q", in, got, want)
		}
	}
}
