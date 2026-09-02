package orchestrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	storagememory "github.com/spawn08/chronos/storage/adapters/memory"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
)

type routingTestProvider struct {
	provider string
	model    string
}

func (p *routingTestProvider) Chat(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
	return nil, nil
}

func (p *routingTestProvider) StreamChat(context.Context, *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	return nil, nil
}

func (p *routingTestProvider) Name() string  { return p.provider }
func (p *routingTestProvider) Model() string { return p.model }

type skillContextTestProvider struct {
	mu       sync.Mutex
	requests []*model.ChatRequest
}

func (p *skillContextTestProvider) Chat(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
	p.record(req)
	return &model.ChatResponse{Role: model.RoleAssistant, Content: "ok"}, nil
}

func (p *skillContextTestProvider) StreamChat(_ context.Context, req *model.ChatRequest) (<-chan *model.ChatResponse, error) {
	p.record(req)
	ch := make(chan *model.ChatResponse, 1)
	ch <- &model.ChatResponse{Role: model.RoleAssistant, Content: "ok"}
	close(ch)
	return ch, nil
}

func (p *skillContextTestProvider) Name() string  { return "test" }
func (p *skillContextTestProvider) Model() string { return "test" }

func (p *skillContextTestProvider) record(req *model.ChatRequest) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, req)
}

func (p *skillContextTestProvider) request(index int) *model.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[index]
}

func writeTestSkill(t *testing.T, root, name, trigger, body string) {
	t.Helper()
	dir := filepath.Join(root, config.ConfigDirName, "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	content := "---\nname: " + name + "\ndescription: test skill\ntriggers: [" + trigger + "]\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
}

func newRoutingTestOrchestrator(t *testing.T, models map[router.Complexity]map[router.TaskKind]router.ModelSpec) *Orchestrator {
	t.Helper()
	cfg := &router.Config{
		IntentRouting: []router.IntentRoute{{Intent: "debug", Agent: "debugger", Patterns: []string{"fix"}}},
		ModelRouting:  router.ModelRouting{Models: models},
	}
	rt, err := router.New(cfg, "coder")
	if err != nil {
		t.Fatalf("router.New() error = %v", err)
	}
	orch := &Orchestrator{
		agents: map[string]*agent.Agent{
			"coder":    {ID: "coder", Model: &routingTestProvider{provider: "old", model: "old-coder"}},
			"debugger": {ID: "debugger", Model: &routingTestProvider{provider: "old", model: "old-debugger"}},
		},
		active:         "coder",
		router:         rt,
		routingConfig:  cfg,
		routingState:   make(map[string]router.Classification),
		modelOverrides: make(map[string]bool),
	}
	orch.buildProvider = func(cfg agent.ModelConfig) (model.Provider, error) {
		return &routingTestProvider{provider: cfg.Provider, model: cfg.Model}, nil
	}
	return orch
}

func TestRouteAppliesResolvedModelWithoutChangingSelectedAgent(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityHigh: {
			router.TaskKindDebug: {Provider: "routed", Model: "high-debug"},
		},
		router.ComplexityMedium: {
			router.TaskKindEdit: {Provider: "fallback", Model: "medium-edit"},
		},
	})

	agentID, matched := orch.Route(context.Background(), "fix this bug across multiple files")
	if !matched || agentID != "debugger" {
		t.Fatalf("Route() = (%q, %v), want (debugger, true)", agentID, matched)
	}
	if orch.ActiveID() != "coder" {
		t.Fatalf("ActiveID() = %q, want Route to leave active agent unchanged", orch.ActiveID())
	}
	selected := orch.agents[agentID].Model
	if selected.Name() != "routed" || selected.Model() != "high-debug" {
		t.Fatalf("selected agent model = (%q, %q), want (routed, high-debug)", selected.Name(), selected.Model())
	}
}

func TestModelEscalationIsCapped(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityLow:    {router.TaskKindEdit: {Provider: "routed", Model: "low"}},
		router.ComplexityMedium: {router.TaskKindEdit: {Provider: "routed", Model: "medium"}},
		router.ComplexityHigh:   {router.TaskKindEdit: {Provider: "routed", Model: "high"}},
	})
	builds := 0
	orch.buildProvider = func(cfg agent.ModelConfig) (model.Provider, error) {
		builds++
		return &routingTestProvider{provider: cfg.Provider, model: cfg.Model}, nil
	}

	orch.Route(context.Background(), "change this")
	hook := modelEscalationHook{orchestrator: orch, agentID: "coder"}
	failedTool := &hooks.Event{Type: hooks.EventToolCallAfter, Error: errors.New("recoverable tool failure")}
	for range 3 {
		if err := hook.After(context.Background(), failedTool); err != nil {
			t.Fatalf("After() error = %v", err)
		}
	}
	if provider, modelID := orch.ActiveModelInfo(); provider != "routed" || modelID != "high" {
		t.Fatalf("ActiveModelInfo() = (%q, %q), want capped model (routed, high)", provider, modelID)
	}
	if builds != 3 {
		t.Fatalf("provider builds = %d, want 3 (route plus two escalations)", builds)
	}
}

