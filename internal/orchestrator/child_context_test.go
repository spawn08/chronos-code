package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/skills"
	"github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
)

func childRequestText(req *model.ChatRequest) string {
	var text strings.Builder
	for _, message := range req.Messages {
		text.WriteString(message.Content + "\n")
	}
	return text.String()
}

func TestDynamicChildFileReadContextAndAuthorization(t *testing.T) {
	calls, approvals, reads := 0, 0, 0
	var childIdentity agent.RunIdentity
	provider := resourceProvider{chat: func(ctx context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
		calls++
		childIdentity, _ = agent.RunIdentityFromContext(ctx)
		if calls == 1 {
			text := childRequestText(req)
			for _, wanted := range []string{"custom specialist", "Never expose secrets", "Cite current file:line", "Project rule: preserve local changes", "Granted tools: file_read", "Effect grant: [read]", "Model: this request is served by resource-test"} {
				if !strings.Contains(text, wanted) {
					t.Errorf("request missing %q: %s", wanted, text)
				}
			}
			for _, absent := range []string{"<skill_catalog>", "discover_tools", "call_tool", "codebase_search", "Specialized tool catalog", "Use git history"} {
				if strings.Contains(text, absent) {
					t.Errorf("misleading child context %q: %s", absent, text)
				}
			}
			if len(req.Tools) != 1 || req.Tools[0].Function.Name != "file_read" {
				t.Fatalf("child schemas = %+v", req.Tools)
			}
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "read", Name: "file_read", Arguments: "{}"}, {ID: "escape", Name: "file_write", Arguments: "{}"}}}, nil
		}
		if !strings.Contains(childRequestText(req), "not found") {
			t.Error("ungranted tool was not rejected")
		}
		return resourceReply("done"), nil
	}}
	parent := resourceAgent(t, "parent", provider)
	parent.ContextCfg.PinnedMessages = []model.Message{{Role: model.RoleSystem, Content: "Project rule: preserve local changes"}}
	parent.ContextPinsFn = func(context.Context) []model.Message {
		return []model.Message{
			{Role: model.RoleSystem, Name: "harness-index-guidance", Content: "# Intent and evidence\nCite current file:line.\n# Code index and graph context\nUse codebase_search. Use git history.\n# Safety\nNever expose secrets."},
			{Role: model.RoleSystem, Content: "<skill_catalog>Always call skill</skill_catalog>"},
			{Role: model.RoleSystem, Content: "Specialized tool catalog (current registry; use discover_tools and call_tool"},
		}
	}
	parent.Tools.Register(&tool.Definition{Name: "file_read", Permission: tool.PermRequireApproval, Effects: []tool.Effect{tool.EffectRead}, Handler: func(context.Context, map[string]any) (any, error) {
		reads++
		return "source", nil
	}})
	parent.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Handler: func(context.Context, map[string]any) (any, error) {
		t.Error("ungranted write executed")
		return nil, nil
	}})
	parent.Tools.SetApprovalHandler(func(_ context.Context, name string, _ map[string]any) (bool, error) {
		if name != "file_read" {
			t.Errorf("unexpected approval %q", name)
		}
		approvals++
		return true, nil
	})
	log := &hooks.LoggingHook{}
	parent.Hooks = hooks.Chain{log, deferredToolsHook{core: []string{"file_read"}}}
	if err := setupSubAgents(map[string]*agent.Agent{"parent": parent}); err != nil {
		t.Fatal(err)
	}
	identity := agent.RunIdentity{RoleID: "parent", InvocationID: "parent-invocation", TaskID: "task"}
	ctx := agent.WithRunIdentity(tool.WithEffectGrant(context.Background(), tool.EffectRead, tool.EffectDeliveryWrite), identity)
	result, err := parent.Tools.Execute(ctx, "spawn_subagent", map[string]any{"system_prompt": "custom specialist", "tools": []any{"file_read"}, "task": "inspect source"})
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || calls != 2 || approvals != 1 || reads != 1 || len(log.Events) == 0 {
		t.Fatalf("result=%v calls=%d approvals=%d reads=%d hooks=%d", result, calls, approvals, reads, len(log.Events))
	}
	if childIdentity.ParentInvocationID != identity.InvocationID || childIdentity.InvocationID == identity.InvocationID || childIdentity.RoleID != "subagent:dynamic" || childIdentity.TaskID != "task" {
		t.Fatalf("child identity = %+v", childIdentity)
	}
}

