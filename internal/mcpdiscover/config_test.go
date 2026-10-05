package mcpdiscover

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/mcp"
)

func TestManagedConfigRoundTripPreservesUnknownJSONAndMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".mcp.json")
	original := `{
  "projectSetting": {"keep": true},
  "mcpServers": {
    "existing": {"command":"old", "extension":{"keep":true}}
  }
}`
	if err := os.WriteFile(path, []byte(original), 0o640); err != nil {
		t.Fatal(err)
	}

	stdio := ManagedServer{Name: "stdio", Transport: mcp.TransportStdio, Command: "npx", Args: []string{"-y", "server"}}
	if err := AddManaged(path, stdio, false); err != nil {
		t.Fatalf("AddManaged(stdio): %v", err)
	}
	sse := ManagedServer{Name: "remote", Transport: mcp.TransportSSE, URL: "https://mcp.example.test/events"}
	if err := AddManaged(path, sse, false); err != nil {
		t.Fatalf("AddManaged(sse): %v", err)
	}

	servers, err := ListManaged(path)
	if err != nil {
		t.Fatalf("ListManaged: %v", err)
	}
	if len(servers) != 3 || servers[1].Name != "remote" || servers[1].Transport != mcp.TransportSSE || servers[2].Name != "stdio" {
		t.Fatalf("servers = %#v, want existing, remote SSE, stdio", servers)
	}
	discovered, err := DiscoverFromFile(path)
	if err != nil {
		t.Fatalf("DiscoverFromFile: %v", err)
	}
	if len(discovered) != 3 {
		t.Fatalf("DiscoverFromFile returned %d servers, want 3", len(discovered))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(doc["projectSetting"], []byte(`"keep": true`)) && !bytes.Contains(doc["projectSetting"], []byte(`"keep":true`)) {
		t.Fatalf("unknown top-level field lost: %s", data)
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(doc["mcpServers"], &entries); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entries["existing"], []byte(`"extension"`)) {
		t.Fatalf("unknown server field lost: %s", entries["existing"])
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("mode = %o, want preserved 640", got)
	}

	if err := RemoveManaged(path, "stdio", false); err != nil {
		t.Fatalf("RemoveManaged: %v", err)
	}
	servers, err = ListManaged(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 2 {
		t.Fatalf("after remove got %d servers, want 2", len(servers))
	}
}

func TestManagedConfigFailuresDoNotChangeOriginalBytes(t *testing.T) {
	tests := []struct {
		name    string
		initial string
		mutate  func(string) error
		want    string
	}{
		{
			name:    "duplicate add",
			initial: `{"mcpServers":{"same":{"command":"old"}}}`,
			mutate: func(path string) error {
				return AddManaged(path, ManagedServer{Name: "same", Transport: mcp.TransportStdio, Command: "new"}, false)
			},
			want: "already exists",
		},
		{
			name:    "missing remove",
			initial: `{"mcpServers":{"kept":{"command":"old"}}}`,
			mutate:  func(path string) error { return RemoveManaged(path, "absent", false) },
			want:    "does not exist",
		},
		{
			name:    "malformed source",
			initial: `{"mcpServers":`,
			mutate: func(path string) error {
				return AddManaged(path, ManagedServer{Name: "new", Transport: mcp.TransportStdio, Command: "cmd"}, false)
			},
			want: "parse MCP config",
		},
		{
			name:    "invalid source shape",
			initial: `{"mcpServers":{"bad":{"transport":"websocket","url":"https://example.test"}}}`,
			mutate: func(path string) error {
				return AddManaged(path, ManagedServer{Name: "new", Transport: mcp.TransportStdio, Command: "cmd"}, false)
			},
			want: "unsupported transport",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ".mcp.json")
			before := []byte(tt.initial)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			err := tt.mutate(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("failed mutation changed bytes: before=%q after=%q", before, after)
			}
		})
	}
}