func TestEscalationProviderFailurePreservesExistingModelAndLevel(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityLow:    {router.TaskKindEdit: {Provider: "routed", Model: "low"}},
		router.ComplexityMedium: {router.TaskKindEdit: {Provider: "routed", Model: "medium"}},
	})
	orch.Route(context.Background(), "change this")
	original := orch.agents["coder"].Model
	orch.buildProvider = func(agent.ModelConfig) (model.Provider, error) {
		return nil, errors.New("provider construction failed")
	}
	hook := modelEscalationHook{orchestrator: orch, agentID: "coder"}
	failedTool := &hooks.Event{Type: hooks.EventToolCallAfter, Error: errors.New("recoverable tool failure")}

	if err := hook.After(context.Background(), failedTool); err != nil {
		t.Fatalf("After() error = %v", err)
	}
	if orch.agents["coder"].Model != original {
		t.Fatal("failed provider construction replaced the existing provider")
	}

	orch.buildProvider = func(cfg agent.ModelConfig) (model.Provider, error) {
		return &routingTestProvider{provider: cfg.Provider, model: cfg.Model}, nil
	}
	if err := hook.After(context.Background(), failedTool); err != nil {
		t.Fatalf("After() retry error = %v", err)
	}
	if provider, modelID := orch.ActiveModelInfo(); provider != "routed" || modelID != "medium" {
		t.Fatalf("ActiveModelInfo() after retry = (%q, %q), want unchanged escalation target (routed, medium)", provider, modelID)
	}
}

func TestRouteRespectsExplicitModelOverride(t *testing.T) {
	orch := newRoutingTestOrchestrator(t, map[router.Complexity]map[router.TaskKind]router.ModelSpec{
		router.ComplexityLow:    {router.TaskKindEdit: {Provider: "routed", Model: "low"}},
		router.ComplexityMedium: {router.TaskKindEdit: {Provider: "routed", Model: "medium"}},
	})
	if err := orch.SwitchModel(context.Background(), "user", "chosen"); err != nil {
		t.Fatalf("SwitchModel() error = %v", err)
	}

	orch.Route(context.Background(), "change this")
	if provider, modelID := orch.ActiveModelInfo(); provider != "user" || modelID != "chosen" {
		t.Fatalf("ActiveModelInfo() = (%q, %q), want explicit override (user, chosen)", provider, modelID)
	}
}

func TestSkillContextParityPreservesExistingPins(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	writeTestSkill(t, root, "parity-skill", "paritytoken", "parity skill body")

	provider := &skillContextTestProvider{}
	a := &agent.Agent{
		ID:         "coder",
		Model:      provider,
		Tools:      tool.NewRegistry(),
		Guardrails: guardrails.NewEngine(),
		ContextPinsFn: func(context.Context) []model.Message {
			return []model.Message{{Role: model.RoleSystem, Content: "existing pin"}}
		},
	}
	setupSkills(&config.Config{}, root, map[string]*agent.Agent{"coder": a})
	orch := &Orchestrator{
		agents:   map[string]*agent.Agent{"coder": a},
		active:   "coder",
		sessions: map[string]string{"coder": "session-1"},
	}

	if _, err := orch.Chat(context.Background(), "paritytoken"); err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	stream, err := orch.ChatStream(context.Background(), "paritytoken")
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	for range stream {
	}

	blockingPins := systemContents(provider.request(0))
	streamingPins := systemContents(provider.request(1))
	if strings.Join(blockingPins, "\n") != strings.Join(streamingPins, "\n") {
		t.Fatalf("blocking pins %q differ from streaming pins %q", blockingPins, streamingPins)
	}
	joined := strings.Join(blockingPins, "\n")
	if !strings.Contains(joined, "existing pin") || !strings.Contains(joined, "parity skill body") {
		t.Fatalf("pins = %q, want existing pin and selected current-message skill", joined)
	}
}

