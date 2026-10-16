package tui

import (
	"slices"

	"github.com/spawn08/chronos-code/internal/orchestrator"
)

const (
	inlinePlanPreviewBytes = 16 << 10
	inlinePlanPreviewLines = 256
)

type activityRenderKey struct {
	content, args, result, failure, plan, editPreview string
	width                                             int
	expanded, editApplied                             bool
	activity                                          activityKind
}

type activityRenderCache struct {
	key      activityRenderKey
	preview  []string
	rendered string
}

func (m *appModel) renderActivityItem(item *turnItem, content string) string {
	key := activityRenderKey{content: item.content, args: item.args, result: item.result, failure: item.failure, plan: item.plan, editPreview: item.editPreview, width: m.viewport.Width(), expanded: m.toolsExpanded, editApplied: item.editApplied, activity: item.activity}
	if cache := item.activityCache; cache != nil && cache.key == key && slices.Equal(cache.preview, item.preview) {
		return cache.rendered
	}
	line := truncateToWidth(content, m.viewport.Width())
	if item.editApplied && item.editPreview != "" {
		line += "\n" + m.renderToolExcerpt("edit diff (captured replacement):", item.editPreview)
	}
	if item.plan != "" {
		plan := boundedTextTail(item.plan, inlinePlanPreviewBytes, inlinePlanPreviewLines)
		if len(plan) < len(item.plan) {
			line += "\n" + styleDim.Render("    plan preview · /inspect shows the full plan")
		}
		line += "\n" + indentLines(RenderMarkdownLite(plan, max(1, m.viewport.Width()-4)), "    ")
	}
	if !m.toolsExpanded && len(item.preview) > 0 {
		line += "\n" + m.renderToolPreview(item.preview)
	}
	if m.toolsExpanded && item.activity == activityTool {
		if item.args != "" && (!item.editApplied || item.editPreview == "") {
			line += "\n" + m.renderToolExcerptLimit("arguments:", item.args, expandedExcerptBytes, expandedExcerptLines, false)
		}
		if item.result != "" {
			line += "\n" + m.renderToolExcerptLimit("result:", item.result, expandedExcerptBytes, expandedExcerptLines, false)
		}
		if item.failure != "" {
			label := "error:"
			if orchestrator.IsScopedInstructionsNeeded(item.failure) {
				label = "instruction loading:"
			}
			line += "\n" + m.renderToolExcerpt(label, item.failure)
		}
	}
	line = boundedTextTail(line, maxItemRenderBytes, maxViewportLines)
	item.activityCache = &activityRenderCache{key: key, preview: slices.Clone(item.preview), rendered: line}
	return line
}
