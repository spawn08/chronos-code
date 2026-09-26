package query_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/query"
)

// contractPaths follows client → contract → handler from declaration from:
// each contract the client uses, as "key -> handler" for every handler of
// every node of that key ("key -> " when a node has no handler, e.g. a
// contract file's declaration).
func contractPaths(t *testing.T, v *query.View, from string) []string {
	t.Helper()
	syms := v.ByQualified([]string{from})[from]
	if len(syms) != 1 {
		t.Fatalf("%s: %d declarations", from, len(syms))
	}
	var out []string
	for _, c := range v.Outgoing(syms[0]) {
		if c.Kind != facts.RefContract {
			continue
		}
		nodes, label := v.Resolve(c)
		if len(nodes) == 0 {
			out = append(out, c.Callee+" -> unresolved")
			continue
		}
		if label != query.ImportResolved {
			t.Errorf("%s: contract %s resolved %s", from, c.Callee, label)
		}
		for _, n := range nodes {
			handlers := handlersOf(v, n)
			if len(handlers) == 0 {
				out = append(out, fmt.Sprintf("%s -> [%s %s]", c.Callee, n.Kind, n.File))
			}
			for _, h := range handlers {
				out = append(out, c.Callee+" -> "+h)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func handlersOf(v *query.View, n query.Symbol) []string {
	var out []string
	for _, h := range v.Outgoing(n) {
		if h.Kind != facts.RefCall {
			continue
		}
		targets, _ := v.Resolve(h)
		if len(targets) > 0 {
			out = append(out, targets[0].Qualified())
		}
	}
	return out
}

func expectPaths(t *testing.T, got []string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("missing path %q; got %q", w, got)
		}
	}
}

// callersOf returns the qualified callers reaching a contract node or
// handler, sorted.
func callersOf(v *query.View, s query.Symbol) []string {
	var out []string
	for _, in := range v.IncomingCalls(s) {
		out = append(out, in.Caller.Qualified())
	}
	slices.Sort(out)
	return out
}

func contractNode(t *testing.T, v *query.View, key, kind, file string) query.Symbol {
	t.Helper()
	for _, s := range v.Symbols(key, kind) {
		if s.File == file {
			return s
		}
	}
	t.Fatalf("no %s %q in %s; have %+v", kind, key, file, v.Symbols(key, ""))
	return query.Symbol{}
}

func TestContractsExpress(t *testing.T) {
	v := newFixture(t, map[string]string{
		"server/routes.js": `const express = require('express');
const users = require('./users');
const app = express();

function getUser(req, res) { res.json({}); }

app.get('/api/users/:id', getUser);
app.post('/api/users', auth, users.createUser);
app.delete('/api/users/:id', (req, res) => { res.json(remove(req)); });
`,
		"server/users.js": "function createUser(req, res) {}\nmodule.exports = { createUser };\n",
		"web/api.ts": "export async function loadUser(id: string) {\n  return fetch(`/api/users/${id}`);\n}\n\n" +
			"export async function saveUser(u: any) {\n  return axios.post('/api/users', u);\n}\n\n" +
			"export async function dropUser(id: string) {\n  return fetch(`${BASE}/api/users/${id}`, { method: 'DELETE' });\n}\n",
	}).view(t)
	expectPaths(t, contractPaths(t, v, "loadUser"), "GET /api/users/{} -> getUser")
	expectPaths(t, contractPaths(t, v, "saveUser"), "POST /api/users -> createUser")
	// An inline handler: the route is found, with no handler.
	expectPaths(t, contractPaths(t, v, "dropUser"), "DELETE /api/users/{} -> [route server/routes.js]")
	route := contractNode(t, v, "GET /api/users/{}", facts.KindRoute, "server/routes.js")
	if got := callersOf(v, route); !slices.Equal(got, []string{"loadUser"}) {
		t.Errorf("callers of route = %v", got)
	}
	handler := v.Symbols("getUser", "func")[0]
	if got := callersOf(v, handler); !slices.Equal(got, []string{"GET /api/users/{}"}) {
		t.Errorf("callers of handler = %v", got)
	}
}

func TestContractsFastAPIAndFlask(t *testing.T) {
	v := newFixture(t, map[string]string{
		"api/users.py": `from fastapi import APIRouter

router = APIRouter(prefix="/v1/users")

@router.get("/{user_id}")
async def read_user(user_id: int):
    return {}

class Admin:
    @router.post(
        "/{user_id}/ban",
    )
    def ban(self, user_id: int):
        pass
`,
		"legacy/app.py": `from flask import Flask
app = Flask(__name__)

@app.route("/orders/<int:order_id>", methods=["PUT"])
@login_required
def update_order(order_id):
    pass
`,
		"client/sdk.py": `import requests

def fetch_user(uid):
    return requests.get(f"https://api.example.com/v1/users/{uid}")

def ban_user(uid):
    return requests.post(f"/v1/users/{uid}/ban")

def put_order(oid):
    return requests.put("/orders/" + str(oid))

def put_order2(oid):
    return requests.put(f"/orders/{oid}")
`,
	}).view(t)
	expectPaths(t, contractPaths(t, v, "fetch_user"), "GET /v1/users/{} -> read_user")
	expectPaths(t, contractPaths(t, v, "ban_user"), "POST /v1/users/{}/ban -> Admin.ban")
	expectPaths(t, contractPaths(t, v, "put_order2"), "PUT /orders/{} -> update_order")
}

func TestContractsSpring(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/com/acme/UserController.java": `package com.acme;

@RestController
@RequestMapping("/api/v1")
public class UserController {
    @GetMapping("/users/{id}")
    public User get(@PathVariable long id) { return null; }

    @PostMapping(value = "/users")
    @ResponseStatus(HttpStatus.CREATED)
    public User create(@RequestBody User u) { return u; }

    @RequestMapping(value = "/users/{id}", method = RequestMethod.DELETE)
    public void delete(@PathVariable long id) {}
}
`,
		"src/main/java/com/acme/UserClient.java": `package com.acme;

public class UserClient {
    public User fetch(long id) {
        return restTemplate.getForObject("http://users/api/v1/users/{id}", User.class, id);
    }
    public void remove(long id) {
        restTemplate.exchange("/api/v1/users/" + id, HttpMethod.DELETE, null, Void.class);
    }
    public User add(User u) {
        return webClient.post().uri("/api/v1/users").retrieve().bodyToMono(User.class).block();
    }
}
`,
	}).view(t)
	expectPaths(t, contractPaths(t, v, "UserClient.fetch"), "GET /api/v1/users/{} -> UserController.get")
	expectPaths(t, contractPaths(t, v, "UserClient.add"), "POST /api/v1/users -> UserController.create")
	if got := v.Symbols("DELETE /api/v1/users/{}", facts.KindRoute); len(got) != 1 {
		t.Errorf("RequestMapping with a method: %+v", got)
	}
}

func TestContractsGoHTTP(t *testing.T) {
	v := newFixture(t, map[string]string{
		"go.mod": "module example.com/svc\n",
		"server/server.go": `package server

import "net/http"

type Server struct{}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {}

func health(w http.ResponseWriter, r *http.Request) {}

func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /users/{id}", s.getUser)
	mux.Handle("/healthz", http.HandlerFunc(health))
	mux.HandleFunc("/inline", func(w http.ResponseWriter, r *http.Request) {})
}
`,
		"server/gin.go": `package server

func listOrders(c *Context) {}

func Mount(r *Engine) {
	v1 := r.Group("/v1")
	r.GET("/orders", authMiddleware, listOrders)
	_ = v1
}
`,
		"client/client.go": `package client

import (
	"context"
	"fmt"
	"net/http"
)

func GetUser(ctx context.Context, base string, id int) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/users/%d", base, id), nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func Health() { http.Get("http://localhost:8080/healthz") }

func Orders() { http.Get("/orders") }
`,
	}).view(t)
	expectPaths(t, contractPaths(t, v, "GetUser"), "GET /users/{} -> Server.getUser")
	expectPaths(t, contractPaths(t, v, "Health"), "GET /healthz -> health")
	expectPaths(t, contractPaths(t, v, "Orders"), "GET /orders -> listOrders")
	if got := v.Symbols("ANY /inline", facts.KindRoute); len(got) != 1 || len(v.Outgoing(got[0])) != 0 {
		t.Errorf("inline handler route: %+v", got)
	}
}

const userProto = `syntax = "proto3";
package acme.users.v1;

// UserService manages users.
service UserService {
  rpc GetUser(GetUserRequest) returns (User);
  rpc ListUsers(ListUsersRequest) returns (stream User) {
    option (google.api.http) = { get: "/v1/users" };
  }
}

message User {
  string id = 1;
  message Address { string city = 1; }
}
message GetUserRequest { string id = 1; }
message ListUsersRequest {}
`

func TestContractsGRPC(t *testing.T) {
	v := newFixture(t, map[string]string{
		"proto/users.proto": userProto,
		"go.mod":            "module example.com/svc\n",
		"gosrv/server.go": `package gosrv

type userServer struct{ pb.UnimplementedUserServiceServer }

func (s *userServer) GetUser(ctx context.Context, r *pb.GetUserRequest) (*pb.User, error) { return nil, nil }
func (s *userServer) ListUsers(r *pb.ListUsersRequest, st pb.UserService_ListUsersServer) error { return nil }

func Register(g *grpc.Server) {
	pb.RegisterUserServiceServer(g, &userServer{})
}
`,
		"gocli/client.go": `package gocli

type Client struct{ users pb.UserServiceClient }

func New(conn *grpc.ClientConn) *Client {
	return &Client{users: pb.NewUserServiceClient(conn)}
}

func (c *Client) Lookup(ctx context.Context, id string) (*pb.User, error) {
	return c.users.GetUser(ctx, &pb.GetUserRequest{Id: id})
}
`,
		"pysrv/server.py": `import users_pb2_grpc

class Users(users_pb2_grpc.UserServiceServicer):
    def GetUser(self, request, context):
        return None

    def _helper(self):
        pass
`,
		"pycli/client.py": `import grpc
import users_pb2_grpc

def lookup(channel, uid):
    stub = users_pb2_grpc.UserServiceStub(channel)
    return stub.ListUsers(uid)
`,
		"src/main/java/acme/UserServiceImpl.java": `package acme;

public class UserServiceImpl extends UserServiceGrpc.UserServiceImplBase {
    @Override
    public void getUser(GetUserRequest req, StreamObserver<User> obs) {}
}
`,
		"src/main/java/acme/Caller.java": `package acme;

public class Caller {
    private final UserServiceGrpc.UserServiceBlockingStub stub;
    Caller(Channel ch) { this.stub = UserServiceGrpc.newBlockingStub(ch); }
    public User find(String id) { return stub.getUser(GetUserRequest.newBuilder().setId(id).build()); }
}
`,
	}).view(t)
	rpc := contractNode(t, v, "UserService/GetUser", facts.KindRPC, "proto/users.proto")
	if rpc.PkgName != "acme.users.v1" || rpc.Parent != "UserService" {
		t.Errorf("proto rpc = %+v", rpc)
	}
	if got := v.Symbols("User.Address", facts.KindMessage); len(got) != 1 {
		t.Errorf("nested message: %+v", got)
	}
	goPaths := contractPaths(t, v, "Client.Lookup")
	expectPaths(t, goPaths, "UserService/GetUser -> userServer.GetUser", "UserService/GetUser -> Users.GetUser",
		"UserService/GetUser -> UserServiceImpl.getUser", "UserService/GetUser -> [rpc proto/users.proto]")
	expectPaths(t, contractPaths(t, v, "lookup"), "UserService/ListUsers -> userServer.ListUsers")
	expectPaths(t, contractPaths(t, v, "Caller.find"), "UserService/GetUser -> UserServiceImpl.getUser")
	if got := callersOf(v, rpc); !slices.Equal(got, []string{"Caller.find", "Client.Lookup"}) {
		t.Errorf("callers of rpc = %v", got)
	}
}

func TestContractsKafka(t *testing.T) {
	v := newFixture(t, map[string]string{
		"src/main/java/acme/OrderEvents.java": `package acme;

public class OrderEvents {
    @KafkaListener(topics = "orders.created", groupId = "billing")
    public void onCreated(String payload) {}

    public void publish(Order o) {
        kafkaTemplate.send("orders.shipped", o.id(), o);
    }
}
`,
		"py/worker.py": `from kafka import KafkaConsumer, KafkaProducer

def consume_shipped():
    consumer = KafkaConsumer("orders.shipped", group_id="mail")
    for msg in consumer:
        print(msg)

def emit_created(p, order):
    p.send("orders.created", order)
`,
		"go.mod": "module example.com/svc\n",
		"gosvc/events.go": `package gosvc

func Consume() {
	r := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers, Topic: "payments"})
	_ = r
}

