package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/spawn08/chronos/engine/model"
	chronosstream "github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/storage"
)

type inspectionSessionLister struct {
	list func(context.Context, string, int, int) ([]*storage.Session, error)
}

func (l inspectionSessionLister) List(ctx context.Context, agent string, limit, offset int) ([]*storage.Session, error) {
	return l.list(ctx, agent, limit, offset)
}

func TestInspectionSessionPickerDeferredFilteringAndStaleResults(t *testing.T) {
	m := &appModel{turnID: 3}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &picker{isSessionPicker: true, filterable: true, ctx: ctx, cancel: cancel, agentID: "coder", details: make(map[string]string)}
	m.picker = p
	called := false
	loader := inspectionSessionLister{list: func(_ context.Context, agent string, limit, offset int) ([]*storage.Session, error) {
		called = true
		if agent != "coder" || limit != sessionPageSize || offset != 0 {
			t.Fatalf("unexpected list scope: %s %d %d", agent, limit, offset)
		}
		return []*storage.Session{
			{ID: "session-one", Status: "completed", Metadata: map[string]any{"title": "Fix renderer"}},
			{ID: "session-two", Status: "paused", Metadata: map[string]any{"title": "Graph freshness"}},
		}, nil
	}}
	cmd := loadSessionPageCmd(p, m.turnID, loader)
	if called || !p.loading {
		t.Fatal("session listing ran on Update")
	}
	p.filter = "GRAPH"
	msg := cmd()
	m.Update(msg)
	if p.loading || len(p.items) != 1 || p.items[0].value != "/resume session-two" {
		t.Fatalf("filter was lost during async load: %+v", p)
	}
	p.filter = "completed"
	p.applyFilter()
	if len(p.items) != 1 || p.items[0].value != "/resume session-one" {
		t.Fatal("session status is not searchable")
	}
	m.picker = &picker{heading: "replacement"}
	m.Update(msg)
	if m.picker.heading != "replacement" || len(m.picker.items) != 0 {
		t.Fatal("stale picker result replaced new overlay")
	}
	m.picker = p
	m.turnID++
	before := len(p.all)
	m.Update(msg)
	if len(p.all) != before {
		t.Fatal("stale turn result appended sessions")
	}
}

func TestInspectionSessionPickerCancelAndPaging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p := &picker{ctx: ctx, cancel: cancel, isSessionPicker: true, details: make(map[string]string)}
	m := &appModel{picker: p}
	called := false
	loader := inspectionSessionLister{list: func(_ context.Context, _ string, _, offset int) ([]*storage.Session, error) {
		called = true
		rows := make([]*storage.Session, sessionPageSize)
		for i := range rows {
			rows[i] = &storage.Session{ID: fmt.Sprintf("session-%d", offset+i)}
		}
		return rows, nil
	}}
	m.Update(loadSessionPageCmd(p, 0, loader)())
	if !p.more || p.offset != sessionPageSize || len(p.all) != sessionPageSize {
		t.Fatalf("first page state: %+v", p)
	}
	m.Update(loadSessionPageCmd(p, 0, loader)())
	if p.offset != 2*sessionPageSize || p.all[sessionPageSize].value != "/resume session-100" {
		t.Fatal("second page did not use an offset")
	}
	called = false
	cmd := loadSessionPageCmd(p, 0, loader)
	m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	msg := cmd().(sessionPickerMsg)
	if called || !errors.Is(msg.err, context.Canceled) {
		t.Fatal("dismissed session picker performed IO")
	}
}

