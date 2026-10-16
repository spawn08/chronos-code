package tui

import (
	"fmt"
	"strings"
	"testing"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestActivityRenderCacheInvalidatesVisibleChanges(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m.toolsExpanded = true
	item := turnItem{kind: turnItemActivity, activity: activityTool, content: "file_read · done", args: "a", result: "first result"}
	first := m.renderOneItem(&item)
	cache := item.activityCache
	if got := m.renderOneItem(&item); got != first || item.activityCache != cache {
		t.Fatal("unchanged activity was rerendered")
	}
	item.result = "second result"
	second := m.renderOneItem(&item)
	if second == first || item.activityCache == cache || !strings.Contains(second, "second result") {
		t.Fatal("result change left stale activity")
	}
	m.toolsExpanded = false
	item.preview = []string{"old preview"}
	first = m.renderOneItem(&item)
	item.preview[0] = "new preview"
	second = m.renderOneItem(&item)
	if first == second || !strings.Contains(second, "new preview") {
		t.Fatal("in-place preview change left stale activity")
	}
	cache = item.activityCache
	m.viewport.SetWidth(60)
	m.renderOneItem(&item)
	if item.activityCache == cache {
		t.Fatal("width change left stale activity")
	}
	item.editApplied = true
	item.editPreview = "replacement"
	if got := m.renderOneItem(&item); !strings.Contains(got, "replacement") {
		t.Fatal("edit preview left stale activity")
	}
}

func TestInlinePlanPreviewIsBoundedAndFullPlanRetained(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	plan := strings.Repeat("Implementation step with detailed verification.\n", 30000) + "Final required step"
	item := turnItem{kind: turnItemActivity, activity: activityTool, content: "exit_plan_mode", plan: plan}
	view := ansi.Strip(m.renderOneItem(&item))
	if len(view) > maxItemRenderBytes || strings.Count(view, "\n") >= maxViewportLines {
		t.Fatal("inline plan rendering exceeded its bound")
	}
	if item.plan != plan || !strings.Contains(view, "Final required step") {
		t.Fatal("full plan or visible tail was lost")
	}
	cache := item.activityCache
	m.renderOneItem(&item)
	if cache != item.activityCache {
		t.Fatal("unchanged plan was rerendered")
	}
}

func TestExpandedToolsRenderOnlyTheRetainedTail(t *testing.T) {
	m := newTestAppModel(t)
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m.toolsExpanded = true
	items := expandedBenchmarkItems(100)
	view := m.renderItemList(items)
	rendered := 0
	for _, item := range items {
		if item.activityCache != nil {
			rendered++
		}
	}
	if rendered >= len(items) || items[len(items)-1].activityCache == nil {
		t.Fatalf("rendered %d of %d items", rendered, len(items))
	}
	if !strings.Contains(view, "result for tool 99") || len(view) > maxRenderBytes || strings.Count(view, "\n") >= maxViewportLines {
		t.Fatal("expanded tail was not bounded or newest output was lost")
	}
}

func expandedBenchmarkItems(n int) []turnItem {
	items := make([]turnItem, n)
	for i := range items {
		result := fmt.Sprintf("result for tool %d\n", i) + strings.Repeat("readable result line with words\n", 40)
		items[i] = turnItem{kind: turnItemActivity, activity: activityTool, content: fmt.Sprintf("file_read %d · done", i), args: "source.go", result: result}
	}
	return items
}

var activityBenchmarkResult string

func BenchmarkExpandedActivityRendering(b *testing.B) {
	for _, n := range []int{10, 50, 100} {
		for _, cached := range []bool{false, true} {
			b.Run(fmt.Sprintf("tools-%d/cached-%t", n, cached), func(b *testing.B) {
				m := &appModel{toolsExpanded: true}
				m.viewport = viewport.New()
				m.viewport.SetWidth(120)
				items := expandedBenchmarkItems(n)
				if cached {
					m.renderItemList(items)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if !cached {
						for j := range items {
							items[j].activityCache = nil
						}
					}
					activityBenchmarkResult = m.renderItemList(items)
				}
			})
		}
	}
}

func BenchmarkInlinePlanPreview(b *testing.B) {
	m := &appModel{}
	m.viewport = viewport.New()
	m.viewport.SetWidth(120)
	item := turnItem{kind: turnItemActivity, activity: activityTool, content: "exit_plan_mode", plan: strings.Repeat("implementation step with words\n", 35000)}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		item.activityCache = nil
		activityBenchmarkResult = m.renderOneItem(&item)
	}
}
