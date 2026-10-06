package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

// unreachableMCPConfig writes a --mcp-config file whose only server listens
// on a closed loopback port. The URL carries a credential to prove it never
// reaches the stream.
func unreachableMCPConfig(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	path := filepath.Join(t.TempDir(), "mcp.json")
	body := fmt.Sprintf(`{"mcpServers":{"state":{"type":"http","url":"http://%s/mcp?token=s3cret","headers":{"Authorization":"Bearer s3cret"}}}}`, addr)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func withCallerMCP(path string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.MCP.CallerConfigs, cfg.MCP.Strict = []string{path}, true
	}
}

func mcpStatusEvent(t *testing.T, events []execution.EventEnvelope) execution.MCPStatusPayload {
	t.Helper()
	for _, event := range events {
		if event.Type == execution.EventMCPStatus {
			var payload execution.MCPStatusPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			return payload
		}
	}
	t.Fatalf("no mcp_status event in %v", eventTypes(events))
	return execution.MCPStatusPayload{}
}

func TestRunStreamJSONRequiredMCPUnavailable(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("provider called before the required MCP check")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(provider.Close)
	orch := newStreamTestOrchestratorWith(t, provider.URL, withCallerMCP(unreachableMCPConfig(t)))
	var out bytes.Buffer
	err := runStreamJSON(context.Background(), orch, orchestrator.ExecutionRequest{Message: "hello"}, newStreamEmitter(&out), "/work", []string{"state", "absent"})
	var exit *ExitError
	if !asExitError(err, &exit) || exit.Code != ExitMCPUnavailable {
		t.Fatalf("err = %v, want exit %d", err, ExitMCPUnavailable)
	}
	if strings.Contains(out.String(), "s3cret") {
		t.Fatalf("stream leaks a credential:\n%s", out.String())
	}
	events := decodeStream(t, out.Bytes())
	if err := execution.ValidateEventOrder(events); err != nil {
		t.Fatalf("event order: %v\n%s", err, out.String())
	}
	if got := strings.Join(eventTypes(events), ","); got != "session,mcp_status,error" {
		t.Fatalf("events = %s", got)
	}
	status := mcpStatusEvent(t, events)
	want := []execution.MCPServerStatus{
		{Name: "absent", State: mcpStateNotConfigured, Error: mcpStateMessages[mcpStateNotConfigured]},
		{Name: "state", State: "connection_failed", Error: "could not connect"},
	}
	if fmt.Sprint(status.Servers) != fmt.Sprint(want) {
		t.Fatalf("servers = %+v, want %+v", status.Servers, want)
	}
	var payload errorPayload
	if err := json.Unmarshal(events[2].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != execution.ErrorMCPUnavailable || payload.StopReason != execution.StopMCPUnavailable || payload.SessionID == "" {
		t.Fatalf("error payload = %+v", payload)
	}
}

func TestRunStreamJSONOptionalMCPFailureContinues(t *testing.T) {
	orch := newStreamTestOrchestratorWith(t, fakeModel(t, 0, 0).URL, withCallerMCP(unreachableMCPConfig(t)))
	var out bytes.Buffer
	if err := runStreamJSON(context.Background(), orch, orchestrator.ExecutionRequest{Message: "hello"}, newStreamEmitter(&out), "/work", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	events := decodeStream(t, out.Bytes())
	if events[1].Type != execution.EventMCPStatus || events[len(events)-1].Type != execution.EventCompletion {
		t.Fatalf("events = %v", eventTypes(events))
	}
	status := mcpStatusEvent(t, events)
	if len(status.Servers) != 1 || status.Servers[0].State != "connection_failed" {
		t.Fatalf("servers = %+v", status.Servers)
	}
}

func TestRunStreamJSONNoMCPOmitsStatus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	orch := newStreamTestOrchestrator(t, fakeModel(t, 0, 0).URL)
	var out bytes.Buffer
	if err := runStreamJSON(context.Background(), orch, orchestrator.ExecutionRequest{Message: "hello"}, newStreamEmitter(&out), "/work", nil); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, event := range decodeStream(t, out.Bytes()) {
		if event.Type == execution.EventMCPStatus {
			t.Fatalf("mcp_status written with no MCP servers")
		}
	}
}

func TestParseRunFlagsRequireMCP(t *testing.T) {
	opts, _, err := parseRunFlags([]string{"--require-mcp", "a, b", "--require-mcp=c", "go"})
	if err != nil || strings.Join(opts.requiredMCP, ",") != "a,b,c" {
		t.Fatalf("requiredMCP = %v (%v)", opts.requiredMCP, err)
	}
	if _, _, err := parseRunFlags([]string{"--require-mcp", ",", "go"}); err == nil {
		t.Fatal("empty --require-mcp accepted")
	}
}