func TestInspectionSessionLoadCancellationDoesNotBlockUI(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &picker{ctx: ctx, cancel: cancel, isSessionPicker: true}
	m := &appModel{picker: p}
	started := make(chan struct{})
	loader := inspectionSessionLister{list: func(ctx context.Context, _ string, _, _ int) ([]*storage.Session, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	cmd := loadSessionPageCmd(p, 0, loader)
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("session command never started")
	}
	m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	select {
	case msg := <-done:
		m.Update(msg)
		if m.picker != nil {
			t.Fatal("canceled load reopened picker")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("session command ignored cancellation")
	}
}

func TestInspectionSessionLoadEmptyAndErrorAreVisible(t *testing.T) {
	for _, failure := range []error{nil, fmt.Errorf("storage unavailable")} {
		p := &picker{ctx: context.Background(), isSessionPicker: true, details: make(map[string]string)}
		m := &appModel{picker: p}
		loader := inspectionSessionLister{list: func(context.Context, string, int, int) ([]*storage.Session, error) { return nil, failure }}
		m.Update(loadSessionPageCmd(p, 0, loader)())
		want := "no sessions"
		if failure != nil {
			want = "storage unavailable"
		}
		if p.loading || !strings.Contains(p.View(3), want) {
			t.Fatalf("load outcome not visible: %s", p.View(3))
		}
	}
}

func TestInspectionSessionPickerSelectResumesAsCommand(t *testing.T) {
	m := newTestAppModel(t)
	first := m.orch.CurrentSessionID()
	const selected = "inspection-target-session"
	if err := m.orch.SessionManager().Ensure(m.ctx, selected, m.orch.ActiveID()); err != nil {
		t.Fatal(err)
	}
	cmd := m.openSessionPicker()
	m.picker.filter = selected
	m.Update(cmd())
	if len(m.picker.items) != 1 {
		t.Fatalf("session picker did not find target: %+v", m.picker.items)
	}
	m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if m.inspection == nil || !strings.Contains(m.inspection.content, selected) {
		t.Fatal("Tab did not open full session metadata")
	}
	m.handleInspectionKey(tea.KeyPressMsg{Code: tea.KeyEsc})
	_, cmd = m.handlePickerKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil || m.orch.CurrentSessionID() != first {
		t.Fatal("picker resume was not deferred")
	}
	m.Update(cmd())
	if m.sending || m.orch.CurrentSessionID() != selected {
		t.Fatal("picker selection did not resume target")
	}
}

func TestInspectionToolDetailsScrollAndWidths(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := newTestAppModel(t)
			m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
			m.orch.SetPlanMode(true)
			m.sending = true
			longBody := strings.Repeat("unchopped file content 世界\n", 200) + "final-argument-marker"
			for _, event := range []chronosstream.Event{
				{Type: chronosstream.EventToolCall, Data: map[string]any{"id": "write-1", "agent": "coder", "tool": "file_write", "args": map[string]any{"path": "long/path/source.go", "new_content": longBody}}},
				{Type: chronosstream.EventToolResult, Data: map[string]any{"id": "write-1", "agent": "coder", "tool": "file_write", "duration_ms": 123, "result": strings.Repeat("result line\n", 100) + "final-result-marker"}},
			} {
				m.handleActivity(activityMsg{ctx: m.ctx, event: event})
			}
			m.input.SetValue("/inspect changes")
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			if m.inspection == nil || !m.sending || m.turnInterrupted {
				t.Fatal("inspection interrupted live turn")
			}
			for _, want := range []string{"final-argument-marker", "final-result-marker", "write-1", "duration: 123ms", "not a working-tree diff"} {
				if !strings.Contains(m.inspection.content, want) {
					t.Fatalf("full detail missing %q", want)
				}
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyPgDown})
			if m.inspection.viewport.YOffset() == 0 {
				t.Fatal("detail page-down did not scroll")
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyEnd})
			if !strings.Contains(m.inspection.viewport.View(), "final-result-marker") {
				t.Fatal("full result is not reachable by scrolling")
			}
			view := m.View()
			if !view.AltScreen || view.MouseMode != tea.MouseModeCellMotion || lipgloss.Width(view.Content) > width || lipgloss.Height(view.Content) > 24 {
				t.Fatalf("inspection geometry/defaults: width=%d height=%d", lipgloss.Width(view.Content), lipgloss.Height(view.Content))
			}
			if !strings.Contains(view.Content, "plan") || !strings.Contains(view.Content, "read-only") {
				t.Fatal("inspection hides plan/read-only indicators")
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyHome})
			if m.inspection.viewport.YOffset() != 0 {
				t.Fatal("Home did not return to start")
			}
			m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
			if m.inspection != nil || !m.sending || !m.orch.PlanMode() {
				t.Fatal("Esc changed live turn/plan state")
			}
		})
	}
}

func TestInspectionContextSnapshotAndClipboard(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 40, Height: 12})
	m.captureExecutionMetadata(testContextReport(), nil)
	m.inspectTurn("context")
	if !strings.Contains(m.inspection.content, "[graph_prediction]") || !strings.Contains(m.inspection.content, "memory intent: none") {
		t.Fatal("context inspector lost source receipts")
	}
	want := m.inspection.content
	var copied string
	m.clipboardWrite = func(s string) error { copied = s; return nil }
	_, cmd := m.handleInspectionKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift})
	if cmd == nil {
		t.Fatal("inspection copy shortcut missing")
	}
	m.Update(cmd())
	if copied != want {
		t.Fatal("inspection copy used truncated viewport")
	}
}

