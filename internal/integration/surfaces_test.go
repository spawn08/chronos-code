package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/cli"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/mcpdiscover"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/server"
	"github.com/spawn08/chronos-code/internal/tui"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
)

const surfaceSessionID = "surface-session"

type surfaceRecord struct {
	Route             string
	Task              string
	Session           string
	PromptTokens      int
	CompletionTokens  int
	SpentMicrodollars budget.Microdollars
	Verification      verification.Status
}

func TestSurfacesExecuteSeededBugfix(t *testing.T) {
	fixture := loadVerifiedBugfixFixture(t)
	var records []surfaceRecord

	t.Run("CLI", func(t *testing.T) {
		workspace, grader := stageBugfix(t, fixture)
		assertGrader(t, fixture, grader, false)
		orch := newSurfaceOrchestrator(t, workspace, newModelServer(t, workspace, grader, fixture, modelSuccess).URL)

		var stdout, stderr bytes.Buffer
		result, err := cli.RunExecution(t.Context(), orch, verifiedRequest(fixture, orchestrator.ExecutionBlocking), &stdout, &stderr)
		if err != nil {
			t.Fatalf("CLI execution: %v", err)
		}
		if !strings.Contains(stdout.String(), "patched by coder") {
			t.Fatalf("CLI output = %q, want successful model response", stdout.String())
		}
		assertGrader(t, fixture, grader, true)
		records = append(records, normalizeResult(t, orch, result, result.Response.Usage))
	})

	t.Run("TUI", func(t *testing.T) {
		workspace, grader := stageBugfix(t, fixture)
		assertGrader(t, fixture, grader, false)
		orch := newSurfaceOrchestrator(t, workspace, newModelServer(t, workspace, grader, fixture, modelSuccess).URL)

		result, err := tui.StartExecution(t.Context(), orch, verifiedRequest(fixture, orchestrator.ExecutionStreaming))
		if err != nil {
			t.Fatalf("TUI execution start: %v", err)
		}
		usage, content, err := consumeStream(result.Stream)
		if err != nil {
			t.Fatalf("TUI stream: %v", err)
		}
		if !strings.Contains(content, "patched by coder") {
			t.Fatalf("TUI output = %q, want successful model response", content)
		}
		assertGrader(t, fixture, grader, true)
		records = append(records, normalizeResult(t, orch, result, usage))
	})

	t.Run("HTTP", func(t *testing.T) {
		workspace, grader := stageBugfix(t, fixture)
		assertGrader(t, fixture, grader, false)
		orch := newSurfaceOrchestrator(t, workspace, newModelServer(t, workspace, grader, fixture, modelSuccess).URL)
		result, err := server.ExecuteRequest(t.Context(), orch, verifiedRequest(fixture, orchestrator.ExecutionBlocking))
		if err != nil {
			t.Fatalf("HTTP execution: %v", err)
		}
		if result.Response.Content != "patched by coder" {
			t.Fatalf("HTTP content = %q, want successful model response", result.Response.Content)
		}
		assertGrader(t, fixture, grader, true)
		records = append(records, normalizeResult(t, orch, result, result.Response.Usage))
	})

	if len(records) != 3 {
		t.Fatalf("surface records = %d, want 3", len(records))
	}
	want := surfaceRecord{
		Route: "coder", Task: "assigned", Session: "assigned",
		PromptTokens: 7, CompletionTokens: 3, SpentMicrodollars: 22,
		Verification: verification.StatusSatisfied,
	}
	if records[0] != want {
		t.Fatalf("normalized surface record = %#v, want %#v", records[0], want)
	}
	for i := 1; i < len(records); i++ {
		if records[i] != records[0] {
			t.Fatalf("normalized surface record %d = %#v, want %#v", i, records[i], records[0])
		}
	}
}

