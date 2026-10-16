package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos-code/internal/skills"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
)

type childContextKey struct{}

type childContext struct {
	agent  *agent.Agent
	skills []*skills.Skill
}

// effectiveCapabilityManifest uses request-local capabilities for dynamic
// children, while leaving configured agents' capability selection unchanged.
func effectiveCapabilityManifest(ctx context.Context, a *agent.Agent) skills.CapabilityManifest {
	child, dynamic := ctx.Value(childContextKey{}).(*childContext)
	if dynamic && child != nil {
		a = child.agent
	}
	manifest := skillCapabilityManifest(a)
	if dynamic && child != nil {
		for _, definition := range a.Tools.List() {
			if definition.ResolveEffects == nil && tool.RequireEffects(ctx, definition.Effects...) != nil {
				delete(manifest.Tools, definition.Name)
			}
		}
	}
	if provider := requestModelProvider(ctx, a); provider != nil {
		manifest.ModelID = provider.Model()
	}
	return manifest
}

// dynamicChildSkillPins is the parent skill installer's request-local seam.
// A dynamic child with no skill grant must not receive a loading obligation.
func dynamicChildSkillPins(ctx context.Context, catalog []*skills.Skill) ([]model.Message, bool) {
	child, ok := ctx.Value(childContextKey{}).(*childContext)
	if !ok || child == nil {
		return nil, false
	}
	manifest := effectiveCapabilityManifest(ctx, child.agent)
	if _, granted := manifest.Tools[skillToolName]; !granted {
		return nil, true
	}
	available := skills.Available(catalog, manifest)
	child.skills = available
	listing, listed := skills.RenderCatalog(available, skillToolName)
	if selected, _ := ctx.Value(explicitSkillKey{}).(*skills.Skill); selected != nil && skills.Find(available, selected.Name) != nil {
		listing = strings.TrimSpace(skills.Render([]*skills.Skill{selected}) + "\n\n" + listing)
		listed++
	}
	if listing == "" {
		return nil, true
	}
	contextSourceSelected(ctx, ContextSourceSkills, listed, len(listing), false)
	return []model.Message{{Role: model.RoleSystem, Name: "harness-skill-catalog", Content: listing}}, true
}

// dynamicChildRunner mirrors the SDK's isolated builder, replacing only
// registry-dependent pins, schema deferral and dispatch. Parent registries
// remain the authorization boundary; agents and their locks are never copied.
type dynamicChildRunner struct {
	parent *agent.Agent
}

