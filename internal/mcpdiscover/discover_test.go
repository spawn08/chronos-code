package mcpdiscover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/mcp"
)

func TestDiscoverFromFile_CursorStyle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeFile(t, path, `{
		"mcpServers": {
			"filesystem": {
				"command": "npx",
				"args": ["-y", "@modelcontextprotocol/server-filesystem", "."]
			},
			"git": {
				"command": "npx",
				"args": ["-y", "@modelcontextprotocol/server-git"]
			}
		}
	}`)

	servers, err := DiscoverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("got %d servers, want 2", len(servers))
	}
	// sorted by name
	assertServer(t, servers[0], "filesystem", mcp.TransportStdio, "npx")
	assertServer(t, servers[1], "git", mcp.TransportStdio, "npx")
}

func TestDiscoverFromFile_URLServer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeFile(t, path, `{
		"mcpServers": {
			"remote-db": {
				"url": "https://mcp.example.com/db"
			}
		}
	}`)

	servers, err := DiscoverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	if servers[0].Transport != mcp.TransportSSE {
		t.Errorf("transport=%q, want sse", servers[0].Transport)
	}
	if servers[0].URL != "https://mcp.example.com/db" {
		t.Errorf("url=%q, want https://mcp.example.com/db", servers[0].URL)
	}
}

func TestDiscoverFromFile_ExplicitTransport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeFile(t, path, `{
		"mcpServers": {
			"my-server": {
				"type": "stdio",
				"command": "/usr/bin/my-mcp"
			}
		}
	}`)

	servers, err := DiscoverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	if servers[0].Transport != mcp.TransportStdio {
		t.Errorf("transport=%q, want stdio", servers[0].Transport)
	}
}

func TestDiscoverFromFile_SkipsServerWithoutCommandOrURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeFile(t, path, `{
		"mcpServers": {
			"empty": {},
			"valid": {"command": "echo"}
		}
	}`)

	servers, err := DiscoverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	if servers[0].Name != "valid" {
		t.Errorf("name=%q, want valid", servers[0].Name)
	}
}

func TestDiscoverFromFile_Permission(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeFile(t, path, `{"mcpServers": {"x": {"command": "x"}}}`)

	servers, err := DiscoverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if servers[0].Permission != "require_approval" {
		t.Errorf("permission=%q, want require_approval", servers[0].Permission)
	}
}

func TestDiscoverFromFile_MissingFile(t *testing.T) {
	servers, err := DiscoverFromFile("/nonexistent/path/mcp.json")
	if err != nil {
		t.Fatalf("missing file should not error, got: %v", err)
	}
	if len(servers) != 0 {
		t.Fatalf("got %d servers, want 0", len(servers))
	}
}

func TestDiscoverFromFile_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeFile(t, path, `{not valid json`)

	_, err := DiscoverFromFile(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestDiscoverFromFile_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcp.json")
	writeFile(t, path, "")

	servers, err := DiscoverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 0 {
		t.Fatalf("got %d servers, want 0", len(servers))
	}
}

func TestDiscoverFromFile_PackageJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "package.json")
	writeFile(t, path, `{
		"name": "my-project",
		"version": "1.0.0",
		"mcpServers": {
			"db": {"command": "mcp-db", "args": ["--port", "5432"]}
		}
	}`)

	servers, err := DiscoverFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 {
		t.Fatalf("got %d servers, want 1", len(servers))
	}
	assertServer(t, servers[0], "db", mcp.TransportStdio, "mcp-db")
	if len(servers[0].Args) != 2 || servers[0].Args[0] != "--port" {
		t.Errorf("args=%v, want [--port 5432]", servers[0].Args)
	}
}

func TestDiscover_Deduplication(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	cursorDir := filepath.Join(root, ".cursor")
	os.MkdirAll(cursorDir, 0o755)
	vscodeDir := filepath.Join(root, ".vscode")
	os.MkdirAll(vscodeDir, 0o755)

	writeFile(t, filepath.Join(cursorDir, "mcp.json"), `{
		"mcpServers": {
			"shared": {"command": "cursor-version"},
			"cursor-only": {"command": "c"}
		}
	}`)
	writeFile(t, filepath.Join(vscodeDir, "mcp.json"), `{
		"mcpServers": {
			"shared": {"command": "vscode-version"},
			"vscode-only": {"command": "v"}
		}
	}`)

	servers := Discover(root)
	if len(servers) != 3 {
		t.Fatalf("got %d servers, want 3 (shared deduped)", len(servers))
	}

	byName := make(map[string]mcp.ServerConfig)
	for _, s := range servers {
		byName[s.Name] = s
	}

	if byName["shared"].Command != "cursor-version" {
		t.Errorf("shared.command=%q, want cursor-version (first wins)", byName["shared"].Command)
	}
	if _, ok := byName["cursor-only"]; !ok {
		t.Error("missing cursor-only")
	}
	if _, ok := byName["vscode-only"]; !ok {
		t.Error("missing vscode-only")
	}
}

func TestDiscover_EmptyRoot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	servers := Discover(root)
	if len(servers) != 0 {
		t.Fatalf("got %d servers, want 0 for empty root", len(servers))
	}
}

func TestLoad_ProjectAndUserPrecedenceIsDeterministic(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".chronos-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(home, ".chronos-code", "mcp.json"), `{
		"mcpServers": {
			"shared": {"command": "user"},
			"user-only": {"command": "user-only"}
		}
	}`)
	writeFile(t, filepath.Join(root, ".cursor", "mcp.json"), `{
		"mcpServers": {
			"shared": {"command": "cursor"},
			"cursor-only": {"command": "cursor-only"}
		}
	}`)
	writeFile(t, filepath.Join(root, ".mcp.json"), `{
		"mcpServers": {
			"shared": {"command": "project"},
			"project-only": {"command": "project-only"}
		}
	}`)

	snapshot := Load(root)
	if snapshot.Err != nil {
		t.Fatal(snapshot.Err)
	}
	wantNames := []string{"cursor-only", "project-only", "shared", "user-only"}
	if len(snapshot.Servers) != len(wantNames) {
		t.Fatalf("got %d servers, want %d", len(snapshot.Servers), len(wantNames))
	}
	for i, want := range wantNames {
		if snapshot.Servers[i].Name != want {
			t.Errorf("servers[%d].Name=%q, want %q", i, snapshot.Servers[i].Name, want)
		}
	}
	if snapshot.Servers[2].Command != "project" {
		t.Errorf("shared command=%q, want project", snapshot.Servers[2].Command)
	}
}

func TestLoad_MalformedFileHasPathContext(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	path := filepath.Join(root, ".mcp.json")
	writeFile(t, path, `{bad`)

	snapshot := Load(root)
	if snapshot.Err == nil {
		t.Fatal("expected malformed config error")
	}
	if !strings.Contains(snapshot.Err.Error(), path) || !strings.Contains(snapshot.Err.Error(), "parse MCP config") {
		t.Fatalf("error %q does not identify malformed source %s", snapshot.Err, path)
	}
	if snapshot.Servers != nil {
		t.Fatalf("malformed load returned servers: %v", snapshot.Servers)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertServer(t *testing.T, s mcp.ServerConfig, name string, transport mcp.Transport, command string) {
	t.Helper()
	if s.Name != name {
		t.Errorf("name=%q, want %q", s.Name, name)
	}
	if s.Transport != transport {
		t.Errorf("%s: transport=%q, want %q", name, s.Transport, transport)
	}
	if s.Command != command {
		t.Errorf("%s: command=%q, want %q", name, s.Command, command)
	}
}