func TestSurfacesRejectUnsupportedCompletion(t *testing.T) {
	fixture := loadVerifiedBugfixFixture(t)
	for _, surface := range []string{"CLI", "TUI", "HTTP"} {
		t.Run(surface, func(t *testing.T) {
			workspace, grader := stageBugfix(t, fixture)
			orch := newSurfaceOrchestrator(t, workspace, newModelServer(t, workspace, grader, fixture, modelSuccess).URL)
			request := verifiedRequest(fixture, orchestrator.ExecutionBlocking)
			request.VerificationEvents = request.VerificationEvents[:1]

			var err error
			switch surface {
			case "CLI":
				var output bytes.Buffer
				_, err = cli.RunExecution(t.Context(), orch, request, &output, io.Discard)
				if output.Len() != 0 {
					t.Fatalf("CLI rendered success after rejection: %q", output.String())
				}
			case "TUI":
				_, err = tui.StartExecution(t.Context(), orch, request)
			case "HTTP":
				_, err = server.ExecuteRequest(t.Context(), orch, request)
			}
			if err == nil || !strings.Contains(err.Error(), "verification does not support") {
				t.Fatalf("%s error = %v, want unsupported completion", surface, err)
			}
		})
	}

	t.Run("HTTP SSE error has no success terminator", func(t *testing.T) {
		workspace, grader := stageBugfix(t, fixture)
		orch := newSurfaceOrchestrator(t, workspace, newModelServer(t, workspace, grader, fixture, modelSuccess).URL)
		request := verifiedRequest(fixture, orchestrator.ExecutionStreaming)
		request.VerificationEvents = request.VerificationEvents[:1]
		result, err := server.ExecuteRequest(t.Context(), orch, request)
		if err != nil {
			t.Fatalf("start HTTP stream: %v", err)
		}
		response := httptest.NewRecorder()
		server.WriteEventStream(t.Context(), response, response, result.Stream, result.SessionID)
		body := response.Body.String()
		if !strings.Contains(body, "event: error\n") || !strings.Contains(body, "verification does not support") || strings.Contains(body, "[DONE]") {
			t.Fatalf("unsupported SSE body = %q, want terminal error without DONE", body)
		}
	})
}

func TestSurfacesTrustFailuresFailClosed(t *testing.T) {
	fixture := loadVerifiedBugfixFixture(t)
	for _, surface := range []string{"CLI", "TUI", "HTTP"} {
		t.Run(surface+" trust failure", func(t *testing.T) {
			workspace, grader := stageBugfix(t, fixture)
			orch := newSurfaceOrchestrator(t, workspace, newModelServer(t, workspace, grader, fixture, modelTrustFailure).URL)
			request := verifiedRequest(fixture, orchestrator.ExecutionBlocking)
			var err error
			switch surface {
			case "CLI":
				var output bytes.Buffer
				_, err = cli.RunExecution(t.Context(), orch, request, &output, io.Discard)
				if output.Len() != 0 {
					t.Fatalf("CLI rendered success after trust failure: %q", output.String())
				}
			case "TUI":
				_, err = tui.StartExecution(t.Context(), orch, request)
			case "HTTP":
				handler := server.New(orch, server.ServerConfig{AuthType: "none"}).Handler()
				req := httptest.NewRequest(http.MethodPost, "/v1/chat", strings.NewReader(`{"message":"fix bug","session_id":"surface-session"}`))
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, req)
				if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "patched by coder") {
					t.Fatalf("HTTP trust failure reported success: status=%d body=%q", response.Code, response.Body.String())
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "Access denied") || strings.Contains(err.Error(), "untrusted model endpoint") {
				t.Fatalf("%s trust error = %v, want sanitized access denial", surface, err)
			}
		})
	}

	t.Run("SSE cancellation has no success terminator", func(t *testing.T) {
		workspace, grader := stageBugfix(t, fixture)
		orch := newSurfaceOrchestrator(t, workspace, newModelServer(t, workspace, grader, fixture, modelWaitForCancellation).URL)
		ctx, cancel := context.WithCancel(context.Background())
		result, err := server.ExecuteRequest(ctx, orch, verifiedRequest(fixture, orchestrator.ExecutionStreaming))
		if err != nil {
			t.Fatalf("start cancellable HTTP stream: %v", err)
		}
		cancel()
		response := httptest.NewRecorder()
		server.WriteEventStream(ctx, response, response, result.Stream, result.SessionID)
		body := response.Body.String()
		if !strings.Contains(body, "event: error\n") || !strings.Contains(body, context.Canceled.Error()) || strings.Contains(body, "[DONE]") {
			t.Fatalf("cancelled SSE body = %q, want cancellation error without DONE", body)
		}
	})

	assertLocalTrustBoundaries(t)
}

type verifiedBugfixFixture struct {
	TaskID       string   `json:"task_id"`
	Message      string   `json:"message"`
	TaskKind     string   `json:"task_kind"`
	ChangedPaths []string `json:"changed_paths"`
	TestCommand  string   `json:"test_command"`
	SeedDir      string   `json:"seed_dir"`
	SolutionPath string   `json:"solution_path"`
	GraderDir    string   `json:"grader_dir"`
}

