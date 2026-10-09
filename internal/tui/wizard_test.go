package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestLoginWizardAcceptsPastedAPIKey(t *testing.T) {
	m := newTestAppModel(t)
	m.wizard = &loginWizard{step: stepProvider, method: "apikey"}
	_, _ = m.advanceWizard("openai")

	_, _ = m.Update(tea.PasteMsg{Content: "sk-pasted"})
	if got := m.wizard.input.Value(); got != "sk-pasted" {
		t.Fatalf("wizard input after paste = %q, want sk-pasted", got)
	}
	if got := m.input.Value(); got != "" {
		t.Fatalf("paste leaked into composer: %q", got)
	}
}

func TestLoginWizardAsksForAzureSettings(t *testing.T) {
	m := newTestAppModel(t)
	m.wizard = &loginWizard{step: stepProvider, method: "apikey"}
	_, _ = m.advanceWizard("azure")
	w := m.wizard
	if len(w.fields) != 4 || !strings.Contains(w.title(), "endpoint") {
		t.Fatalf("azure wizard fields = %d, title = %q", len(w.fields), w.title())
	}

	w.input.SetValue("not a url")
	_, _ = m.submitWizardInput()
	if w.err == "" || len(w.answers) != 0 {
		t.Fatalf("invalid endpoint accepted: answers = %q, err = %q", w.answers, w.err)
	}

	w.input.SetValue("https://res.openai.azure.com")
	_, _ = m.submitWizardInput()
	if len(w.answers) != 1 || !strings.Contains(w.title(), "API key") || w.err != "" {
		t.Fatalf("after endpoint: answers = %q, title = %q, err = %q", w.answers, w.title(), w.err)
	}

	_, _ = m.Update(tea.PasteMsg{Content: "az-key"})
	_, _ = m.submitWizardInput()
	if len(w.answers) != 2 || w.answers[1] != "az-key" || !strings.Contains(w.title(), "Deployment") {
		t.Fatalf("after key: answers = %q, title = %q", w.answers, w.title())
	}

	_, _ = m.submitWizardInput()
	if m.wizard == nil || len(w.answers) != 3 || w.answers[2] != "" || !strings.Contains(w.title(), "API version") {
		t.Fatalf("optional deployment was not skippable: answers = %q", w.answers)
	}
}
