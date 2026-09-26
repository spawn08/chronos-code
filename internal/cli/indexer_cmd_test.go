package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/graph"
)

func TestIndexerFederationFlags(t *testing.T) {
	root := filepath.FromSlash("/work/app")
	got, err := indexerFederation(root, []config.FederatedRepo{{Name: "cfg", Root: "/srv/cfg"}},
		[]string{"--repo", "../users", "--repo=billing=/srv/billing"})
	if err != nil {
		t.Fatal(err)
	}
	want := []graph.FederatedRoot{{Name: "cfg", Root: "/srv/cfg"}, {Root: filepath.Join(root, "../users")}, {Name: "billing", Root: "/srv/billing"}}
	if len(got) != len(want) {
		t.Fatalf("federation = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("member %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, bad := range [][]string{{"--repo"}, {"--repo="}, {"--verbose"}} {
		if _, err := indexerFederation(root, nil, bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestIndexerCommandUsage(t *testing.T) {
	for _, args := range [][]string{nil, {"status"}} {
		if err := runIndexerCommand(context.Background(), &config.Config{}, args, nil, nil); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// TestIndexerMCPServesFederatedIndex runs "chronos-code indexer mcp" over
// stdio against a workspace federating a sibling repository.
func TestIndexerMCPServesFederatedIndex(t *testing.T) {
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHRONOS_CODE_DATA_HOME", filepath.Join(parent, "data"))
	app, lib := filepath.Join(parent, "app"), filepath.Join(parent, "lib")
	files := map[string]string{
		filepath.Join(app, "go.mod"):            "module example.com/app\n",
		filepath.Join(app, "main.go"):           "package main\n\nimport \"example.com/lib/greet\"\n\nfunc main() { greet.Hello() }\n",
		filepath.Join(lib, "go.mod"):            "module example.com/lib\n",
		filepath.Join(lib, "greet.go"):          "package lib\n",
		filepath.Join(lib, "greet", "greet.go"): "package greet\n\n// Hello greets.\nfunc Hello() {}\n",
	}
	for path, text := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Workspace: config.WorkspaceConfig{Root: app}}
	in := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n" +
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"find_callers","arguments":{"name":"Hello"}}}` + "\n")
	var out bytes.Buffer
	if err := runIndexerCommand(context.Background(), cfg, []string{"mcp", "--repo", "../lib"}, in, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses: %q", out.String())
	}
	var resp struct {
		Result struct {
			Content []struct{ Text string } `json:"content"`
			IsError bool                    `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &resp); err != nil || resp.Result.IsError || len(resp.Result.Content) != 1 {
		t.Fatalf("find_callers response %s: %v", lines[1], err)
	}
	if text := resp.Result.Content[0].Text; !strings.Contains(text, `"lib:Hello":{"import_resolved":["main (main.go:5, this workspace via import)"]}`) {
		t.Errorf("find_callers over MCP = %s", text)
	}
}