func Pay(w *kafka.Writer) {
	w.WriteMessages(ctx, kafka.Message{Topic: "payments", Value: v})
}
`,
		"js/audit.js": `async function startAudit(consumer) {
  await consumer.subscribe({ topics: ['audit'] });
}

async function auditEvent(producer, e) {
  await producer.send({ topic: 'audit', messages: [e] });
}
`,
	}).view(t)
	expectPaths(t, contractPaths(t, v, "emit_created"), "orders.created -> OrderEvents.onCreated")
	expectPaths(t, contractPaths(t, v, "OrderEvents.publish"), "orders.shipped -> consume_shipped")
	expectPaths(t, contractPaths(t, v, "Pay"), "payments -> Consume")
	expectPaths(t, contractPaths(t, v, "auditEvent"), "audit -> startAudit")
}

func TestContractFiles(t *testing.T) {
	v := newFixture(t, map[string]string{
		"api/openapi.yaml": `openapi: 3.0.0
info:
  title: Users API
servers:
  - url: https://api.example.com/v2
paths:
  /users/{id}:
    get:
      operationId: getUser
      summary: Fetch one user
    delete:
      operationId: deleteUser
components:
  schemas:
    User:
      type: object
`,
		"api/schema.graphql": `# The schema
type Query {
  user(id: ID!): User
  users(first: Int = 10): [User!]!
}

