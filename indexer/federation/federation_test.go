package federation_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/spawn08/chronos-code/indexer"
	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/federation"
	"github.com/spawn08/chronos-code/indexer/query"
)

type repo struct {
	name  string
	files map[string]string
}

// workspace indexes each repo in its own directory and returns a
// workspace over their current snapshots, in order.
func workspace(t *testing.T, repos ...repo) *federation.Workspace {
	t.Helper()
	parent := t.TempDir()
	var members []federation.Member
	for _, r := range repos {
		root := filepath.Join(parent, r.name)
		for name, text := range r.files {
			path := filepath.Join(root, filepath.FromSlash(name))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		eng, err := indexer.Open(indexer.Options{Root: root, Dir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = eng.Close() })
		if _, err := eng.Reconcile(context.Background()); err != nil {
			t.Fatal(err)
		}
		sn := eng.Snapshot()
		t.Cleanup(sn.Release)
		members = append(members, federation.Member{Name: r.name, View: query.NewView(sn, query.NewCache())})
	}
	w, err := federation.New(members...)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

// one returns the single declaration with qualified identity q in repo.
func one(t *testing.T, w *federation.Workspace, repoName, q string) federation.Symbol {
	t.Helper()
	for _, m := range w.Members() {
		if m.Name != repoName {
			continue
		}
		syms := m.View.ByQualified([]string{q})[q]
		if len(syms) != 1 {
			t.Fatalf("%s:%s: %d declarations", repoName, q, len(syms))
		}
		return federation.Symbol{Repo: repoName, Symbol: syms[0]}
	}
	t.Fatalf("no member %q", repoName)
	return federation.Symbol{}
}

// render formats edges as "caller -> callee [label via]".
func render(edges []federation.Edge) []string {
	out := make([]string, len(edges))
	for i, e := range edges {
		via := e.Via
		if via == "" {
			via = "local"
		}
		out[i] = fmt.Sprintf("%s:%s -> %s:%s [%s %s]", e.Caller.Repo, e.Caller.Qualified(), e.Callee.Repo, e.Callee.Qualified(), e.Resolution, via)
	}
	return out
}

func expect(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing %q; got %q", w, got)
		}
	}
}

func reject(t *testing.T, got []string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if slices.Contains(got, u) {
			t.Errorf("unexpected %q; got %q", u, got)
		}
	}
}

const goUsersB = `package users

// Lookup finds a user.
func Lookup(id string) string { return normalize(id) }

func normalize(id string) string { return id }
`

func TestNewRejectsBadMembers(t *testing.T) {
	if _, err := federation.New(); err == nil {
		t.Error("no members accepted")
	}
	if _, err := federation.New(federation.Member{Name: "a"}); err == nil {
		t.Error("member without a view accepted")
	}
}

func TestSingleMemberMatchesView(t *testing.T) {
	w := workspace(t, repo{"b", map[string]string{"go.mod": "module example.com/b\n", "users/users.go": goUsersB}})
	v := w.Members()[0].View
	if got, want := w.Symbols("Lookup", ""), v.Symbols("Lookup", ""); len(got) != 1 || !reflect.DeepEqual(got[0].Symbol, want[0]) || got[0].Repo != "b" {
		t.Fatalf("Symbols = %+v, want %+v", got, want)
	}
	norm := one(t, w, "b", "normalize")
	fed := w.IncomingEdges([]federation.Symbol{norm})
	local := v.IncomingEdges([]query.Symbol{norm.Symbol})
	if len(fed) != len(local) || len(fed) != 1 {
		t.Fatalf("incoming: federated %v, view %v", render(fed), local)
	}
	for i := range fed {
		if !reflect.DeepEqual(fed[i].Caller.Symbol, local[i].Caller) || fed[i].Resolution != local[i].Resolution || fed[i].Via != federation.ViaLocal {
			t.Errorf("edge %d: %+v vs %+v", i, fed[i], local[i])
		}
	}
	lookup := one(t, w, "b", "Lookup")
	if got := render(w.OutgoingEdges([]federation.Symbol{lookup})); !slices.Equal(got, []string{"b:Lookup -> b:normalize [import_resolved local]"}) {
		t.Errorf("outgoing = %q", got)
	}
}