func TestDynamicChildRicherGrantCatalogAndDispatch(t *testing.T) {
	calls, reads, approvals := 0, 0, 0
	provider := resourceProvider{chat: func(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
		calls++
		text := childRequestText(req)
		if calls <= 2 && (strings.Contains(text, "file_write") || strings.Contains(text, "requires-write")) {
			t.Fatalf("parent-only capability leaked: %s", text)
		}
		if calls == 1 {
			if len(req.Tools) != 4 || !strings.Contains(text, "read-source") || strings.Contains(text, "requires-other-model") {
				t.Fatalf("richer request = %+v\n%s", req.Tools, text)
			}
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "discover", Name: "discover_tools", Arguments: "{}"}}}, nil
		}
		if calls == 2 {
			if !strings.Contains(text, "file_read") {
				t.Fatal("granted tool missing from discovery")
			}
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "dispatch", Name: "call_tool", Arguments: `{"name":"file_read","arguments":{}}`}, {ID: "escape", Name: "call_tool", Arguments: `{"name":"file_write","arguments":{}}`}}}, nil
		}
		if calls == 3 {
			if !strings.Contains(text, "unknown tool") {
				t.Fatal("dispatch escape was not rejected")
			}
			return &model.ChatResponse{StopReason: model.StopReasonToolCall, ToolCalls: []model.ToolCall{{ID: "bad-skill", Name: "skill", Arguments: `{"name":"requires-write"}`}, {ID: "bad-dispatched-skill", Name: "call_tool", Arguments: `{"name":"skill","arguments":{"name":"requires-write"}}`}}}, nil
		}
		if !strings.Contains(text, "use granted tools directly") {
			t.Fatal("unavailable skill was not rejected")
		}
		return resourceReply("done"), nil
	}}
	parent := resourceAgent(t, "parent", provider)
	parent.Tools.Register(&tool.Definition{Name: "file_read", Permission: tool.PermRequireApproval, Effects: []tool.Effect{tool.EffectRead}, Handler: func(context.Context, map[string]any) (any, error) { reads++; return "source", nil }})
	parent.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectDeliveryWrite}, Handler: func(context.Context, map[string]any) (any, error) { t.Error("dispatch escaped grant"); return nil, nil }})
	parent.Tools.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) { approvals++; return true, nil })
	orch := &Orchestrator{cfg: &config.Config{}}
	orch.cfg.Tools.DeferredSchemas = true
	orch.installDeferredTools(parent)
	catalog := []*skills.Skill{
		{Name: "read-source", ToolsRequired: []string{"file_read"}},
		{Name: "requires-write", ToolsRequired: []string{"file_write"}},
		{Name: "requires-other-model", ModelHint: "different-model"},
	}
	parent.Tools.Register(skillTool(parent, catalog))
	previous := parent.ContextPinsFn
	parent.ContextPinsFn = func(ctx context.Context) []model.Message {
		pins := previous(ctx)
		if childPins, dynamic := dynamicChildSkillPins(ctx, catalog); dynamic {
			return append(pins, childPins...)
		}
		return pins
	}
	runner := &dynamicChildRunner{parent: parent}
	result, err := runner.Run(tool.WithEffectGrant(context.Background(), tool.EffectRead), harness.SubAgentSpec{Name: "reader", SystemPrompt: "Read directly when feasible", ToolNames: []string{"file_read", "discover_tools", "call_tool", "skill"}}, "inspect")
	if err != nil || result != "done" || reads != 1 || approvals != 1 {
		t.Fatalf("result=%q error=%v reads=%d approvals=%d", result, err, reads, approvals)
	}
}

func TestDynamicChildProviderOverrideAndGuardrails(t *testing.T) {
	parentProvider := &subagentTestProvider{name: "parent", modelID: "parent-model", response: "parent"}
	override := &subagentTestProvider{name: "override", modelID: "current-model", response: "done"}
	parent := resourceAgent(t, "parent", parentProvider)
	parent.Guardrails.AddRule(guardrails.Rule{Name: "input", Position: guardrails.Input, Guardrail: &guardrails.BlocklistGuardrail{Blocklist: []string{"blocked"}}})
	parent.Guardrails.AddRule(guardrails.Rule{Name: "output", Position: guardrails.Output, Guardrail: &guardrails.BlocklistGuardrail{Blocklist: []string{"forbidden"}}})
	runner := &dynamicChildRunner{parent: parent}
	spec := harness.SubAgentSpec{Name: "reasoner", SystemPrompt: "custom role"}
	ctx := agent.WithModelProvider(context.Background(), override)
	if _, err := runner.Run(ctx, spec, "blocked"); err == nil || !strings.Contains(err.Error(), "input guardrail") {
		t.Fatalf("input guardrail error = %v", err)
	}
	if result, err := runner.Run(ctx, spec, "reason"); err != nil || result != "done" {
		t.Fatalf("result=%q error=%v", result, err)
	}
	if len(parentProvider.requests) != 0 || len(override.requests) != 1 {
		t.Fatalf("provider calls parent=%d override=%d", len(parentProvider.requests), len(override.requests))
	}
	if text := childRequestText(override.requests[0]); !strings.Contains(text, "current-model (provider override)") || strings.Contains(text, "parent-model") {
		t.Fatalf("stale model snapshot: %s", text)
	}
	override.response = "forbidden"
	if _, err := runner.Run(ctx, spec, "reason"); err == nil || !strings.Contains(err.Error(), "output guardrail") {
		t.Fatalf("output guardrail error = %v", err)
	}
}