func TestValidateManagedServerRejectsInvalidShapeTransportURLAndSecrets(t *testing.T) {
	tests := []struct {
		name   string
		server ManagedServer
		want   string
	}{
		{name: "unknown transport", server: ManagedServer{Name: "x", Transport: "websocket", URL: "https://example.test"}, want: "unsupported transport"},
		{name: "plain http streamable", server: ManagedServer{Name: "x", Transport: mcp.TransportStreamableHTTP, URL: "http://example.test"}, want: "allowed_insecure_hosts"},
		{name: "insecure SSE", server: ManagedServer{Name: "x", Transport: mcp.TransportSSE, URL: "http://example.test"}, want: "absolute HTTPS"},
		{name: "SSE command", server: ManagedServer{Name: "x", Transport: mcp.TransportSSE, URL: "https://example.test", Command: "bad"}, want: "cannot include command"},
		{name: "stdio URL", server: ManagedServer{Name: "x", Transport: mcp.TransportStdio, Command: "cmd", URL: "https://example.test"}, want: "cannot include url"},
		{name: "expanded arg secret", server: ManagedServer{Name: "x", Transport: mcp.TransportStdio, Command: "cmd", Args: []string{"--api-key", "plaintext"}}, want: "environment reference"},
		{name: "expanded query secret", server: ManagedServer{Name: "x", Transport: mcp.TransportSSE, URL: "https://example.test?token=plaintext"}, want: "environment reference"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateManagedServer(tt.server)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}

	for _, server := range []ManagedServer{
		{Name: "stdio", Transport: mcp.TransportStdio, Command: "cmd", Args: []string{"--api-key=${MCP_KEY}"}},
		{Name: "sse", Transport: mcp.TransportSSE, URL: "https://example.test?token=${MCP_TOKEN}"},
	} {
		if err := ValidateManagedServer(server); err != nil {
			t.Errorf("reference-based server rejected: %v", err)
		}
	}
}

func TestListManagedRejectsInvalidKnownFieldShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".mcp.json")
	if err := os.WriteFile(path, []byte(`{"mcpServers":{"bad":{"command":42}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ListManaged(path); err == nil || !strings.Contains(err.Error(), "invalid field shape") {
		t.Fatalf("ListManaged error = %v", err)
	}
}

func TestUserConfigIsPrivate(t *testing.T) {
	home := t.TempDir()
	path, err := CanonicalPath("unused", home, ScopeUser)
	if err != nil {
		t.Fatal(err)
	}
	if err := AddManaged(path, ManagedServer{Name: "x", Transport: mcp.TransportStdio, Command: "cmd"}, true); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("user config mode = %o, want 600", got)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got&0o077 != 0 {
		t.Fatalf("user config directory mode = %o, want no group/other access", got)
	}
}

func TestRedactedEndpoint(t *testing.T) {
	stdio := ManagedServer{Name: "x", Transport: mcp.TransportStdio, Command: "cmd", Args: []string{"--token", "very-secret", "--safe", "visible", "--api-key=also-secret"}}
	got := RedactedEndpoint(stdio)
	if strings.Contains(got, "very-secret") || strings.Contains(got, "also-secret") || !strings.Contains(got, "visible") {
		t.Fatalf("stdio redaction = %q", got)
	}
	sse := ManagedServer{Name: "x", Transport: mcp.TransportSSE, URL: "https://example.test/mcp?token=very-secret&region=west"}
	got = RedactedEndpoint(sse)
	if strings.Contains(got, "very-secret") || !strings.Contains(got, "region=west") || !strings.Contains(got, "%3Credacted%3E") {
		t.Fatalf("SSE redaction = %q", got)
	}
}

func TestStreamableHTTPValidation(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		hosts   []string
		headers map[string]string
		wantErr bool
	}{
		{"https any host", "https://mcp.example.com/mcp", nil, nil, false},
		{"loopback http", "http://127.0.0.1:8080/mcp", nil, nil, false},
		{"cluster http", "http://state.ns.svc.cluster.local/mcp", nil, nil, false},
		{"other http refused", "http://mcp.example.com/mcp", nil, nil, true},
		{"other http allowed by policy", "http://mcp.example.com/mcp", []string{"mcp.example.com"}, nil, false},
		{"userinfo refused", "https://u:p@mcp.example.com/mcp", nil, nil, true},
		{"bearer env reference", "https://mcp.example.com/mcp", nil, map[string]string{"Authorization": "Bearer ${TOKEN}"}, false},
		{"token env reference", "https://mcp.example.com/mcp", nil, map[string]string{"Authorization": "${TOKEN}"}, false},
		{"literal credential refused", "https://mcp.example.com/mcp", nil, map[string]string{"Authorization": "Bearer abc"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateManagedServerWithHosts(ManagedServer{Name: "s", Transport: mcp.TransportStreamableHTTP, URL: tc.url, Headers: tc.headers}, tc.hosts)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
		})
	}
}