type Mutation {
  createUser(input: NewUser!): User
}

type User { id: ID! name: String }
input NewUser { name: String! }
`,
		"api/users.thrift": `namespace go acme.users
namespace java com.acme.users

struct User { 1: i64 id, 2: string name }
exception NotFound {}

service UserStore extends Base {
  User getUser(1: i64 id) throws (1: NotFound nf),
  oneway void ping()
  list<User> listUsers(1: i32 limit);
}
`,
		"db/schema.sql": `CREATE TABLE IF NOT EXISTS public.users (
  id BIGINT PRIMARY KEY,
  name TEXT
);

create table orders (
  id bigint primary key,
  user_id bigint references users(id)
);
`,
		"web/client.ts": "export async function loadUser(id: string) {\n  return fetch(`https://api.example.com/v2/users/${id}`);\n}\n\n" +
			"const Q = gql`\n  query GetUser($id: ID!) {\n    user(id: $id) { name }\n  }\n`;\n" +
			"export function useUser(id: string) {\n  return client.query({ query: gql`query { users { id } }` });\n}\n",
		"go.mod":          "module example.com/svc\n",
		"store/orders.go": "package store\n\nfunc CountOrders(db *sql.DB) int {\n\trow := db.QueryRow(`SELECT count(*) FROM orders WHERE user_id = $1`, 1)\n\t_ = row\n\treturn 0\n}\n",
	}).view(t)
	expectPaths(t, contractPaths(t, v, "loadUser"), "GET /v2/users/{} -> [route api/openapi.yaml]")
	expectPaths(t, contractPaths(t, v, "useUser"), "Query/Users -> [rpc api/schema.graphql]")
	expectPaths(t, contractPaths(t, v, "CountOrders"), "orders -> [table db/schema.sql]")

	op := contractNode(t, v, "DELETE /v2/users/{}", facts.KindRoute, "api/openapi.yaml")
	if op.Signature != "DELETE /v2/users/{id} (deleteUser)" {
		t.Errorf("openapi signature = %q", op.Signature)
	}
	contractNode(t, v, "User", facts.KindMessage, "api/openapi.yaml")
	contractNode(t, v, "Mutation/CreateUser", facts.KindRPC, "api/schema.graphql")
	contractNode(t, v, "NewUser", facts.KindMessage, "api/schema.graphql")
	for _, key := range []string{"UserStore/GetUser", "UserStore/Ping", "UserStore/ListUsers"} {
		contractNode(t, v, key, facts.KindRPC, "api/users.thrift")
	}
	contractNode(t, v, "NotFound", facts.KindMessage, "api/users.thrift")
	users := contractNode(t, v, "users", facts.KindTable, "db/schema.sql")
	if users.Line != 1 || users.EndLine != 4 {
		t.Errorf("users table lines %d-%d", users.Line, users.EndLine)
	}
	// A foreign key is a use of the referenced table.
	if got := callersOf(v, users); !slices.Equal(got, []string{"orders"}) {
		t.Errorf("tables referencing users = %v", got)
	}
	// The anonymous-selection query in the first gql string names GetUser's field.
	var keys []string
	for _, s := range v.Symbols("Query/User", facts.KindRPC) {
		keys = append(keys, s.File)
	}
	if !strings.Contains(strings.Join(keys, ","), "api/schema.graphql") {
		t.Errorf("Query/User = %v", keys)
	}
}
