package query

import (
	"fmt"
	"testing"
)

func TestParamTypes(t *testing.T) {
	for _, c := range []struct{ sig, name, lang, want string }{
		{"public static Doc parse(@Nullable String html, final int n, Parser p)", "parse", "java", "[String int Parser]"},
		{"bool XMLTest(const char* testString, XMLError expected, bool echo = true)", "XMLTest", "cpp", "[const char* XMLError bool]"},
		{"void f(int *p, char s[])", "f", "c", "[int* char[]]"},
		{"fun <T> mock(name: String, block: T.() -> Unit = {}): T", "mock", "kotlin", "[String T.() -> Unit]"},
		{"void f(void)", "f", "c", "[]"},
		{"public void Add(this IServices s, params string[] names)", "Add", "csharp", "[IServices string[]]"},
		{"void f(int a, int b...", "f", "cpp", "<nil>"},
		{"Task<TResponse> Send<TResponse>(IRequest<TResponse> request, CancellationToken ct = default)", "Send", "csharp", "[IRequest<TResponse> CancellationToken]"},
		{"void Resend(int a)", "Send", "csharp", "<nil>"},
		{"bool XMLTest (const char* s, bool echo=true )", "XMLTest", "cpp", "[const char* bool]"},
	} {
		got := paramTypes(c.sig, c.name, c.lang)
		s := fmt.Sprint(got)
		if got == nil {
			s = "<nil>"
		}
		if s != c.want {
			t.Errorf("paramTypes(%q) = %s, want %s", c.sig, s, c.want)
		}
	}
}

func TestTypeFamily(t *testing.T) {
	for in, want := range map[string]string{
		"const char*": "1 char*", "String": "1 String", "int": "2 int", "size_t": "2 size_t", "bool": "3 bool",
		"char": "4 char", "Object": "5 Object", "XMLError": "7 XMLError", "const std::string&": "1 string",
		"List<User>": "7 List", "": "0 ",
	} {
		f, b := typeFamily(in)
		if got := fmt.Sprintf("%d %s", f, b); got != want {
			t.Errorf("typeFamily(%q) = %s, want %s", in, got, want)
		}
	}
}