func TestSkillToolHistoryIsBoundedAndSessionScoped(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	toolNames := []string{"toolalpha", "toolbravo", "toolcharlie", "tooldelta", "toolecho", "toolfoxtrot"}
	for _, name := range toolNames {
		writeTestSkill(t, root, name+"-skill", name, name+" body")
	}

	a := &agent.Agent{ID: "coder"}
	setupSkills(&config.Config{}, root, map[string]*agent.Agent{"coder": a})
	if len(a.Hooks) != 1 {
		t.Fatalf("skill hooks = %d, want 1", len(a.Hooks))
	}
	sessionOne := storage.WithSession(context.Background(), "session-1")
	for _, name := range toolNames {
		if err := a.Hooks.After(sessionOne, &hooks.Event{Type: hooks.EventToolCallAfter, Name: name}); err != nil {
			t.Fatalf("After(%q) error = %v", name, err)
		}
	}

	pins := a.ContextPinsFn(context.WithValue(sessionOne, messageKey{}, "continue"))
	rendered := strings.Join(messageContents(pins), "\n")
	if strings.Contains(rendered, "toolalpha body") {
		t.Fatalf("oldest tool influenced selection after history limit: %q", rendered)
	}
	if count := strings.Count(rendered, "<skill name="); count != 3 {
		t.Fatalf("selected skill count = %d, want top-K limit 3; pins = %q", count, rendered)
	}

	sessionTwo := storage.WithSession(context.Background(), "session-2")
	otherPins := a.ContextPinsFn(context.WithValue(sessionTwo, messageKey{}, "continue"))
	otherRendered := strings.Join(messageContents(otherPins), "\n")
	for _, name := range toolNames {
		if strings.Contains(otherRendered, name+" body") {
			t.Fatalf("session-1 tool %q leaked into session-2 pins: %q", name, otherRendered)
		}
	}
}

func systemContents(req *model.ChatRequest) []string {
	var contents []string
	for _, msg := range req.Messages {
		if msg.Role == model.RoleSystem {
			contents = append(contents, msg.Content)
		}
	}
	return contents
}

func messageContents(messages []model.Message) []string {
	contents := make([]string, 0, len(messages))
	for _, msg := range messages {
		contents = append(contents, msg.Content)
	}
	return contents
}

func newPermissionTestOrchestrator(t *testing.T, store storage.Storage) *Orchestrator {
	t.Helper()
	policy, err := security.LoadPolicy([]byte(`
shell:
  allowed_commands: ["go", "git", "rm"]
  auto_allow: ['^go test']
  confirm: ['^git push']
  never_allow: ['^rm dangerous$']
`))
	if err != nil {
		t.Fatalf("LoadPolicy() error = %v", err)
	}
	agents := map[string]*agent.Agent{
		"coder":    {ID: "coder", Tools: tool.NewRegistry()},
		"reviewer": {ID: "reviewer", Tools: tool.NewRegistry()},
	}
	orch := &Orchestrator{
		agents:            agents,
		store:             store,
		permissionChecker: security.NewPermissionChecker(policy, "/workspace"),
	}
	orch.SetApprovalHandler(nil)
	return orch
}

func registerApprovalTool(registry *tool.Registry, name string, executions *int) {
	registerPermissionTool(registry, name, tool.PermRequireApproval, executions)
}

func registerPermissionTool(registry *tool.Registry, name string, permission tool.Permission, executions *int) {
	registry.Register(&tool.Definition{
		Name:       name,
		Permission: permission,
		Handler: func(context.Context, map[string]any) (any, error) {
			(*executions)++
			return "executed", nil
		},
	})
}

func TestNormalizeAllowShellAppliesConfirmAndNeverAllowPolicy(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerPermissionTool(a.Tools, "shell", tool.PermAllow, &executions)
	normalizeToolPermissions(orch.agents)

	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "git push origin main"}); err == nil {
		t.Fatal("originally allowed git push executed after human rejection")
	}
	if approvals != 1 || executions != 0 {
		t.Fatalf("git push counts = approvals %d, executions %d; want 1, 0", approvals, executions)
	}
	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "rm dangerous"}); err == nil {
		t.Fatal("originally allowed never_allow command executed")
	}
	if approvals != 1 || executions != 0 {
		t.Fatalf("never_allow counts = approvals %d, executions %d; want 1, 0", approvals, executions)
	}
}

func TestNormalizeAllowUnknownToolRequiresConfirmation(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerPermissionTool(a.Tools, "mcp_unknown", tool.PermAllow, &executions)
	normalizeToolPermissions(orch.agents)

	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if _, err := a.Tools.Execute(context.Background(), "mcp_unknown", nil); err == nil {
		t.Fatal("originally allowed unknown tool executed after human rejection")
	}
	if approvals != 1 || executions != 0 {
		t.Fatalf("counts = approvals %d, executions %d; want 1, 0", approvals, executions)
	}
}