func TestCrossRepoImportGo(t *testing.T) {
	w := workspace(t,
		repo{"a", map[string]string{
			"go.mod": "module example.com/a\n",
			"cmd/main.go": `package main

import (
	"fmt"

	"example.com/b/users"
	cusers "example.com/c/users"
)

func Run() {
	fmt.Println(users.Lookup("42"))
	_ = cusers.Lookup
	helper()
}

func helper() {}
`,
		}},
		repo{"b", map[string]string{"go.mod": "module example.com/b\n", "users/users.go": goUsersB}},
		// c declares the same package name and function in another module
		// path; a's call through example.com/b/users must not reach it.
		repo{"c", map[string]string{"go.mod": "module example.com/other\n", "users/users.go": goUsersB}},
	)
	run := one(t, w, "a", "Run")
	out := render(w.OutgoingEdges([]federation.Symbol{run}))
	expect(t, out, "a:Run -> b:Lookup [import_resolved import]", "a:Run -> a:helper [import_resolved local]")
	reject(t, out, "a:Run -> c:Lookup [import_resolved import]")

	in := render(w.IncomingEdges([]federation.Symbol{one(t, w, "b", "Lookup")}))
	if !slices.Equal(in, []string{"a:Run -> b:Lookup [import_resolved import]"}) {
		t.Errorf("incoming b:Lookup = %q", in)
	}
	if in := w.IncomingEdges([]federation.Symbol{one(t, w, "c", "Lookup")}); len(in) != 0 {
		t.Errorf("incoming c:Lookup = %q", render(in))
	}
	// Unexported declarations are not importable.
	if in := w.IncomingEdges([]federation.Symbol{one(t, w, "b", "normalize")}); !slices.Equal(render(in), []string{"b:Lookup -> b:normalize [import_resolved local]"}) {
		t.Errorf("incoming b:normalize = %q", render(in))
	}
	// CallerEdges by name spans members.
	expect(t, render(w.CallerEdges("Lookup")), "a:Run -> b:Lookup [import_resolved import]")
}

func TestCrossRepoImportLanguages(t *testing.T) {
	tests := []struct {
		name         string
		a, b         map[string]string
		caller, want string
	}{
		{
			name: "typescript package name",
			a: map[string]string{
				"package.json":  `{"name": "web"}`,
				"src/signup.ts": "import { createUser as make } from '@acme/users';\n\nexport function signup(name: string) {\n  return make(name);\n}\n",
			},
			b: map[string]string{
				"package.json":   `{"name": "@acme/users", "main": "dist/index.js"}`,
				"src/index.ts":   "export { createUser } from './create';\n",
				"src/create.ts":  "export function createUser(name: string) {\n  return { name };\n}\n",
				"src/unused.ts":  "export function other() {}\n",
				"tsconfig.json":  `{"compilerOptions": {"outDir": "dist"}}`,
				"src/helpers.ts": "export const x = 1;\n",
			},
			caller: "signup", want: "a:signup -> b:createUser [import_resolved import]",
		},
		{
			name: "python package",
			a: map[string]string{
				"app/main.py": "from bsvc.client import make_client as mk\n\n\ndef run():\n    return mk()\n",
			},
			b: map[string]string{
				"bsvc/__init__.py": "",
				"bsvc/client.py":   "def make_client():\n    return object()\n",
			},
			caller: "run", want: "a:run -> b:make_client [import_resolved import]",
		},
		{
			name: "java static import of a declared package",
			a: map[string]string{
				"src/main/java/com/acme/a/App.java": "package com.acme.a;\n\nimport static com.acme.b.Util.greet;\n\npublic class App {\n    public String run() { return greet(\"x\"); }\n}\n",
			},
			b: map[string]string{
				"lib/src/com/acme/b/Util.java": "package com.acme.b;\n\npublic class Util {\n    public static String greet(String n) { return n; }\n}\n",
			},
			caller: "App.run", want: "a:App.run -> b:Util.greet [import_resolved import]",
		},
		{
			name: "rust crate",
			a: map[string]string{
				"Cargo.toml":  "[package]\nname = \"app\"\n",
				"src/main.rs": "use b_core::model::load;\n\nfn run() {\n    load();\n}\n\nfn main() { run(); }\n",
			},
			b: map[string]string{
				"Cargo.toml":   "[package]\nname = \"b-core\"\n",
				"src/lib.rs":   "pub mod model;\n",
				"src/model.rs": "pub fn load() {}\n",
			},
			caller: "run", want: "a:run -> b:load [import_resolved import]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := workspace(t, repo{"a", tt.a}, repo{"b", tt.b})
			caller := one(t, w, "a", tt.caller)
			expect(t, render(w.OutgoingEdges([]federation.Symbol{caller})), tt.want)
			var callee federation.Symbol
			for _, e := range w.OutgoingEdges([]federation.Symbol{caller}) {
				if e.Via == federation.ViaImport {
					callee = e.Callee
				}
			}
			if callee.Repo == "" {
				return
			}
			expect(t, render(w.IncomingEdges([]federation.Symbol{callee})), tt.want)
		})
	}
}

// contractNode returns repo's contract node of kind named key in file.
func contractNode(t *testing.T, w *federation.Workspace, repoName, key, kind, file string) federation.Symbol {
	t.Helper()
	for _, s := range w.Symbols(key, kind) {
		if s.Repo == repoName && s.File == file {
			return s
		}
	}
	t.Fatalf("no %s %q in %s:%s; have %+v", kind, key, repoName, file, w.Symbols(key, ""))
	return federation.Symbol{}
}