func loadVerifiedBugfixFixture(t *testing.T) verifiedBugfixFixture {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join("testdata", "verified_bugfix.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture verifiedBugfixFixture
	if err := json.Unmarshal(contents, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.TaskID == "" || fixture.Message == "" || fixture.TaskKind != "debug" || len(fixture.ChangedPaths) == 0 || fixture.TestCommand == "" || fixture.SeedDir == "" || fixture.SolutionPath == "" || fixture.GraderDir == "" {
		t.Fatalf("invalid verified bugfix fixture: %#v", fixture)
	}
	return fixture
}

func verifiedRequest(fixture verifiedBugfixFixture, mode orchestrator.ExecutionMode) orchestrator.ExecutionRequest {
	obligations := verification.Derive(verification.Input{
		TaskKind: fixture.TaskKind, ChangedPaths: fixture.ChangedPaths, TestCommands: []string{fixture.TestCommand},
	})
	return orchestrator.ExecutionRequest{
		Message: fixture.Message, Mode: mode, SessionID: surfaceSessionID, TaskID: fixture.TaskID,
		VerificationMode: verification.ModeEnforce, VerificationObligations: obligations,
		VerificationEvents: []execution.Event{
			{ID: "write", TaskID: execution.TaskID(fixture.TaskID), Sequence: 1, Type: execution.EventWrite, Paths: fixture.ChangedPaths},
			{ID: "verify", TaskID: execution.TaskID(fixture.TaskID), Sequence: 2, Type: execution.EventVerification, EvidenceID: "hidden-grader", Paths: fixture.ChangedPaths, Detail: fixture.TestCommand, Passed: true},
		},
	}
}

func normalizeResult(t *testing.T, orch *orchestrator.Orchestrator, result orchestrator.ExecutionResult, usage model.Usage) surfaceRecord {
	t.Helper()
	assertAdapterUsesExecute(t, result)
	cost := orch.SessionCost()
	return surfaceRecord{
		Route: result.AgentID, Task: normalizeIdentity(result.TaskID), Session: normalizeIdentity(result.SessionID),
		PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		SpentMicrodollars: cost.SpentMicrodollars, Verification: verification.StatusSatisfied,
	}
}

func assertAdapterUsesExecute(t *testing.T, result orchestrator.ExecutionResult) {
	t.Helper()
	if result.AgentID == "" || result.TaskID == "" || result.SessionID == "" {
		t.Fatalf("adapter did not return common Execute identity: %#v", result)
	}
	wantKinds := []orchestrator.ContextSourceKind{
		orchestrator.ContextSourceSessionSummaries, orchestrator.ContextSourceMemory,
		orchestrator.ContextSourceLearnedPattern, orchestrator.ContextSourceProjectDocs,
		orchestrator.ContextSourceSkills, orchestrator.ContextSourceDiagnostics,
		orchestrator.ContextSourceGraphPrediction, orchestrator.ContextSourceUserHook,
	}
	if len(result.ContextReport.Sources) != len(wantKinds) {
		t.Fatalf("context report sources = %d, want %d", len(result.ContextReport.Sources), len(wantKinds))
	}
	for i, kind := range wantKinds {
		if result.ContextReport.Sources[i].Kind != kind {
			t.Fatalf("context report source %d = %q, want %q", i, result.ContextReport.Sources[i].Kind, kind)
		}
	}
}

func normalizeIdentity(value string) string {
	if value == "" {
		return ""
	}
	return "assigned"
}

func consumeStream(stream <-chan *model.ChatResponse) (model.Usage, string, error) {
	var usage model.Usage
	var content strings.Builder
	for response := range stream {
		if response.Err != nil {
			return usage, content.String(), response.Err
		}
		content.WriteString(response.Content)
		if response.Usage.PromptTokens != 0 {
			usage.PromptTokens = response.Usage.PromptTokens
		}
		if response.Usage.CompletionTokens != 0 {
			usage.CompletionTokens = response.Usage.CompletionTokens
		}
	}
	return usage, content.String(), nil
}

type modelServerMode uint8

const (
	modelSuccess modelServerMode = iota
	modelTrustFailure
	modelWaitForCancellation
)

func newModelServer(t *testing.T, workspace, grader string, fixture verifiedBugfixFixture, mode modelServerMode) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode == modelTrustFailure {
			writeModelError(w, http.StatusForbidden, "untrusted model endpoint")
			return
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			writeModelError(w, http.StatusBadRequest, err.Error())
			return
		}
		if mode == modelWaitForCancellation {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}

		mu.Lock()
		solution, err := os.ReadFile(filepath.Join("testdata", fixture.SolutionPath))
		if err == nil {
			err = os.WriteFile(filepath.Join(workspace, fixture.ChangedPaths[0]), solution, 0o600)
		}
		if err == nil {
			_, err = runGrader(r.Context(), fixture, grader)
		}
		mu.Unlock()
		if err != nil {
			writeModelError(w, http.StatusInternalServerError, "deterministic patch failed hidden grading: "+err.Error())
			return
		}

		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"id":"surface","choices":[{"delta":{"content":"patched by coder"},"finish_reason":"stop"}]}`)
			fmt.Fprintln(w, `data: {"id":"surface","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
			fmt.Fprintln(w, "data: [DONE]")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"surface","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"patched by coder"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	})
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server
}

func writeModelError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message}})
}

