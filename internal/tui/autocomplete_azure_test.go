package tui

import (
	"reflect"
	"testing"

	"github.com/spawn08/chronos-code/internal/modelinfo"
)

func TestInputCompletionsAcceptAzureOpenAIAliases(t *testing.T) {
	models := []string{"anthropic claude-sonnet-4-6", "azure gpt-4o", "azure corp-gpt", "openai gpt-4o"}
	for _, input := range []string{
		"/model azure",
		"/model azure-openai",
		"/model Azure-OpenAI",
		"/model azure openai",
		"/model azure_openai",
		"/model azureopenai",
	} {
		got := inputCompletions(input, nil, nil, nil, nil, nil, models)
		want := []string{"/model azure gpt-4o", "/model azure corp-gpt"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("inputCompletions(%q) = %v, want %v", input, got, want)
		}
	}

	// A model typed after the alias narrows the azure entries.
	got := inputCompletions("/model azure-openai corp", nil, nil, nil, nil, nil, models)
	if want := []string{"/model azure corp-gpt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("alias plus model = %v, want %v", got, want)
	}
}

func TestNormalizeModelQueryLeavesOtherProvidersAlone(t *testing.T) {
	for in, want := range map[string]string{
		"/model openai gpt":         "/model openai gpt",
		"/model azure-openaix":      "/model azure-openaix",
		"/model azure-openai":       "/model azure",
		"/model azure openai gpt-5": "/model azure gpt-5",
	} {
		if got := normalizeModelQuery(in); got != want {
			t.Errorf("normalizeModelQuery(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeLiveModelInfosReplacesProviderButKeepsAzureDeployment(t *testing.T) {
	t.Setenv("AZURE_OPENAI_DEPLOYMENT", "corp-gpt")
	list := []modelinfo.Info{
		{Provider: "anthropic", Model: "claude-a"},
		{Provider: "azure", Model: "corp-gpt"},
		{Provider: "azure", Model: "gpt-4o"},
		{Provider: "openai", Model: "gpt-4o"},
	}
	live := map[string][]modelinfo.Info{
		"azure": {{Provider: "azure", Model: "gpt-5.1"}, {Provider: "azure", Model: "corp-gpt"}},
	}
	got := mergeLiveModelInfos(list, live)
	var names []string
	for _, info := range got {
		names = append(names, info.Provider+" "+info.Model)
	}
	want := []string{"anthropic claude-a", "azure corp-gpt", "openai gpt-4o", "azure gpt-5.1"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("merged = %v, want %v", names, want)
	}
	if same := mergeLiveModelInfos(list, nil); !reflect.DeepEqual(same, list) {
		t.Fatalf("no live data must return the list unchanged, got %v", same)
	}
}

func TestLiveModelsReachCompletions(t *testing.T) {
	m := newTestAppModel(t)
	m.input.SetValue("/model azure-openai live")
	if got := m.inputCompletions(); len(got) != 0 {
		t.Fatalf("before live fetch completions = %v, want none", got)
	}
	m.Update(modelPickerLiveMsg{results: []providerModelsResult{
		{provider: "azure", ok: true, models: []modelinfo.Info{{Provider: "azure", Model: "live-deployment"}}},
		{provider: "openai", ok: false},
	}})
	got := m.inputCompletions()
	if want := []string{"/model azure live-deployment"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after live fetch completions = %v, want %v", got, want)
	}
}
