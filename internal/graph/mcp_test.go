package graph

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestMCPMatchesInProcessTools is the M9 acceptance for the MCP adapter:
// over stdio, an external agent lists the same tools and gets the same
// results (federated repositories included) as the in-process tools.
func TestMCPMatchesInProcessTools(t *testing.T) {
	parent := federatedRepos(t)
	// Separate scopes, so session state (codebase_context's delivered
	// source) cannot leak between the two sides.
	local, remote := openFederated(t, parent), openFederated(t, parent)
	ctx := context.Background()

	calls := []struct{ name, args string }{
		{"graph_query", `{"name": "Find"}`},
		{"graph_query", `{"names": ["Find", "Show", "Nope"]}`},
		{"codebase_search", `{"query": "user by id", "top_k": 5}`},
		{"codebase_context", `{"query": "show a user", "max_tokens": 1024}`},
		{"codebase_map", `{}`},
		{"find_callers", `{"name": "getUser", "depth": 2}`},
		{"find_callers", `{"names": ["Find", "Show"]}`},
		{"find_implementations", `{"name": "Payer"}`},
		{"multi_resolution_view", `{"target": "", "level": "L0"}`},
		{"multi_resolution_view", `{"target": "Find", "level": "L2"}`},
		{"multi_resolution_view", `{"target": "Show", "level": "L3"}`},
		{"resolve_symbol", `{"name": "Find"}`},
		{"impact_analysis", `{"file": "app/app.go", "start_line": 1, "end_line": 20}`},
		{"test_map", `{"symbol": "Show"}`},
		{"co_change", `{"file": "app/app.go", "days": 30}`},
		{"graph_query", `{}`}, // an error, reported as an MCP tool error
	}

	var in bytes.Buffer
	in.WriteString(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}` + "\n")
	for i, c := range calls {
		fmt.Fprintf(&in, `{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`+"\n", i+2, c.name, c.args)
	}
	var out bytes.Buffer
	if err := NewMCPServer(remote, "test").ServeStdio(ctx, &in, &out); err != nil {
		t.Fatal(err)
	}
	responses := map[int]json.RawMessage{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var r struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  any             `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil || r.Error != nil {
			t.Fatalf("response %s: %v", line, err)
		}
		responses[r.ID] = r.Result
	}
	if len(responses) != len(calls)+2 {
		t.Fatalf("%d responses for %d requests", len(responses), len(calls)+2)
	}

	var init struct {
		ServerInfo struct{ Name string } `json:"serverInfo"`
	}
	_ = json.Unmarshal(responses[0], &init)
	if init.ServerInfo.Name != MCPServerName {
		t.Errorf("serverInfo = %s", responses[0])
	}

	// tools/list: the same names, descriptions and schemas.
	var list struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(responses[1], &list); err != nil {
		t.Fatal(err)
	}
	defs := append(local.Tools(), local.ImpactTools()...)
	var want, got []string
	for _, d := range defs {
		schema, _ := json.Marshal(d.Parameters)
		want = append(want, d.Name+"\x00"+d.Description+"\x00"+string(schema))
	}
	for _, tl := range list.Tools {
		schema, _ := json.Marshal(tl.InputSchema)
		got = append(got, tl.Name+"\x00"+tl.Description+"\x00"+string(schema))
	}
	sort.Strings(want)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tools/list differs:\n got %q\nwant %q", got, want)
	}

	for i, c := range calls {
		var args map[string]any
		if err := json.Unmarshal([]byte(c.args), &args); err != nil {
			t.Fatal(err)
		}
		var expected any
		isErr := false
		res, err := toolFrom(t, defs, c.name).Handler(ctx, args)
		if err != nil {
			expected, isErr = err.Error(), true
		} else {
			data, err := json.Marshal(res)
			if err != nil {
				t.Fatal(err)
			}
			if s, ok := res.(string); ok {
				expected = s
			} else if err := json.Unmarshal(data, &expected); err != nil {
				t.Fatal(err)
			}
		}

		var result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		}
		if err := json.Unmarshal(responses[i+2], &result); err != nil || len(result.Content) != 1 {
			t.Fatalf("%s %s: result %s", c.name, c.args, responses[i+2])
		}
		var actual any = result.Content[0].Text
		if !result.IsError {
			if _, isString := expected.(string); !isString {
				if err := json.Unmarshal([]byte(result.Content[0].Text), &actual); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
			}
		}
		if result.IsError != isErr || !reflect.DeepEqual(actual, expected) {
			t.Errorf("%s %s:\n  mcp        %v (error %v)\n  in-process %v (error %v)", c.name, c.args, actual, result.IsError, expected, isErr)
		}
	}
	// The federated repository answers over MCP too.
	if !strings.Contains(string(responses[2]), `\"repo\":\"users\"`) {
		t.Errorf("graph_query Find over MCP = %s", responses[2])
	}
}
