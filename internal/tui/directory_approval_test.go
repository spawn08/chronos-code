package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestDirectoryApprovalModalAllowsOnlyYesOrNo(t *testing.T) {
	m := newTestAppModel(t)
	m.width = 100
	resp := make(chan approvalDecision, 1)
	m.approval = &pendingApproval{
		toolName: directoryAccessToolName,
		args:     map[string]any{"directory": "/tmp/other", "path": "/tmp/other/a.go", "access": "read"},
		resp:     resp,
	}
	view := m.renderApprovalModal()
	for _, want := range []string{"Access outside the workspace", "/tmp/other", "for this session"} {
		if !strings.Contains(view, want) {
			t.Fatalf("modal missing %q:\n%s", want, view)
		}
	}

	// "a" (always tool) is not an option for directory access.
	_, _ = m.handleApprovalKey(tea.KeyPressMsg{Code: 'a', Text: "a"})
	if m.approval == nil || len(resp) != 0 {
		t.Fatal("'a' must not answer a directory prompt")
	}
	_, _ = m.handleApprovalKey(tea.KeyPressMsg{Code: 'y', Text: "y"})
	if m.approval != nil {
		t.Fatal("'y' must close the prompt")
	}
	if dec := <-resp; !dec.allow {
		t.Fatal("'y' must allow")
	}
}
