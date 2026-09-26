package graph

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// federatedScope indexes a client repository (the primary workspace)
// importing a users module from a sibling repository, which also serves
// a route the client calls.
func federatedScope(t *testing.T) (*IndexScope, string) {
	t.Helper()
	parent := federatedRepos(t)
	return openFederated(t, parent), filepath.Join(parent, "client")
}

// openFederated opens a scope (with its own index directories) over the
// repositories federatedRepos wrote under parent.
func openFederated(t *testing.T, parent string) *IndexScope {
	t.Helper()
	s, err := NewIndexScope(context.Background(), IndexScopeOptions{
		Root: filepath.Join(parent, "client"), DataDir: t.TempDir(), IndexOnStart: true,
		Federation: []FederatedRoot{{Root: filepath.Join(parent, "users")}, {Name: "missing", Root: filepath.Join(parent, "nope")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func federatedRepos(t *testing.T) string {
	t.Helper()
	parent := canonicalTempDir(t)
	client, users := filepath.Join(parent, "client"), filepath.Join(parent, "users")
	writeTree(t, client, map[string]string{
		"go.mod": "module example.com/client\n",
		"app/app.go": `package app

import (
	"net/http"

	"example.com/users/lookup"
)

// Show prints a user.
func Show(id string) string { return lookup.Find(id) }

// Fetch calls the users service.
func Fetch() { http.Get("/v1/users/42") }
`,
	})
	writeTree(t, users, map[string]string{
		"go.mod": "module example.com/users\n",
		"lookup/lookup.go": `package lookup

// Find returns a user by id.
func Find(id string) string { return id }
`,
		"server/server.go": `package server

import "net/http"

func getUser(w http.ResponseWriter, r *http.Request) {}

func Routes(mux *http.ServeMux) { mux.HandleFunc("GET /v1/users/{id}", getUser) }
`,
	})
	return parent
}

func TestFederatedToolsCrossRepositories(t *testing.T) {
	s, _ := federatedScope(t)
	ctx := context.Background()
	defs := append(s.Tools(), s.ImpactTools()...)

	got := call(t, ctx, toolFrom(t, defs, "graph_query"), map[string]any{"name": "Find"})
	syms, _ := got["symbols"].([]map[string]any)
	if len(syms) != 1 || syms[0]["repo"] != "users" || syms[0]["file"] != "../users/lookup/lookup.go" {
		t.Fatalf("graph_query Find = %+v", got)
	}
	idx, _ := got["index"].(map[string]any)
	repos, _ := idx["repos"].([]map[string]any)
	if len(repos) != 2 || repos[0]["name"] != "missing" || repos[0]["error"] == nil || repos[1]["name"] != "users" || repos[1]["root"] != "../users" || repos[1]["files"].(int) == 0 {
		t.Fatalf("index repos = %+v", repos)
	}

	// Callers across the import, and through the route contract to the
	// handler.
	callers := call(t, ctx, toolFrom(t, defs, "find_callers"), map[string]any{"name": "Find"})
	data, _ := json.Marshal(callers["callers_by_depth"])
	if !strings.Contains(string(data), `"users:Find":{"import_resolved":["Show (app/app.go:10, this workspace via import)"]}`) {
		t.Errorf("find_callers Find = %s", data)
	}
	route := call(t, ctx, toolFrom(t, defs, "find_callers"), map[string]any{"name": "getUser", "depth": 2})
	data, _ = json.Marshal(route["callers_by_depth"])
	if !strings.Contains(string(data), "GET /v1/users/{}") || !strings.Contains(string(data), "Fetch (app/app.go:13, this workspace via contract)") {
		t.Errorf("find_callers getUser depth 2 = %s", data)
	}

	// Primary symbols are unchanged by federation: no repo field.
	show := call(t, ctx, toolFrom(t, defs, "resolve_symbol"), map[string]any{"name": "Show"})
	if sym, _ := show["symbol"].(map[string]any); sym == nil || sym["file"] != "app/app.go" || sym["repo"] != nil {
		t.Errorf("resolve_symbol Show = %+v", show)
	}
	search := call(t, ctx, toolFrom(t, defs, "codebase_search"), map[string]any{"query": "user by id"})
	data, _ = json.Marshal(search["symbols"])
	if !strings.Contains(string(data), `"repo":"users"`) {
		t.Errorf("codebase_search = %s", data)
	}
	// L2 counts the cross-repository caller.
	l2 := call(t, ctx, toolFrom(t, defs, "multi_resolution_view"), map[string]any{"target": "Find", "level": "L2"})
	if out, _ := l2["symbols"].([]map[string]any); len(out) != 1 || out[0]["caller_count"] != 1 {
		t.Errorf("L2 Find = %+v", l2)
	}
}

// Federating an unrelated repository leaves the primary's answers
// unchanged, apart from the index report.
func TestFederationKeepsPrimaryAnswers(t *testing.T) {
	parent := canonicalTempDir(t)
	root, other := filepath.Join(parent, "pay"), filepath.Join(parent, "zoo")
	writeTree(t, root, payFiles)
	writeTree(t, other, map[string]string{"go.mod": "module zoo\n", "zoo.go": "package zoo\n\n// Zebra stripes.\nfunc Zebra() {}\n"})
	plain := newTestScope(t, root, false)
	fed, err := NewIndexScope(context.Background(), IndexScopeOptions{Root: root, DataDir: t.TempDir(), IndexOnStart: true, Federation: []FederatedRoot{{Root: other}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fed.Close() })
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"find_callers", map[string]any{"name": "record", "depth": 2}},
		{"graph_query", map[string]any{"name": "Pay"}},
		{"resolve_symbol", map[string]any{"name": "Handle"}},
		{"codebase_search", map[string]any{"query": "charge card"}},
		{"find_implementations", map[string]any{"name": "Payer"}},
	} {
		a := call(t, ctx, toolFrom(t, plain.Tools(), tc.name), tc.args)
		b := call(t, ctx, toolFrom(t, fed.Tools(), tc.name), tc.args)
		if !reflect.DeepEqual(withoutIndex(a), withoutIndex(b)) {
			t.Errorf("%s: %+v != %+v", tc.name, a, b)
		}
	}
	got := call(t, ctx, toolFrom(t, fed.Tools(), "graph_query"), map[string]any{"name": "Zebra"})
	if syms, _ := got["symbols"].([]map[string]any); len(syms) != 1 || syms[0]["repo"] != "zoo" {
		t.Errorf("graph_query Zebra = %+v", got)
	}
}

// Impact analysis counts callers in federated repositories: an exported
// declaration another repository calls is a potential breaking change.
func TestFederatedImpactAnalysis(t *testing.T) {
	parent := federatedRepos(t)
	s, err := NewIndexScope(context.Background(), IndexScopeOptions{
		Root: filepath.Join(parent, "users"), DataDir: t.TempDir(), IndexOnStart: true,
		Federation: []FederatedRoot{{Root: filepath.Join(parent, "client")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	got := call(t, context.Background(), toolFrom(t, s.ImpactTools(), "impact_analysis"),
		map[string]any{"file": "lookup/lookup.go", "start_line": 1, "end_line": 5})
	affected, _ := got["affected_symbols"].([]map[string]any)
	if len(affected) != 1 || !reflect.DeepEqual(affected[0]["callers"], []string{"client:Show"}) || got["potential_breaking_change"] != true {
		t.Errorf("impact_analysis = %+v", got)
	}
}