func TestNormalizeAllowReadOnlyToolExecutesWithoutHumanCallback(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerPermissionTool(a.Tools, "file_read", tool.PermAllow, &executions)
	normalizeToolPermissions(orch.agents)

	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if _, err := a.Tools.Execute(context.Background(), "file_read", map[string]any{"path": "README.md"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if approvals != 0 || executions != 1 {
		t.Fatalf("counts = approvals %d, executions %d; want 0, 1", approvals, executions)
	}
}

func TestNormalizeLeavesDeniedToolDenied(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerPermissionTool(a.Tools, "blocked", tool.PermDeny, &executions)
	normalizeToolPermissions(orch.agents)

	def, ok := a.Tools.Get("blocked")
	if !ok || def.Permission != tool.PermDeny {
		t.Fatalf("permission = %q, found %v; want %q", def.Permission, ok, tool.PermDeny)
	}
	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return true, nil
	})
	if _, err := a.Tools.Execute(context.Background(), "blocked", nil); err == nil {
		t.Fatal("denied tool executed")
	}
	if approvals != 0 || executions != 0 {
		t.Fatalf("counts = approvals %d, executions %d; want 0, 0", approvals, executions)
	}
}

func TestApprovalCompositionAppliesToEveryAgent(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})

	for id, a := range orch.agents {
		executions := 0
		registerApprovalTool(a.Tools, "shell", &executions)
		if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "go test ./..."}); err != nil {
			t.Fatalf("%s auto Execute() error = %v", id, err)
		}
		if approvals != 0 || executions != 1 {
			t.Fatalf("%s auto counts = approvals %d, executions %d; want 0, 1", id, approvals, executions)
		}
		if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "git push origin main"}); err == nil {
			t.Fatalf("%s confirm Execute() succeeded after rejection", id)
		}
		if approvals != 1 || executions != 1 {
			t.Fatalf("%s confirm counts = approvals %d, executions %d; want 1, 1", id, approvals, executions)
		}
		approvals = 0
	}
}

func TestAutoApproveModePreservesConfirmAndDeny(t *testing.T) {
	store := storagememory.New()
	orch := newPermissionTestOrchestrator(t, store)
	a := orch.agents["coder"]
	executions := 0
	registerApprovalTool(a.Tools, "shell", &executions)
	registerApprovalTool(a.Tools, "file_write", &executions)
	approvals := 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		approvals++
		return false, nil
	})
	if err := orch.SetPermissionMode("auto_approve"); err != nil {
		t.Fatalf("SetPermissionMode() error = %v", err)
	}
	if got := a.Tools.PermissionMode(); got != tool.PermissionModePrompt {
		t.Fatalf("registry permission mode = %q, want prompt so policy is not bypassed", got)
	}

	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "go test ./..."}); err != nil {
		t.Fatalf("auto Execute() error = %v", err)
	}
	if _, err := a.Tools.Execute(context.Background(), "file_write", map[string]any{"path": "internal/new.go"}); err != nil {
		t.Fatalf("yolo file_write Execute() error = %v", err)
	}
	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "git push origin main"}); err == nil {
		t.Fatal("explicit confirm command executed after rejection")
	}
	ctx := storage.WithSession(context.Background(), "session-1")
	if _, err := a.Tools.Execute(ctx, "shell", map[string]any{"command": "rm dangerous"}); err == nil {
		t.Fatal("never_allow command executed")
	}
	if approvals != 1 || executions != 2 {
		t.Fatalf("counts = approvals %d, executions %d; want 1, 2", approvals, executions)
	}
	logs, err := store.ListAuditLogs(ctx, "session-1", 10, 0)
	if err != nil {
		t.Fatalf("ListAuditLogs() error = %v", err)
	}
	if len(logs) != 1 || logs[0].Action != "block" || logs[0].Resource != "shell" {
		t.Fatalf("audit logs = %#v, want one shell block", logs)
	}
}

func TestApprovalHandlerReplacementRetainsPolicyComposition(t *testing.T) {
	orch := newPermissionTestOrchestrator(t, nil)
	a := orch.agents["coder"]
	executions := 0
	registerApprovalTool(a.Tools, "shell", &executions)
	first, second := 0, 0
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		first++
		return true, nil
	})
	orch.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) {
		second++
		return true, nil
	})

	if _, err := a.Tools.Execute(context.Background(), "shell", map[string]any{"command": "git push origin main"}); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if first != 0 || second != 1 || executions != 1 {
		t.Fatalf("counts = first %d, second %d, executions %d; want 0, 1, 1", first, second, executions)
	}
}
