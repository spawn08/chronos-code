package tui

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	chronosstream "github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

func TestFormatShellOutputShowsTailAndFailsOnNonZeroExit(t *testing.T) {
	stdout := "line 1\nline 2\nline 3\nline 4\nline 5\n"
	out := formatToolOutput("shell", map[string]any{"stdout": stdout, "stderr": "boom\n", "exit_code": 2}, "")
	if !out.failed || out.summary != "exit 2" {
		t.Fatalf("failed=%v summary=%q", out.failed, out.summary)
	}
	if !strings.Contains(out.text, "line 1") || !strings.Contains(out.text, "boom") || !strings.Contains(out.text, "[exit code 2]") {
		t.Fatalf("text = %q", out.text)
	}
	want := []string{"… +2 lines", "line 3", "line 4", "line 5", "boom"}
	if strings.Join(out.preview, "|") != strings.Join(want, "|") {
		t.Fatalf("preview = %q, want %q", out.preview, want)
	}
	if ok := formatToolOutput("shell", map[string]any{"stdout": "", "stderr": "", "exit_code": 0}, ""); ok.failed || ok.text != "(no output)" {
		t.Fatalf("empty success = %+v", ok)
	}
}

func TestFormatToolOutputIsReadableNotJSON(t *testing.T) {
	root := "/repo"
	cases := []struct {
		name, tool string
		result     any
		want       []string
		summary    string
	}{
		{"grep typed slice", "file_grep", map[string]any{"matches": []map[string]any{{"file": "/repo/a/b.go", "line_number": 12, "content": "func X()"}}, "truncated": true},
			[]string{"a/b.go:12: func X()"}, "1 match · truncated"},
		{"glob", "file_glob", map[string]any{"matches": []string{"/repo/x.go", "/repo/y/z.go"}}, []string{"x.go", "y/z.go"}, "2 files"},
		{"list", "file_list", map[string]any{"entries": []map[string]any{{"name": "cmd", "is_dir": true}, {"name": "go.mod", "is_dir": false, "size": int64(2048)}}},
			[]string{"cmd/", "go.mod  2.0 KiB"}, "2 entries"},
		{"read range", "file_read", map[string]any{"content": "a\nb", "start_line": 10, "end_line": 11, "total_lines": 40}, []string{"a\nb"}, "lines 10-11 of 40"},
		{"write edit", "file_write", map[string]any{"path": "/repo/main.go", "bytes_written": 10, "replacements": 1}, []string{"main.go"}, "1 replacement"},
		{"generic", "workspace_info", map[string]any{"root": "/repo", "languages": []any{"go", "ts"}, "files": float64(42)},
			[]string{"files: 42", "languages:\n  - go\n  - ts", "root: /repo"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := formatToolOutput(tc.tool, tc.result, root)
			for _, want := range tc.want {
				if !strings.Contains(out.text, want) {
					t.Fatalf("text %q missing %q", out.text, want)
				}
			}
			if strings.Contains(out.text, `{"`) || strings.Contains(out.text, `":`) {
				t.Fatalf("text still looks like JSON: %q", out.text)
			}
			if out.summary != tc.summary {
				t.Fatalf("summary = %q, want %q", out.summary, tc.summary)
			}
		})
	}
	if out := formatToolOutput("file_read", "whole file text", ""); len(out.preview) != 0 {
		t.Fatal("file reads must not flood the collapsed transcript")
	}
}