func newSurfaceOrchestrator(t *testing.T, workspace, baseURL string) *orchestrator.Orchestrator {
	t.Helper()
	indexOnStart := false
	cfg := &config.Config{
		FileConfig: agent.FileConfig{
			Defaults: &agent.AgentConfig{Storage: agent.StorageConfig{Backend: "sqlite", DSN: filepath.Join(t.TempDir(), "sessions.db")}},
			Agents: []agent.AgentConfig{{
				ID: "coder", Name: "Coder", System: "Apply the requested deterministic bug fix.",
				Model: agent.ModelConfig{Provider: "openai", Model: "claude-haiku-4-5", APIKey: "test", BaseURL: baseURL},
			}},
		},
		Workspace:    config.WorkspaceConfig{Root: workspace, IndexOnStart: &indexOnStart},
		Verification: config.VerificationConfig{Mode: verification.ModeEnforce},
	}
	orch, err := orchestrator.New(t.Context(), cfg, surfaceSessionID)
	if err != nil {
		t.Fatalf("orchestrator.New(): %v", err)
	}
	t.Cleanup(func() {
		if err := orch.Close(); err != nil {
			t.Errorf("orchestrator.Close(): %v", err)
		}
	})
	return orch
}

func stageBugfix(t *testing.T, fixture verifiedBugfixFixture) (string, string) {
	t.Helper()
	workspace := filepath.Join(t.TempDir(), "acting-workspace")
	grader := filepath.Join(t.TempDir(), "hidden-grader")
	copyTree(t, filepath.Join("testdata", fixture.SeedDir), workspace)
	copyTree(t, filepath.Join("testdata", fixture.GraderDir), grader)
	goMod := filepath.Join(grader, "go.mod")
	contents, err := os.ReadFile(goMod)
	if err != nil {
		t.Fatal(err)
	}
	contents = bytes.ReplaceAll(contents, []byte("WORKSPACE"), []byte(filepath.ToSlash(workspace)))
	if err := os.WriteFile(goMod, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(grader, workspace+string(filepath.Separator)) {
		t.Fatalf("hidden grader %q is inside acting workspace %q", grader, workspace)
	}
	return workspace, grader
}

func copyTree(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.MkdirAll(destination, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		sourcePath := filepath.Join(source, entry.Name())
		destinationPath := filepath.Join(destination, entry.Name())
		if entry.IsDir() {
			copyTree(t, sourcePath, destinationPath)
			continue
		}
		contents, err := os.ReadFile(sourcePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(destinationPath, contents, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertGrader(t *testing.T, fixture verifiedBugfixFixture, grader string, wantPass bool) {
	t.Helper()
	output, err := runGrader(t.Context(), fixture, grader)
	if wantPass && err != nil {
		t.Fatalf("hidden grader failed after patch: %v\n%s", err, output)
	}
	if !wantPass && err == nil {
		t.Fatalf("hidden grader passed seeded bug before patch:\n%s", output)
	}
}

func runGrader(ctx context.Context, fixture verifiedBugfixFixture, grader string) ([]byte, error) {
	parts := strings.Fields(fixture.TestCommand)
	command := exec.CommandContext(ctx, parts[0], parts[1:]...)
	command.Dir = grader
	command.Env = append(os.Environ(), "GOWORK=off")
	return command.CombinedOutput()
}

func assertLocalTrustBoundaries(t *testing.T) {
	t.Helper()
	if _, err := mcpdiscover.DefaultDocumentationProvider(mcpdiscover.DocumentationPolicy{OfficialDomains: []string{"pkg.go.dev"}}); !errors.Is(err, mcpdiscover.ErrDocumentationUnavailable) {
		t.Fatalf("DefaultDocumentationProvider() error = %v, want ErrDocumentationUnavailable", err)
	}
	if _, err := security.NewOSSandbox("", false); err == nil {
		t.Fatal("NewOSSandbox() succeeded without a workspace")
	}
	workspace := t.TempDir()
	guard := security.NewGuard(&security.Policy{WritablePaths: []string{"."}}, workspace, nil)
	if err := guard.Before(context.Background(), &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: map[string]any{"path": "../outside.go"}}); err == nil {
		t.Fatal("file_write traversal was allowed")
	}
	if _, err := plan.AdmitPlanScope("trusted-tenant", plan.PlanScope{TenantID: "untrusted-tenant", RepositoryID: "repository"}); !errors.Is(err, plan.ErrTenantScopeMismatch) {
		t.Fatalf("AdmitPlanScope() error = %v, want ErrTenantScopeMismatch", err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