func TestChildContextNestedGrantCannotExpand(t *testing.T) {
	parent := resourceAgent(t, "parent", resourceProvider{chat: func(context.Context, *model.ChatRequest) (*model.ChatResponse, error) {
		return nil, fmt.Errorf("provider must not be called")
	}})
	parent.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	ancestor := resourceAgent(t, "ancestor", parent.Model)
	ctx := context.WithValue(context.Background(), childContextKey{}, &childContext{agent: ancestor})
	_, err := (&dynamicChildRunner{parent: parent}).Run(ctx, harness.SubAgentSpec{Name: "nested", SystemPrompt: "role", ToolNames: []string{"file_write"}}, "task")
	if err == nil || !strings.Contains(err.Error(), "undelegated tool") {
		t.Fatalf("nested grant expansion = %v", err)
	}
}

func TestDynamicChildSharedYAMLPolicyAndEmptyGrant(t *testing.T) {
	provider := resourceProvider{chat: func(_ context.Context, req *model.ChatRequest) (*model.ChatResponse, error) {
		text := childRequestText(req)
		for _, wanted := range []string{"custom role", "# Intent and evidence", "Name any material coverage gap", "Granted tools: none", "Effect grant: []"} {
			if !strings.Contains(text, wanted) {
				t.Errorf("missing shared policy %q: %s", wanted, text)
			}
		}
		for _, absent := range []string{"# Code index and graph context", "Use git history for", "codebase_search"} {
			if strings.Contains(text, absent) {
				t.Errorf("unavailable capability guidance %q: %s", absent, text)
			}
		}
		if len(req.Tools) != 0 {
			t.Fatalf("reasoning-only child schemas: %+v", req.Tools)
		}
		return resourceReply("done"), nil
	}}
	parent := resourceAgent(t, "parent", provider)
	if err := installIndexGuidance(map[string]*agent.Agent{"parent": parent}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&dynamicChildRunner{parent: parent}).Run(tool.WithEffectGrant(context.Background()), harness.SubAgentSpec{Name: "reasoner", SystemPrompt: "custom role"}, "reason from supplied evidence"); err != nil {
		t.Fatal(err)
	}
}

func TestChildContextEffectAndModelSnapshot(t *testing.T) {
	provider := &subagentTestProvider{name: "live", modelID: "current-model"}
	child := resourceAgent(t, "child", provider)
	child.Tools.Register(&tool.Definition{Name: "file_read", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectRead}, Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	child.Tools.Register(&tool.Definition{Name: "file_write", Permission: tool.PermAllow, Effects: []tool.Effect{tool.EffectDeliveryWrite}, Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	ctx := context.WithValue(tool.WithEffectGrant(context.Background(), tool.EffectRead), childContextKey{}, &childContext{agent: child})
	manifest := effectiveCapabilityManifest(ctx, nil)
	if _, denied := manifest.Tools["file_write"]; denied || manifest.ModelID != "current-model" {
		t.Fatalf("snapshot = %+v", manifest)
	}
	if _, available := manifest.Tools["file_read"]; !available {
		t.Fatal("read capability missing")
	}
}

func TestChildContextNavigationRemovalPreservesStandingPolicy(t *testing.T) {
	policy := "# Safety\nNever leak credentials.\n# Code index and graph context\nCall parent graph tools.\n# Intent and evidence\nReport coverage gaps.\n# Project rules\nPreserve edits."
	actual := withoutChildNavigation(policy)
	if strings.Contains(actual, "parent graph") || !strings.Contains(actual, "Never leak credentials.") || !strings.Contains(actual, "Report coverage gaps.") || !strings.Contains(actual, "Preserve edits.") {
		t.Fatalf("filtered policy = %s", actual)
	}
}
