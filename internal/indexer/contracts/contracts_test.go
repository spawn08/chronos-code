package contracts

import (
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

func TestRouteKey(t *testing.T) {
	for _, c := range []struct{ method, path, want string }{
		{"get", "/users/:id", "GET /users/{}"},
		{"", "GET /users/{id}", "GET /users/{}"},
		{"POST", "https://api.example.com:8443/v1/users/?x=1#top", "POST /v1/users"},
		{"GET", "/v1/users/${id}/posts", "GET /v1/users/{}/posts"},
		{"GET", "${BASE}/v1/users", "GET /v1/users"},
		{"GET", "%s/users/%d", "GET /users/{}"},
		{"delete", "/orders/<int:order_id>", "DELETE /orders/{}"},
		{"GET", "/users/42", "GET /users/{}"},
		{"GET", "/files/{path...}", "GET /files/{}"},
		{"GET", "/users/user-{id}.json", "GET /users/user-{}.json"},
		{"", "/", "ANY /"},
		{"all", "/x", "ANY /x"},
		{"GET", "users", ""},
		{"GET", "", ""},
	} {
		if got := RouteKey(c.method, c.path); got != c.want {
			t.Errorf("RouteKey(%q, %q) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

func TestKeys(t *testing.T) {
	if got := RPCKey("acme.v1.UserService", "getUser"); got != "UserService/GetUser" {
		t.Errorf("RPCKey = %q", got)
	}
	if got := RPCKey("query", "user"); got != "Query/User" {
		t.Errorf("RPCKey(graphql) = %q", got)
	}
	if got := TableKey(`public."Users"`); got != "users" {
		t.Errorf("TableKey = %q", got)
	}
	if got := JoinPath("/api/", "/users"); got != "/api/users" {
		t.Errorf("JoinPath = %q", got)
	}
}

func TestKind(t *testing.T) {
	for rel, want := range map[string]string{
		"api/users.proto": LangProto, "x.thrift": LangThrift, "schema.graphqls": LangGraphQL, "q.gql": LangGraphQL,
		"db/001_init.sql": LangSQL, "openapi.yaml": LangOpenAPI, "api/swagger.json": LangOpenAPI,
		"users.openapi.yml": LangOpenAPI, "config.yaml": "", "main.go": "",
	} {
		if got := Kind(rel); got != want {
			t.Errorf("Kind(%q) = %q, want %q", rel, got, want)
		}
	}
}

// TestRulesValid checks every embedded recogniser compiles and names a
// known language.
func TestRulesValid(t *testing.T) {
	langs := map[string]bool{"go": true, "java": true, "kotlin": true, "scala": true, "python": true, "javascript": true,
		"typescript": true, "ruby": true, "php": true, "csharp": true, "rust": true, "cpp": true, "c": true}
	if len(Rules()) == 0 {
		t.Fatal("no recognisers")
	}
	for _, r := range Rules() {
		for _, l := range r.Langs {
			if !langs[l] {
				t.Errorf("rule %s: unknown language %q", r.ID, l)
			}
		}
	}
	if _, err := LoadRules([]byte("rules:\n  - id: x\n    langs: [go]\n    prefilter: [a]\n    pattern: '('\n    role: use\n")); err == nil {
		t.Error("bad pattern accepted")
	}
	if _, err := LoadRules([]byte("rules:\n  - id: x\n    langs: [go]\n    prefilter: [a]\n    pattern: 'a'\n    role: serve\n    kind: message\n    key: x\n")); err == nil {
		t.Error("message recogniser accepted")
	}
}

// TestRecognizeWithoutMatches leaves a file untouched when no prefilter
// literal occurs.
func TestRecognizeWithoutMatches(t *testing.T) {
	f := &facts.File{Lang: "go", Symbols: []facts.Symbol{{Name: "f", Kind: facts.KindFunc, Line: 1, EndLine: 1}}}
	Recognize(f, []byte("package x\nfunc f() {}\n"))
	if len(f.Symbols) != 1 || len(f.Refs) != 0 {
		t.Fatalf("facts changed: %+v", f)
	}
}