func TestActivityShowsRealOutputInsteadOfCompressedJSON(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.sending = true
	stdout := strings.Repeat("ok  pkg/line\n", 200) + "FAIL pkg/broken\n"
	events := []chronosstream.Event{
		{Type: chronosstream.EventToolCall, Data: map[string]any{"id": "sh-1", "agent": "coder", "tool": "shell", "args": map[string]any{"command": "go test ./..."}}},
		{Type: orchestrator.EventToolOutput, Data: map[string]any{"id": "sh-1", "agent": "coder", "tool": "shell", "output": map[string]any{"stdout": stdout, "stderr": "", "exit_code": 1}}},
		{Type: chronosstream.EventToolResult, Data: map[string]any{"id": "sh-1", "agent": "coder", "tool": "shell", "result": map[string]any{
			"compressed": true, "preview": `{"exit_code":1,"stderr":"","stdout":"ok  pkg/line\n...`, "full_size_bytes": 3000, "storage_key": "k"}}},
	}
	for _, event := range events {
		m.handleActivity(activityMsg{ctx: m.ctx, event: event})
	}
	got := ansi.Strip(m.renderTranscript())
	for _, want := range []string{"✗ @coder shell · failed", "go test ./...", "exit 1", "⎿", "FAIL pkg/broken", "… +197 lines"} {
		if !strings.Contains(got, want) {
			t.Fatalf("collapsed transcript missing %q:\n%s", want, got)
		}
	}
	for _, unwanted := range []string{`{"`, "compressed", "storage_key"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("collapsed transcript shows %q:\n%s", unwanted, got)
		}
	}
	m.Update(tea.KeyPressMsg{Code: 'o', Mod: tea.ModCtrl})
	got = ansi.Strip(m.renderTranscript())
	for _, want := range []string{"arguments:", "$ go test ./...", "result:", "ok  pkg/line", "/inspect for more"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expanded transcript missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `{"`) {
		t.Fatalf("expanded transcript shows JSON:\n%s", got)
	}
}

func TestWholeFileWriteHasDiffPreview(t *testing.T) {
	preview := editDiffPreview(map[string]any{"path": "main.go", "content": "package main\n", "old_content": ""})
	if !strings.Contains(preview, "+++ whole file") || !strings.Contains(preview, "+ package main") {
		t.Fatalf("whole-file write preview = %q", preview)
	}
	if modal := ansi.Strip(RenderFileWriteDiff(map[string]any{"path": "main.go", "content": "package main"})); !strings.Contains(modal, "+ package main") {
		t.Fatalf("approval diff lost content: %q", modal)
	}
}

func TestExitPlanModeShowsPlanAndApprovedPlanStartsImplementation(t *testing.T) {
	m := newTestAppModelWith(t, func(cfg *config.Config) {
		cfg.Agents[0].Tools = []agent.ToolConfig{{Name: "file_write"}}
	})
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	registry := m.orch.ActiveAgent().Tools
	if _, ok := registry.Get(orchestrator.ExitPlanModeToolName); !ok {
		t.Fatal("primary agent must offer exit_plan_mode")
	}
	m.sending = true
	m.turnID = 1
	plan := "## Plan\n1. Update the parser\n2. Add tests"
	m.handleActivity(activityMsg{turnID: 1, ctx: m.ctx, event: chronosstream.Event{Type: chronosstream.EventToolCall, Data: map[string]any{
		"id": "plan-1", "agent": "coder", "tool": orchestrator.ExitPlanModeToolName, "args": map[string]any{"plan": plan}}}})
	got := ansi.Strip(m.renderTranscript())
	for _, want := range []string{"exit_plan_mode", "Update the parser", "Add tests"} {
		if !strings.Contains(got, want) {
			t.Fatalf("plan not visible, missing %q:\n%s", want, got)
		}
	}

	// Headless-style approval (no handler) marks the plan approved.
	m.orch.SetPlanMode(true)
	if _, err := registry.Execute(context.Background(), orchestrator.ExitPlanModeToolName, map[string]any{"plan": plan}); err != nil {
		t.Fatal(err)
	}
	m.finalizeTurn(nil)
	if !m.sending || !strings.Contains(m.activeRequest, "Update the parser") || m.orch.PlanMode() {
		t.Fatalf("approved plan did not start implementation: sending=%v request=%q plan=%v", m.sending, m.activeRequest, m.orch.PlanMode())
	}
	if got := ansi.Strip(m.renderTranscript()); !strings.Contains(got, "plan approved · plan mode off · implementing") {
		t.Fatalf("implementation handoff not shown:\n%s", got)
	}
	m.turnCancel()
}

func TestRetryEventReachesTranscript(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.sending = true
	ch := make(chan chronosstream.Event, 1)
	ch <- chronosstream.Event{Type: chronosstream.EventCustom, Data: map[string]any{"type": "api_retry", "agent": "coder", "message": "model API rate limited · retrying in 2s (retry 1)"}}
	msg, ok := listenActivity(m.ctx, 0, ch)().(activityMsg)
	if !ok {
		t.Fatal("listener dropped the api_retry event")
	}
	m.handleActivity(msg)
	if got := ansi.Strip(m.renderTranscript()); !strings.Contains(got, "↻ model API rate limited · retrying in 2s") {
		t.Fatalf("retry not shown:\n%s", got)
	}
}