func (r *dynamicChildRunner) Run(ctx context.Context, spec harness.SubAgentSpec, task string) (string, error) {
	if spec.SystemPrompt == "" && len(spec.Instructions) == 0 {
		return "", fmt.Errorf("dynamic subagent %q needs a system prompt or instructions", spec.Name)
	}
	parent := r.parent
	if parent == nil || parent.Tools == nil {
		return "", fmt.Errorf("dynamic subagent %q requires a parent tool registry", spec.Name)
	}
	provider := requestModelProvider(ctx, parent)
	if provider == nil {
		return "", fmt.Errorf("dynamic subagent %q requires a model", spec.Name)
	}
	contextCfg := parent.ContextCfg
	contextCfg.PinnedMessages = nil
	b := agent.New("subagent:"+spec.Name, spec.Name).
		WithModel(provider).
		WithUserID(parent.UserID).
		WithStorage(parent.Storage).
		WithBroker(parent.Broker).
		WithTracer(parent.Tracer).
		WithContextConfig(contextCfg).
		WithReasoningConfig(parent.ReasoningConfig).
		WithMaxIterations(parent.MaxIterations).
		WithSystemPrompt(spec.SystemPrompt)
	for _, instruction := range spec.Instructions {
		b.AddInstruction(instruction)
	}
	grants := tool.NewRegistry()
	for _, name := range spec.ToolNames {
		if ancestor, ok := ctx.Value(childContextKey{}).(*childContext); ok && ancestor != nil {
			if _, granted := ancestor.agent.Tools.Get(name); !granted {
				return "", fmt.Errorf("dynamic subagent %q requests undelegated tool %q", spec.Name, name)
			}
		}
		definition, ok := parent.Tools.Get(name)
		if !ok {
			return "", fmt.Errorf("dynamic subagent %q requests unknown tool %q", spec.Name, name)
		}
		grants.Register(definition)
	}
	for _, definition := range grants.List() {
		name := definition.Name
		granted := *definition
		if granted.Permission != tool.PermDeny {
			granted.Permission = tool.PermAllow
		}
		granted.RequiresConfirmation = false
		granted.RequiresUserInput = false
		granted.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			if name == skillToolName {
				if err := validateChildSkill(ctx, args); err != nil {
					return nil, err
				}
			}
			if name == "call_tool" {
				if _, _, err := childDeferredTarget(ctx, grants, args); err != nil {
					return nil, fmt.Errorf("dynamic child dispatch: %w", err)
				}
			}
			result, err := parent.Tools.Execute(ctx, name, args)
			if err != nil || name != "discover_tools" {
				return result, err
			}
			return childDiscovery(grants, args)
		}
		if name == "call_tool" {
			granted.ResolveEffects = func(ctx context.Context, args map[string]any) ([]tool.Effect, error) {
				target, arguments, err := childDeferredTarget(ctx, grants, args)
				if err != nil {
					return nil, fmt.Errorf("dynamic child dispatch: %w", err)
				}
				if target.ResolveEffects != nil {
					return target.ResolveEffects(ctx, arguments)
				}
				return target.Effects, nil
			}
		}
		if definition.Handler == nil {
			granted.Handler = nil
		}
		b.AddTool(&granted)
	}
	child, err := b.Build()
	if err != nil {
		return "", fmt.Errorf("build dynamic subagent %q: %w", spec.Name, err)
	}
	// Deferral closes over the parent's core list; all granted child schemas
	// are small enough to expose directly. Retain security and accounting hooks.
	for _, hook := range parent.Hooks {
		if _, deferred := hook.(deferredToolsHook); !deferred {
			child.Hooks = append(child.Hooks, hook)
		}
	}
	if parent.Guardrails != nil {
		child.Guardrails = parent.Guardrails
	}
	child.MaxConcurrentSubAgents = parent.MaxConcurrentSubAgents
	child.ContextPinsFn = func(ctx context.Context) []model.Message {
		pins := append([]model.Message(nil), parent.ContextCfg.PinnedMessages...)
		if parent.ContextPinsFn != nil {
			pins = append(pins, parent.ContextPinsFn(ctx)...)
		}
		return childContextPins(ctx, child, pins)
	}
	ctx = context.WithValue(ctx, childContextKey{}, &childContext{agent: child})
	ctx = agent.WithModelProvider(ctx, provider)
	identity, _ := agent.RunIdentityFromContext(ctx)
	identity.ParentInvocationID = identity.InvocationID
	identity.InvocationID = session.NewSessionID()
	identity.RoleID = child.ID
	ctx = agent.WithRunIdentity(ctx, identity)
	defer releaseScopedRuleState(ctx, child)
	if policy, renewing := longRunningPolicyFromContext(ctx); renewing {
		runtime, _ := taskRuntimeFromContext(ctx)
		ctx = agent.WithToolLoopController(ctx, child.ID, newWindowGovernor(policy, runtime, time.Now))
	}
	if inbox, ok := ctx.Value(supervisedInboxKey{}).(*SteeringInbox); ok {
		ctx = agent.WithPendingInput(ctx, child.ID, inbox)
	}
	result, err := child.Execute(ctx, task)
	if err != nil {
		return result, fmt.Errorf("dynamic subagent %q: %w", spec.Name, err)
	}
	return result, nil
}

func childContextPins(ctx context.Context, child *agent.Agent, inherited []model.Message) []model.Message {
	var pins []model.Message
	_, hasSkill := effectiveCapabilityManifest(ctx, child).Tools[skillToolName]
	for _, pin := range inherited {
		if pin.Role != model.RoleSystem {
			pins = append(pins, pin)
			continue
		}
		switch {
		case pin.Name == "harness-index-guidance":
			// Keep intent, evidence and safety policy verbatim, removing only
			// the parent-specific navigation section from the shared YAML pin.
			pin.Content = withoutChildNavigation(pin.Content)
			pin.Content = strings.ReplaceAll(pin.Content, "Use git history for ", "When a granted tool supports history inspection, use it for ")
		case strings.HasPrefix(pin.Content, "Specialized tool catalog (current registry;"):
			continue
		case strings.HasPrefix(strings.TrimSpace(pin.Content), "<skill_catalog>") || strings.HasPrefix(strings.TrimSpace(pin.Content), "<skill name="):
			// Only named catalogs re-rendered against the child snapshot are
			// safe to retain; old unnamed catalogs close over the parent.
			if !hasSkill || pin.Name != "harness-skill-catalog" {
				continue
			}
		}
		if strings.TrimSpace(pin.Content) != "" {
			pins = append(pins, pin)
		}
	}
	pins = append(pins, model.Message{Role: model.RoleSystem, Name: "dynamic-child-capabilities", Content: childCapabilityGuidance(ctx, child)})
	return pins
}

