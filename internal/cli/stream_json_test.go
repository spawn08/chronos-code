package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/sdk/agent"
)

// fakeModel serves an OpenAI-compatible endpoint. toolRounds model turns
// request file_read; the next turn answers with text. A negative toolRounds
// means every turn requests a tool.
func fakeModel(t *testing.T, toolRounds int, failWith int) *httptest.Server {
	t.Helper()
	return fakeToolModel(t, toolRounds, failWith, "file_read", `{"path":"note.txt"}`)
}

// fakeToolModel is fakeModel with a chosen tool name and JSON arguments.
func fakeToolModel(t *testing.T, toolRounds int, failWith int, toolName, toolArgs string) *httptest.Server {
	t.Helper()
	quotedArgs, _ := json.Marshal(toolArgs)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(calls.Add(1))
		if failWith != 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(failWith)
			fmt.Fprint(w, `{"error":{"message":"provider exploded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if toolRounds < 0 || n <= toolRounds {
			fmt.Fprintf(w, "data: {\"id\":\"r%d\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_%d\",\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":\"tool_calls\"}]}\n\n", n, n, toolName, quotedArgs)
			fmt.Fprintf(w, "data: {\"id\":\"r%d\",\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":2}}\n\n", n)
		} else {
			fmt.Fprintf(w, "data: {\"id\":\"r%d\",\"choices\":[{\"delta\":{\"content\":\"all \"}}]}\n\n", n)
			fmt.Fprintf(w, "data: {\"id\":\"r%d\",\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n", n)
			fmt.Fprintf(w, "data: {\"id\":\"r%d\",\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":4}}\n\n", n)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server
}

func newStreamTestOrchestrator(t *testing.T, baseURL string) *orchestrator.Orchestrator {
	t.Helper()
	return newStreamTestOrchestratorWith(t, baseURL, nil)
}

// newStreamTestOrchestratorWith lets adjust change the config, and the
// workspace at cfg.Workspace.Root, before the orchestrator is built.
func newStreamTestOrchestratorWith(t *testing.T, baseURL string, adjust func(*config.Config)) *orchestrator.Orchestrator {
	t.Helper()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	indexOnStart := false
	cfg := &config.Config{
		FileConfig: agent.FileConfig{
			Defaults: &agent.AgentConfig{Storage: agent.StorageConfig{Backend: "sqlite", DSN: filepath.Join(t.TempDir(), "sessions.db")}},
			Agents: []agent.AgentConfig{{
				ID: "coder", Name: "Coder", System: "You are a test agent.",
				Tools: []agent.ToolConfig{{Name: "file_read", Permission: "allow"}, {Name: "shell", Permission: "require_approval"}},
				Model: agent.ModelConfig{Provider: "openai", Model: "test-model", APIKey: "test", BaseURL: baseURL},
			}},
		},
		Workspace:    config.WorkspaceConfig{Root: workspace, IndexOnStart: &indexOnStart},
		Verification: config.VerificationConfig{Mode: verification.ModeReport},
	}
	if adjust != nil {
		adjust(cfg)
	}
	orch, err := orchestrator.New(t.Context(), cfg, "")
	if err != nil {
		t.Fatalf("orchestrator.New: %v", err)
	}
	t.Cleanup(func() { _ = orch.Close() })
	return orch
}

func decodeStream(t *testing.T, out []byte) []execution.EventEnvelope {
	t.Helper()
	var events []execution.EventEnvelope
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var event execution.EventEnvelope
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line is not JSON: %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

func eventTypes(events []execution.EventEnvelope) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = string(e.Type)
	}
	return types
}

func TestRunStreamJSONToolCallGolden(t *testing.T) {
	orch := newStreamTestOrchestrator(t, fakeModel(t, 1, 0).URL)
	var out bytes.Buffer
	err := runStreamJSON(context.Background(), orch, orchestrator.ExecutionRequest{Message: "read the note"}, newStreamEmitter(&out), "/work", nil)
	if err != nil {
		t.Fatalf("runStreamJSON: %v\n%s", err, out.String())
	}
	events := decodeStream(t, out.Bytes())
	if err := execution.ValidateEventOrder(events); err != nil {
		t.Fatalf("event order: %v\n%s", err, out.String())
	}
	if events[0].Type != execution.EventSession {
		t.Fatalf("first event = %s, want session", events[0].Type)
	}
	var session execution.SessionPayload
	if err := json.Unmarshal(events[0].Payload, &session); err != nil || session.SessionID == "" || session.Model == "" || session.Cwd != "/work" {
		t.Fatalf("session payload = %+v (%v)", session, err)
	}
	types := eventTypes(events)
	index := func(want string) int {
		for i, typ := range types {
			if typ == want {
				return i
			}
		}
		return -1
	}
	tool, result, content := index("tool"), index("tool_result"), index("content")
	if tool < 0 || result < tool || content < result {
		t.Fatalf("want tool < tool_result < content, got %v", types)
	}
	var payload execution.ToolPayload
	if err := json.Unmarshal(events[tool].Payload, &payload); err != nil || payload.Name != "file_read" || payload.ID == "" {
		t.Fatalf("tool payload = %+v (%v)", payload, err)
	}
	var input map[string]any
	if err := json.Unmarshal(payload.Input, &input); err != nil || input["path"] != "note.txt" {
		t.Fatalf("tool input = %s (%v)", payload.Input, err)
	}
	last := events[len(events)-1]
	if last.Type != execution.EventCompletion {
		t.Fatalf("last event = %s, want completion", last.Type)
	}
	var envelope execution.ExecutionEnvelope
	if err := json.Unmarshal(last.Payload, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Status != execution.StatusSucceeded || !strings.Contains(envelope.Content, "all done") || envelope.SessionID != session.SessionID {
		t.Fatalf("envelope = %+v", envelope)
	}
	var total int
	for _, usage := range envelope.UsageByModel {
		total += usage.PromptTokens
	}
	if total == 0 {
		t.Fatalf("usage_by_model empty: %+v", envelope.UsageByModel)
	}
}

func TestRunStreamJSONProviderFailureGolden(t *testing.T) {
	orch := newStreamTestOrchestrator(t, fakeModel(t, 0, http.StatusBadRequest).URL)
	var out bytes.Buffer
	err := runStreamJSON(context.Background(), orch, orchestrator.ExecutionRequest{Message: "hello"}, newStreamEmitter(&out), "/work", nil)
	var exit *ExitError
	if !asExitError(err, &exit) || exit.Code == ExitSuccess {
		t.Fatalf("err = %v, want a non-zero ExitError", err)
	}
	events := decodeStream(t, out.Bytes())
	if err := execution.ValidateEventOrder(events); err != nil {
		t.Fatalf("event order: %v\n%s", err, out.String())
	}
	if events[0].Type != execution.EventSession {
		t.Fatalf("first event = %s", events[0].Type)
	}
	last := events[len(events)-1]
	if last.Type != execution.EventError {
		t.Fatalf("last event = %s, want error", last.Type)
	}
	var payload errorPayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil || payload.Code == "" || payload.Status == execution.StatusSucceeded {
		t.Fatalf("error payload = %+v (%v)", payload, err)
	}
	if exit.Code != ExitCodeForStatus(payload.Status) {
		t.Fatalf("exit code %d does not match status %s", exit.Code, payload.Status)
	}
}

func TestRunStreamJSONMaxTurns(t *testing.T) {
	orch := newStreamTestOrchestrator(t, fakeModel(t, -1, 0).URL)
	var out bytes.Buffer
	err := runStreamJSON(context.Background(), orch, orchestrator.ExecutionRequest{Message: "loop", MaxTurns: 1}, newStreamEmitter(&out), "/work", nil)
	var exit *ExitError
	if !asExitError(err, &exit) || exit.Code != ExitFailure {
		t.Fatalf("err = %v, want ExitFailure", err)
	}
	events := decodeStream(t, out.Bytes())
	if err := execution.ValidateEventOrder(events); err != nil {
		t.Fatalf("event order: %v", err)
	}
	var toolEvents int
	for _, e := range events {
		if e.Type == execution.EventTool {
			toolEvents++
		}
	}
	if toolEvents != 1 {
		t.Fatalf("tool events = %d, want 1 (types %v)", toolEvents, eventTypes(events))
	}
	var payload errorPayload
	if err := json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil || payload.StopReason != execution.StopMaxTurns || payload.Code != execution.ErrorMaxTurns {
		t.Fatalf("terminal payload = %+v (%v)", payload, err)
	}
}

func TestStreamEmitterSingleTerminal(t *testing.T) {
	var out bytes.Buffer
	e := newStreamEmitter(&out)
	e.emit(execution.EventContent, execution.ContentPayload{Content: "a"})
	if !e.terminate(execution.EventCompletion, map[string]string{}) {
		t.Fatal("first terminate should write")
	}
	if e.terminate(execution.EventError, map[string]string{}) {
		t.Fatal("second terminate must be dropped")
	}
	e.emit(execution.EventContent, execution.ContentPayload{Content: "late"})
	if got := len(decodeStream(t, out.Bytes())); got != 2 {
		t.Fatalf("events = %d, want 2", got)
	}
}

func TestParseRunFlagsPolicyFiles(t *testing.T) {
	opts, _, err := parseRunFlags([]string{"--policy-file", "a.yaml", "--policy-file=b.yaml", "go"})
	if err != nil || strings.Join(opts.policyFiles, ",") != "a.yaml,b.yaml" {
		t.Fatalf("policyFiles = %v (%v)", opts.policyFiles, err)
	}
}

func TestParseRunFlagsSkillSources(t *testing.T) {
	opts, _, err := parseRunFlags([]string{"--skill-sources", "project", "go"})
	if err != nil || opts.skillSources != "project" {
		t.Fatalf("skillSources = %q (%v)", opts.skillSources, err)
	}
	if _, _, err := parseRunFlags([]string{"--skill-sources=repo", "go"}); err == nil {
		t.Fatal("invalid --skill-sources accepted")
	}
}

func TestParseRunFlagsAndPrompt(t *testing.T) {
	opts, words, err := parseRunFlags([]string{"--output-format", "stream-json", "--max-turns=5", "--mcp-config", "a.json", "--mcp-config=b.json", "--strict-mcp-config", "--prompt-stdin", "--thinking", "high", "--ephemeral", "fix", "--", "--json-ish"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.streamJSON() || opts.maxTurns != 5 || len(opts.mcpConfigs) != 2 || !opts.strictMCPConfig || !opts.promptStdin || opts.thinking != "high" || !opts.ephemeral {
		t.Fatalf("opts = %+v", opts)
	}
	if strings.Join(words, " ") != "fix --json-ish" {
		t.Fatalf("words = %v", words)
	}
	for _, bad := range [][]string{{"--max-turns", "0"}, {"--max-turns", "x"}, {"--output-format", "xml"}, {"--system-prompt"}, {"--bogus-flag", "hi"}} {
		if _, _, err := parseRunFlags(bad); err == nil {
			t.Errorf("parseRunFlags(%v) succeeded", bad)
		}
	}
	if opts, _, err := parseRunFlags([]string{"--output-format", "stream-json", "--bogus-flag"}); err == nil || !opts.streamJSON() {
		t.Fatalf("unknown flag: opts=%+v err=%v; want error that keeps stream-json", opts, err)
	}
	if opts, _, err := parseRunFlags([]string{"--help"}); err != nil || !opts.help {
		t.Fatalf("--help: opts=%+v err=%v", opts, err)
	}
	big := strings.Repeat("x", 500<<10)
	got, err := resolvePrompt(runOptions{promptStdin: true}, nil, strings.NewReader(big+"\n"))
	if err != nil || got != big {
		t.Fatalf("500 KB stdin prompt: len=%d err=%v", len(got), err)
	}
	if _, err := resolvePrompt(runOptions{promptStdin: true}, nil, strings.NewReader("  \n")); err == nil {
		t.Fatal("empty stdin prompt accepted")
	}
	if got, _ := resolvePrompt(runOptions{}, []string{"-"}, strings.NewReader("from stdin")); got != "from stdin" {
		t.Fatalf("'-' prompt = %q", got)
	}
}

func asExitError(err error, target **ExitError) bool {
	e, ok := err.(*ExitError)
	if ok {
		*target = e
	}
	return ok
}

// syncBuffer lets the test read events while the run is still writing.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf.Bytes()...)
}

func TestRunStreamJSONCancelDuringToolCall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process groups are Unix-only")
	}
	marker := filepath.Join(t.TempDir(), "child.pid")
	command := fmt.Sprintf("sleep 300 & echo $! > %s; wait", marker)
	args, _ := json.Marshal(map[string]string{"command": command})
	orch := newStreamTestOrchestrator(t, fakeToolModel(t, -1, 0, "shell", string(args)).URL)
	orch.SetSkipPermissions(true)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- runStreamJSON(ctx, orch, orchestrator.ExecutionRequest{Message: "sleep"}, newStreamEmitter(out), "/work", nil)
	}()

	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(string(out.Bytes()), `"type":"tool"`) || !fileExists(marker) {
		if time.Now().After(deadline) {
			t.Fatalf("tool never started; output:\n%s", out.Bytes())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("run did not stop within 10s of cancellation; output:\n%s", out.Bytes())
	}
	var exit *ExitError
	if !asExitError(err, &exit) || exit.Code != ExitCancelled {
		t.Fatalf("err = %v, want exit code %d", err, ExitCancelled)
	}
	events := decodeStream(t, out.Bytes())
	if err := execution.ValidateEventOrder(events); err != nil {
		t.Fatalf("event order: %v", err)
	}
	var payload errorPayload
	if err := json.Unmarshal(events[len(events)-1].Payload, &payload); err != nil || payload.Status != execution.StatusCancelled {
		t.Fatalf("terminal = %s %+v (%v)", events[len(events)-1].Type, payload, err)
	}
	raw, _ := os.ReadFile(marker)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if pid == 0 {
		t.Fatalf("no child pid recorded: %q", raw)
	}
	for i := 0; i < 100 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("child process %d survived cancellation", pid)
	}
}

func fileExists(path string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(data)) != ""
}

func TestRunStreamJSONOverBudgetProjectDocs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var workspace string
	orch := newStreamTestOrchestratorWith(t, fakeModel(t, 0, 0).URL, func(cfg *config.Config) {
		workspace = cfg.Workspace.Root
		if err := os.WriteFile(filepath.Join(workspace, "AGENTS.md"), []byte(strings.Repeat("rule ", 20000)), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg.ProjectDocsBudget = 2000
	})
	var out bytes.Buffer
	if err := runStreamJSON(context.Background(), orch, orchestrator.ExecutionRequest{Message: "hello"}, newStreamEmitter(&out), "/work", nil); err != nil {
		t.Fatalf("run: %v\n%s", err, out.String())
	}
	var warning execution.WarningPayload
	for _, event := range decodeStream(t, out.Bytes()) {
		if event.Type == execution.EventWarning {
			if err := json.Unmarshal(event.Payload, &warning); err != nil {
				t.Fatal(err)
			}
		}
	}
	if warning.Code != "project_docs_truncated" || !strings.Contains(warning.Message, "2000-token budget") {
		t.Fatalf("warning = %+v\n%s", warning, out.String())
	}
	entries, err := os.ReadDir(workspace)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if name := entry.Name(); name != "note.txt" && name != "AGENTS.md" {
			t.Errorf("run wrote %s under the workspace", name)
		}
	}
}