func TestInspectionIndividualFieldsRemainReachablePastOverviewCap(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.lastTurnItems = []turnItem{
		{kind: turnItemText, content: strings.Repeat("large overview\n", 100000)},
		{kind: turnItemActivity, toolName: "file_read", callID: "last-call", content: "file_read done", args: `{"path":"source.go"}`, result: "LAST RESULT MUST REMAIN REACHABLE"},
	}
	m.inspectTurn("")
	if !strings.Contains(m.inspection.content, "inspection capped") {
		t.Fatal("overview cap was not explained")
	}
	for range m.inspection.entries {
		m.handleInspectionKey(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	if m.inspection.content != "LAST RESULT MUST REMAIN REACHABLE" {
		t.Fatal("overview clipping made later tool results inaccessible")
	}
}

func TestInspectionCapturedDetailBudgetDoesNotRetainFullSummaryArgs(t *testing.T) {
	m := &appModel{}
	large := strings.Repeat("x", maxInspectionBytes)
	for i := 0; i < 4; i++ {
		if got := m.captureActivityValue(large); got != large {
			t.Fatal("capture clipped detail before aggregate budget")
		}
	}
	if got := m.captureActivityValue("beyond budget"); !strings.Contains(got, "detail capture budget exhausted") || m.activityDetailBytes > maxTranscriptBytes {
		t.Fatal("aggregate detail capture is unbounded")
	}
	args := activitySummaryArgs(map[string]any{"new_content": large, "path": "source.go"}).(map[string]any)
	if len(args["new_content"].(string)) > 1030 || args["path"] != "source.go" {
		t.Fatal("lifecycle summaries retained bulk edit arguments")
	}
}

func TestInspectionTypedActivitiesDoNotInflateToolCount(t *testing.T) {
	m := newTestAppModel(t)
	m.sending = true
	m.handleActivity(activityMsg{ctx: m.ctx, event: chronosstream.Event{Type: chronosstream.EventModelCall, Data: map[string]any{"agent": "coder", "model": "test-model"}}})
	for _, id := range []string{"a", "b"} {
		m.handleActivity(activityMsg{ctx: m.ctx, event: chronosstream.Event{Type: chronosstream.EventToolCall, Data: map[string]any{"agent": "coder", "id": id, "tool": "file_read"}}})
	}
	m.captureExecutionMetadata(testContextReport(), nil)
	m.handleActivity(activityMsg{ctx: m.ctx, event: chronosstream.Event{Type: chronosstream.EventCustom, Data: map[string]any{"type": "api_retry", "message": "retrying model call"}}})
	got := m.renderTurnItems()
	for _, want := range []string{"2 tool calls", "model", "context ·", "retrying model call"} {
		if !strings.Contains(got, want) {
			t.Fatalf("typed activity output missing %q: %s", want, got)
		}
	}
	if strings.Contains(got, "5 tool calls") || m.activeTurnItems[0].activity != activityModel || m.activeTurnItems[len(m.activeTurnItems)-1].activity != activityRetry {
		t.Fatal("non-tool events were labeled as tools")
	}
}

func TestInspectionTranscriptBoundedBeforeJoining(t *testing.T) {
	m := newTestAppModel(t)
	m.appendBlock("old-prefix\n" + strings.Repeat("history line\n", 250000))
	got := m.renderTranscript()
	if len(got) > maxRenderBytes || strings.Count(got, "\n") >= maxViewportLines || len(m.finalizedText) > maxRenderBytes || strings.Contains(got, "old-prefix") {
		t.Fatalf("finalized render is unbounded: output=%d cache=%d", len(got), len(m.finalizedText))
	}
	// A cache hit on an idle frame must not allocate/copy the multi-MiB history.
	if allocations := testing.AllocsPerRun(20, func() { _ = m.renderTranscript() }); allocations > 0 {
		t.Fatalf("cached transcript allocates: %f", allocations)
	}
	m.sending = true
	m.appendTurnText("START\n")
	for i := 0; i < 10000; i++ {
		m.appendTurnText("stream chunk content\n")
	}
	m.appendTurnText("END")
	if len(m.activeTurnItems) != 1 || m.activeTurnItems[0].text == nil {
		t.Fatal("stream text is not builder-backed")
	}
	got = m.renderTranscript()
	if len(got) > maxRenderBytes || len(m.activeTurnItems[0].rendered) > 2*maxItemRenderBytes || !strings.Contains(got, "END") {
		t.Fatalf("active render is unbounded: %d", len(got))
	}
	m.finalizeTurn(nil)
	copied, _, err := m.copyText("")
	if err != nil || !strings.HasPrefix(copied, "START\n") || !strings.HasSuffix(copied, "END") || len(copied) < 200000 {
		t.Fatalf("full completed response copy was clipped: %d, %v", len(copied), err)
	}
	all, _, err := m.copyText("all")
	if err != nil || !strings.Contains(all, "old-prefix") || !strings.Contains(all, "START\n") {
		t.Fatal("explicit copy-all used bounded render cache")
	}
}

func TestInspectionResizeReflowsRawUserAndAssistant(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	user := strings.Repeat("user instruction ", 15)
	answer := strings.Repeat("assistant explanation ", 15) + "\n```go\n" + strings.Repeat("x", 140) + "\n```"
	m.appendUserTurn(user)
	m.sending = true
	m.appendTurnText(answer)
	m.finalizeTurn(nil)
	m.sending = true // Reflow old blocks while a later turn is live.
	for _, width := range []int{40, 80, 120} {
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		if got := m.renderTranscript(); lipgloss.Width(got) > width || !strings.Contains(got, "user instruction") || !strings.Contains(got, "assistant") {
			t.Fatalf("raw blocks did not reflow to %d: %s", width, got)
		}
	}
	code, _, err := m.copyText("code")
	if err != nil || code != strings.Repeat("x", 140) {
		t.Fatalf("resize clipped raw code copy: %q, %v", code, err)
	}
}

func TestInspectionRawReflowSourcesAreBounded(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	message := strings.Repeat("user input\n", 100000)
	for i := 0; i < 6; i++ {
		m.appendUserTurn(message)
	}
	if m.rawBlockBytes > maxTranscriptBytes || m.blockBytes > maxTranscriptBytes || m.blockSources[0] != nil {
		t.Fatalf("raw/rendered sources not bounded: raw=%d rendered=%d", m.rawBlockBytes, m.blockBytes)
	}
	if source := m.blockSources[len(m.blockSources)-1]; source == nil || source.user != message {
		t.Fatal("newest original user text was not retained for reflow/copy")
	}
}

func TestInspectionNewTurnSnapshotSurvivesOldBlockEviction(t *testing.T) {
	m := newTestAppModel(t)
	m.appendBlock(strings.Repeat("x", maxTranscriptBytes))
	m.hasLastTurn, m.lastTurnBlockIdx = true, 0
	m.sending = true
	m.appendTurnText("new assistant response")
	m.finalizeTurn(nil)
	if len(m.lastTurnItems) != 1 || m.lastTurnItems[0].content != "new assistant response" || len(m.blocks) != 1 {
		t.Fatal("evicting old transcript cleared new structured turn")
	}
}

func TestInspectionBudgetRecoveryNeverClaimsAutomaticCompaction(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("maximum context length exceeded"),
		&model.APIError{StatusCode: 413, Body: "request_too_large"},
	} {
		got := classifyErrorMessage(err)
		if !strings.Contains(got, "/compact") || !strings.Contains(got, "/clear") || strings.Contains(got, "Compacting session") {
			t.Fatalf("misleading recovery message: %s", got)
		}
	}
}

func TestInspectionSessionPickerFitsWidths(t *testing.T) {
	for _, width := range []int{40, 80, 120} {
		m := newTestAppModel(t)
		m.Update(tea.WindowSizeMsg{Width: width, Height: 24})
		m.picker = &picker{heading: "Sessions:", isSessionPicker: true, filterable: true, more: true}
		for i := 0; i < 100; i++ {
			m.picker.items = append(m.picker.items, wizardItem{label: fmt.Sprintf("session-%d %s", i, strings.Repeat("long title ", 20)), hint: time.Now().Format(time.RFC3339)})
		}
		m.resizeViewport()
		view := m.View().Content
		if lipgloss.Width(view) > width || lipgloss.Height(view) > 24 {
			t.Fatalf("session picker overflows %d: %dx%d (rows=%d modal=%d viewport=%d)\n%s", width, lipgloss.Width(view), lipgloss.Height(view), m.pickerVisibleRows(), lipgloss.Height(m.bottomView), m.viewport.Height(), view)
		}
	}
}
