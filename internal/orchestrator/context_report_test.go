package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

func TestContextReportStableMetadataOnlyContract(t *testing.T) {
	collector := newContextReportCollector()
	ctx := withContextReportCollector(context.Background(), collector)
	secret := "sk-secret-memory-body"
	contextSourceSelected(ctx, ContextSourceMemory, 2, len(secret), true)
	contextSourceOmitted(ctx, ContextSourceDiagnostics, "failure includes "+secret)

	report := collector.report()
	wantKinds := []ContextSourceKind{
		ContextSourceSessionSummaries, ContextSourceMemory, ContextSourceLearnedPattern,
		ContextSourceProjectDocs, ContextSourceSkills, ContextSourceDiagnostics,
		ContextSourceGraphPrediction, ContextSourceUserHook,
	}
	gotKinds := make([]ContextSourceKind, len(report.Sources))
	for i := range report.Sources {
		gotKinds[i] = report.Sources[i].Kind
	}
	if !reflect.DeepEqual(gotKinds, wantKinds) {
		t.Fatalf("source order = %#v, want %#v", gotKinds, wantKinds)
	}
	if report.TotalCount != 2 || report.TotalBytes != len(secret) || !report.Truncated || report.BudgetBytes <= 0 {
		t.Fatalf("report totals = %#v", report)
	}
	if got := contextSource(report, ContextSourceDiagnostics).OmissionReason; got != ContextOmittedSourceError {
		t.Fatalf("unsafe omission reason normalized to %q", got)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), "body") {
		t.Fatalf("report leaked context content: %s", encoded)
	}
}

func TestContextReportSnapshotDoesNotAliasCollector(t *testing.T) {
	collector := newContextReportCollector()
	first := collector.report()
	first.Sources[0].Title = "mutated"
	if second := collector.report(); second.Sources[0].Title == "mutated" {
		t.Fatal("report snapshot aliases collector state")
	}
}

func TestExecuteContextReportBlockingStreamingParityAndRedaction(t *testing.T) {
	const secret = "sk-live-secret memory body hidden prompt tool_args=token"
	provider := &executionTestProvider{name: "coder", modelID: "test"}
	a := &agent.Agent{ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine()}
	a.ContextPinsFn = func(ctx context.Context) []model.Message {
		for _, kind := range []ContextSourceKind{
			ContextSourceSessionSummaries, ContextSourceMemory, ContextSourceLearnedPattern,
			ContextSourceProjectDocs, ContextSourceSkills, ContextSourceDiagnostics,
			ContextSourceGraphPrediction, ContextSourceUserHook,
		} {
			contextSourceSelected(ctx, kind, 1, len(secret), false)
		}
		return []model.Message{{Role: model.RoleSystem, Content: secret}}
	}
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder"}

	blocking, err := orch.Execute(context.Background(), ExecutionRequest{Message: "inspect"})
	if err != nil {
		t.Fatalf("blocking Execute() error = %v", err)
	}
	streaming, err := orch.Execute(context.Background(), ExecutionRequest{Message: "inspect", Mode: ExecutionStreaming})
	if err != nil {
		t.Fatalf("streaming Execute() error = %v", err)
	}
	for range streaming.Stream {
	}
	if !reflect.DeepEqual(blocking.ContextReport, streaming.ContextReport) {
		t.Fatalf("blocking report %#v differs from streaming report %#v", blocking.ContextReport, streaming.ContextReport)
	}
	if blocking.ContextReport.TotalCount != len(contextSourceDefinitions) || blocking.ContextReport.TotalBytes != len(contextSourceDefinitions)*len(secret) {
		t.Fatalf("report totals = %#v", blocking.ContextReport)
	}
	encoded, err := json.Marshal(blocking.ContextReport)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{secret, "sk-live-secret", "memory body", "hidden prompt", "tool_args", "token"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("context report contains forbidden %q: %s", forbidden, encoded)
		}
	}
}

func TestExecuteContextReportRecordsOptionalDiagnosticFailure(t *testing.T) {
	provider := &executionTestProvider{name: "coder", modelID: "test"}
	a := &agent.Agent{ID: "coder", Model: provider, Tools: tool.NewRegistry(), Guardrails: guardrails.NewEngine()}
	installLSPTools("/workspace", []string{"main.go"}, map[string]*agent.Agent{"coder": a}, []*tool.Definition{{
		Name: "lsp_diagnostics",
		Handler: func(context.Context, map[string]any) (any, error) {
			return nil, errors.New("secret diagnostic server credential")
		},
	}})
	orch := &Orchestrator{agents: map[string]*agent.Agent{"coder": a}, active: "coder"}

	result, err := orch.Execute(context.Background(), ExecutionRequest{Message: "inspect main.go"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	source := contextSource(result.ContextReport, ContextSourceDiagnostics)
	if source.OmissionReason != ContextOmittedSourceError || source.SelectedCount != 0 || source.Bytes != 0 {
		t.Fatalf("diagnostics report = %#v", source)
	}
	encoded, err := json.Marshal(result.ContextReport)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "credential") {
		t.Fatalf("report leaked source error: %s", encoded)
	}
}

func contextSource(report ContextReport, kind ContextSourceKind) ContextSourceReport {
	for _, source := range report.Sources {
		if source.Kind == kind {
			return source
		}
	}
	return ContextSourceReport{}
}