func TestCrossRepoContractRoute(t *testing.T) {
	w := workspace(t,
		repo{"web", map[string]string{
			"src/api.ts": "export async function loadUser(id: string) {\n  return fetch(`/api/users/${id}`);\n}\n",
		}},
		repo{"users", map[string]string{
			"server/routes.js": "const app = require('express')();\n\nfunction getUser(req, res) { res.json({}); }\n\napp.get('/api/users/:id', getUser);\n",
		}},
	)
	load := one(t, w, "web", "loadUser")
	route := contractNode(t, w, "users", "GET /api/users/{}", facts.KindRoute, "server/routes.js")
	// client (web) -> route (users) -> handler (users)
	expect(t, render(w.OutgoingEdges([]federation.Symbol{load})), "web:loadUser -> users:GET /api/users/{} [import_resolved contract]")
	expect(t, render(w.OutgoingEdges([]federation.Symbol{route})), "users:GET /api/users/{} -> users:getUser [import_resolved local]")
	if got := render(w.IncomingEdges([]federation.Symbol{route})); !slices.Equal(got, []string{"web:loadUser -> users:GET /api/users/{} [import_resolved contract]"}) {
		t.Errorf("incoming route = %q", got)
	}
}

const usersProto = `syntax = "proto3";
package acme.users.v1;

service UserService {
  rpc GetUser(GetUserRequest) returns (User);
}
message User { string id = 1; }
message GetUserRequest { string id = 1; }
`

func TestCrossRepoContractRPCPackage(t *testing.T) {
	other := `syntax = "proto3";
package other.v1;

service UserService {
  rpc GetUser(Req) returns (Resp);
}
`
	w := workspace(t,
		// The client repository carries its own copy of the proto.
		repo{"client", map[string]string{
			"proto/users.proto": usersProto,
			"go.mod":            "module example.com/client\n",
			"gocli/client.go": `package gocli

type Client struct{ users pb.UserServiceClient }

func New(conn *grpc.ClientConn) *Client {
	return &Client{users: pb.NewUserServiceClient(conn)}
}

func (c *Client) Lookup(ctx context.Context, id string) (*pb.User, error) {
	return c.users.GetUser(ctx, &pb.GetUserRequest{Id: id})
}
`,
		}},
		repo{"server", map[string]string{
			"proto/users.proto": usersProto,
			"pysrv/server.py": `import users_pb2_grpc

class Users(users_pb2_grpc.UserServiceServicer):
    def GetUser(self, request, context):
        return None
`,
		}},
		// Same service and method, another proto package: not joined.
		repo{"unrelated", map[string]string{"proto/users.proto": other}},
	)
	lookup := one(t, w, "client", "Client.Lookup")
	out := render(w.OutgoingEdges([]federation.Symbol{lookup}))
	expect(t, out,
		"client:Client.Lookup -> server:UserService/GetUser [import_resolved contract]")
	for _, e := range w.OutgoingEdges([]federation.Symbol{lookup}) {
		if e.Callee.Repo == "unrelated" {
			t.Errorf("joined another proto package: %+v", e)
		}
	}
	// The server's rpc node declared by the recogniser leads to its handler.
	var handlers []string
	for _, s := range w.Symbols("UserService/GetUser", facts.KindRPC) {
		if s.Repo != "server" {
			continue
		}
		for _, e := range w.OutgoingEdges([]federation.Symbol{s}) {
			handlers = append(handlers, e.Callee.Qualified())
		}
	}
	if !slices.Contains(handlers, "Users.GetUser") {
		t.Errorf("server rpc handlers = %v", handlers)
	}
	rpc := contractNode(t, w, "server", "UserService/GetUser", facts.KindRPC, "proto/users.proto")
	expect(t, render(w.IncomingEdges([]federation.Symbol{rpc})), "client:Client.Lookup -> server:UserService/GetUser [import_resolved contract]")
	if in := w.IncomingEdges([]federation.Symbol{contractNode(t, w, "unrelated", "UserService/GetUser", facts.KindRPC, "proto/users.proto")}); len(in) != 0 {
		t.Errorf("incoming unrelated rpc = %q", render(in))
	}
}

func TestCrossRepoContractTopicAmbiguous(t *testing.T) {
	consumer := func(fn string) map[string]string {
		return map[string]string{"worker.py": "from kafka import KafkaConsumer\n\ndef " + fn + "():\n    consumer = KafkaConsumer(\"orders.created\", group_id=\"g\")\n    for m in consumer:\n        print(m)\n"}
	}
	w := workspace(t,
		repo{"orders", map[string]string{
			"src/main/java/acme/OrderEvents.java": "package acme;\n\npublic class OrderEvents {\n    public void publish(Order o) {\n        kafkaTemplate.send(\"orders.created\", o.id(), o);\n    }\n}\n",
		}},
		repo{"billing", consumer("bill")},
		repo{"mail", consumer("notify")},
	)
	publish := one(t, w, "orders", "OrderEvents.publish")
	out := w.OutgoingEdges([]federation.Symbol{publish})
	repos := map[string]bool{}
	for _, e := range out {
		if e.Via != federation.ViaContract {
			continue
		}
		repos[e.Callee.Repo] = true
		if e.Resolution != query.Ambiguous || e.Candidates < 2 {
			t.Errorf("topic consumed in two repositories: %+v", e)
		}
	}
	if !repos["billing"] || !repos["mail"] {
		t.Errorf("topic joined %v; edges %q", repos, render(out))
	}
}