func withoutChildNavigation(policy string) string {
	lines := strings.Split(policy, "\n")
	out := make([]string, 0, len(lines))
	omit := false
	for _, line := range lines {
		if strings.HasPrefix(line, "# ") {
			omit = strings.TrimSpace(line) == "# Code index and graph context"
		}
		if !omit {
			out = append(out, line)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func childCapabilityGuidance(ctx context.Context, child *agent.Agent) string {
	manifest := effectiveCapabilityManifest(ctx, child)
	var names []string
	for name := range manifest.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	var text strings.Builder
	text.WriteString("Dynamic child capabilities (availability is not authorization). Use granted tools directly for feasible work; do not assume additional tools or parent conversation history are available. Report any coverage limitation instead of inventing evidence.\nGranted tools: ")
	if len(names) == 0 {
		text.WriteString("none; reason from the supplied task and evidence only")
	} else {
		text.WriteString(strings.Join(names, ", "))
	}
	if effects, constrained := tool.EffectGrantFromContext(ctx); constrained {
		var allowed []string
		for effect := range effects {
			allowed = append(allowed, string(effect))
		}
		sort.Strings(allowed)
		fmt.Fprintf(&text, "\nEffect grant: [%s]. Tool approvals and live policy still apply.", strings.Join(allowed, ", "))
	} else {
		text.WriteString("\nEffects remain subject to tool authorization and live policy; no request-scoped effect grant is set.")
	}
	text.WriteString(modelLine(requestModelProvider(ctx, child)))
	return text.String()
}

func validateChildSkill(ctx context.Context, args map[string]any) error {
	child, ok := ctx.Value(childContextKey{}).(*childContext)
	name, _ := args["name"].(string)
	if !ok || child == nil || skills.Find(child.skills, name) == nil {
		return fmt.Errorf("dynamic child skill %q is unavailable; use granted tools directly", name)
	}
	return nil
}

func childDeferredTarget(ctx context.Context, registry *tool.Registry, args map[string]any) (*tool.Definition, map[string]any, error) {
	target, arguments, err := deferredTarget(registry, args)
	if err == nil && target.Name == skillToolName {
		err = validateChildSkill(ctx, arguments)
	}
	return target, arguments, err
}

func childDiscovery(registry *tool.Registry, args map[string]any) (any, error) {
	namespace, err := discoveryString(args, "namespace")
	if err != nil {
		return nil, err
	}
	name, err := discoveryString(args, "name")
	if err != nil {
		return nil, err
	}
	query, err := discoveryString(args, "query")
	if err != nil {
		return nil, err
	}
	matches := make([]map[string]any, 0)
	for _, definition := range registry.List() {
		if definition.Name == "call_tool" || definition.Name == "discover_tools" || definition.Permission == tool.PermDeny || definition.Handler == nil {
			continue
		}
		if name != "" && name != definition.Name {
			continue
		}
		if namespace != "" && namespace != toolNamespace(definition.Name) && "mcp:"+namespace != toolNamespace(definition.Name) {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(definition.Name+" "+definition.Description), strings.ToLower(query)) {
			continue
		}
		matches = append(matches, map[string]any{"name": definition.Name, "namespace": toolNamespace(definition.Name), "description": definition.Description, "parameters": definition.Parameters, "permission": definition.Permission})
	}
	if name != "" && len(matches) == 0 {
		return nil, fmt.Errorf("dynamic child cannot discover ungranted tool %q", name)
	}
	truncated := len(matches) > deferredSchemaLimit
	if truncated {
		matches = matches[:deferredSchemaLimit]
	}
	return map[string]any{"tools": matches, "truncated": truncated}, nil
}
