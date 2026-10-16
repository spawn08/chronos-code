package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

func deferredFixture(t *testing.T) *agent.Agent {
	t.Helper()
	a := pipelineAgent(t)
	o := &Orchestrator{cfg: &config.Config{Tools: config.ToolsConfig{DeferredSchemas: true, CoreTools: []string{"file_read", "shell", "file_read"}}}}
	o.installDeferredTools(a)
	return a
}

func TestDeferredCorePrefixStableAcrossDiscoveryAndReload(t *testing.T) {
	a := deferredFixture(t)
	request := func(reverse bool) []byte {
		t.Helper()
		defs := []model.ToolDefinition{{Function: model.FunctionDef{Name: "call_tool"}}, {Function: model.FunctionDef{Name: "shell"}}, {Function: model.FunctionDef{Name: "mcp__fixture__read"}}, {Function: model.FunctionDef{Name: "file_read"}}, {Function: model.FunctionDef{Name: "discover_tools"}}}
		if reverse {
			for i, j := 0, len(defs)-1; i < j; i, j = i+1, j-1 {
				defs[i], defs[j] = defs[j], defs[i]
			}
		}
		req := &model.ChatRequest{Tools: defs}
		if err := a.Hooks.Before(context.Background(), &hooks.Event{Type: hooks.EventModelCallBefore, Input: req}); err != nil {
			t.Fatal(err)
		}
		if len(req.Tools) != 4 || req.Tools[0].Function.Name != "file_read" || req.Tools[1].Function.Name != "shell" {
			t.Fatalf("core order: %v", req.Tools)
		}
		data, err := json.Marshal(req.Tools)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	before := string(request(false))
	for i := range 100 {
		a.Tools.Register(&tool.Definition{Name: fmt.Sprintf("mcp__fixture__read%d", i), Permission: tool.PermAllow, Handler: func(context.Context, map[string]any) (any, error) { return "wrapped", nil }})
	}
	if _, err := a.Tools.Execute(context.Background(), "discover_tools", map[string]any{"namespace": "fixture"}); err != nil {
		t.Fatal(err)
	}
	if after := string(request(true)); after != before {
		t.Fatalf("core prefix changed: %s != %s", after, before)
	}
}

func TestDeferredDiscoveryBoundedAndExplicitErrors(t *testing.T) {
	a := deferredFixture(t)
	for i := range 30 {
		a.Tools.Register(&tool.Definition{Name: fmt.Sprintf("mcp__fixture__read%02d", i), Permission: tool.PermAllow, Parameters: map[string]any{"type": "object"}, Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }})
	}
	a.Tools.Register(&tool.Definition{Name: "mcp__fixture__denied", Permission: tool.PermDeny})
	discover := func(args map[string]any) (any, error) {
		return a.Tools.Execute(context.Background(), "discover_tools", args)
	}
	out, err := discover(map[string]any{"namespace": "mcp:fixture"})
	if err != nil {
		t.Fatal(err)
	}
	values := out.(map[string]any)
	if len(values["tools"].([]map[string]any)) != deferredSchemaLimit || values["truncated"] != true {
		t.Fatalf("discovery: %v", out)
	}
	for _, args := range []map[string]any{{"namespace": "unknown"}, {"name": "missing"}, {"name": "mcp__fixture__denied"}, {"query": 123}, {"name": "mcp__fixture__read00", "query": "no match"}} {
		if _, err := discover(args); err == nil {
			t.Fatalf("accepted invalid discovery %v", args)
		}
	}
	out, err = discover(map[string]any{"namespace": "fixture", "query": "not present"})
	if err != nil || len(out.(map[string]any)["tools"].([]map[string]any)) != 0 {
		t.Fatalf("empty search %v %v", out, err)
	}
	out, err = discover(map[string]any{"name": "mcp__fixture__read29"})
	if err != nil || len(out.(map[string]any)["tools"].([]map[string]any)) != 1 {
		t.Fatalf("exact discovery %v %v", out, err)
	}
	pins := a.ContextPinsFn(context.Background())
	if !pins[len(pins)-1].TurnScoped || !strings.Contains(pins[len(pins)-1].Content, "30 available, 1 unavailable") {
		t.Fatalf("catalog: %v", pins)
	}
}

type deferredObservationHook struct {
	before, after int
	result        any
	failure       error
}

func (h *deferredObservationHook) Before(_ context.Context, e *hooks.Event) error {
	if e.Type == hooks.EventToolCallBefore && e.Name == "mcp__fixture__write" {
		h.before++
	}
	return nil
}
func (h *deferredObservationHook) After(_ context.Context, e *hooks.Event) error {
	if e.Type == hooks.EventToolCallAfter && e.Name == "mcp__fixture__write" {
		h.after++
		h.result = e.Output
		return h.failure
	}
	return nil
}

func TestDeferredDispatchRetainsEffectsApprovalWrappersAndErrors(t *testing.T) {
	a := deferredFixture(t)
	denied := errors.New("approval refused")
	observation := errors.New("observation failed")
	childFailure := errors.New("child failed")
	spy := &deferredObservationHook{}
	a.Hooks = append(a.Hooks, spy)
	calls, approvals := 0, 0
	a.Tools.SetApprovalHandler(func(_ context.Context, name string, args map[string]any) (bool, error) {
		approvals++
		if name != "mcp__fixture__write" || args["path"] != "source" {
			t.Fatalf("approval target %s %v", name, args)
		}
		return false, denied
	})
	register := func(failure error) {
		a.Tools.Register(&tool.Definition{Name: "mcp__fixture__write", Permission: tool.PermRequireApproval, ResolveEffects: func(context.Context, map[string]any) ([]tool.Effect, error) {
			return []tool.Effect{tool.EffectDeliveryWrite}, nil
		}, Handler: func(context.Context, map[string]any) (any, error) { calls++; return "wrapped output", failure }})
	}
	register(nil)
	args := map[string]any{"name": "mcp__fixture__write", "arguments": map[string]any{"path": "source"}}
	if _, err := a.Tools.Execute(tool.WithEffectGrant(context.Background(), tool.EffectRead), "call_tool", args); err == nil || approvals != 0 || calls != 0 {
		t.Fatalf("effects bypassed %v approvals=%d calls=%d", err, approvals, calls)
	}
	if _, err := a.Tools.Execute(context.Background(), "call_tool", args); !errors.Is(err, denied) || calls != 0 || spy.after != 1 {
		t.Fatalf("approval bypassed %v calls=%d after=%d", err, calls, spy.after)
	}
	a.Tools.SetApprovalHandler(func(context.Context, string, map[string]any) (bool, error) { return true, nil })
	register(childFailure)
	spy.failure = observation
	out, err := a.Tools.Execute(context.Background(), "call_tool", args)
	if out != "wrapped output" || spy.result != out || calls != 1 || !errors.Is(err, childFailure) || !errors.Is(err, observation) {
		t.Fatalf("lost wrapped output/errors %v %v", out, err)
	}
	for _, name := range []string{"missing", "call_tool", "discover_tools"} {
		if _, err := a.Tools.Execute(context.Background(), "call_tool", map[string]any{"name": name, "arguments": map[string]any{}}); err == nil {
			t.Fatalf("accepted target %q", name)
		}
	}
}
