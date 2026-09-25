package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

var (
	reBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reItalic     = regexp.MustCompile(`_(.+?)_`)
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	reHeader     = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet     = regexp.MustCompile(`^(\s*)([-*]|\d+\.)\s+(.*)$`)
	reBlockquote = regexp.MustCompile(`^>\s?(.*)$`)
)

// Fix Issue 3: pre-allocated base styles instead of lipgloss.NewStyle() per call.
// lipgloss.Style is a value type — calling .Width() / .MaxWidth() on these returns
// a new copy without mutating the base, so sharing them across concurrent calls is safe.
var truncateBaseStyle = lipgloss.NewStyle()

// Inspection is a read-only snapshot. Its viewport owns scrolling separately
// from the transcript, so inspecting a running turn never jumps live output.
type inspectionOverlay struct {
	title    string
	content  string
	viewport viewport.Model
	width    int
	overview string
	entries  []inspectionEntry
	entry    int
}

type inspectionEntry struct{ title, content string }

func (m *appModel) openInspection(title, content string) {
	m.diffRequestID++ // A newer inspection supersedes an in-flight /diff snapshot.
	m.inspection = &inspectionOverlay{title: title, content: limitInspection(content), viewport: viewport.New(), entry: -1}
	m.inspection.overview = m.inspection.content
	m.inspection.resize(m.width, m.inspectionHeight())
}

func (m *appModel) inspectionHeight() int {
	height := m.height
	if operational := m.renderOperationalBar(); operational != "" {
		height -= lipgloss.Height(operational)
	}
	return max(1, height)
}

func (v *inspectionOverlay) resize(width, height int) {
	width = max(1, width)
	v.viewport.SetWidth(width)
	v.viewport.SetHeight(max(1, height-4))
	if v.width != width {
		offset := v.viewport.YOffset()
		v.viewport.SetContent(wrapText(ansi.Strip(v.content), width))
		v.viewport.SetYOffset(offset)
		v.width = width
	}
}

func (v *inspectionOverlay) View() string {
	header := truncateToWidth(styleHeader.Render(v.title+" · snapshot"), v.width)
	footer := truncateToWidth(fmt.Sprintf("↑↓ pgup/pgdn home/end · esc · %.0f%%", v.viewport.ScrollPercent()*100), v.width)
	if len(v.entries) > 0 {
		footer = truncateToWidth(fmt.Sprintf("←→ detail %d/%d · ↑↓ pg · esc", v.entry+1, len(v.entries)), v.width)
	}
	return joinLayout(header, v.viewport.View(), styleDim.Render(footer))
}

func (m *appModel) handleInspectionKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+shift+c" {
		return m, m.copyClipboardCmd(ansi.Strip(m.inspection.content), "copied inspection")
	}
	switch msg.Code {
	case tea.KeyEsc:
		m.inspection = nil
		return m, nil
	case tea.KeyHome:
		m.inspection.viewport.GotoTop()
		return m, nil
	case tea.KeyEnd:
		m.inspection.viewport.GotoBottom()
		return m, nil
	case tea.KeyLeft, tea.KeyRight:
		v := m.inspection
		if len(v.entries) == 0 {
			return m, nil
		}
		delta := 1
		if msg.Code == tea.KeyLeft {
			delta = -1
		}
		v.entry = min(len(v.entries)-1, max(-1, v.entry+delta))
		if v.entry < 0 {
			v.title, v.content = "Turn overview · read-only", v.overview
		} else {
			v.title = v.entries[v.entry].title + " · read-only"
			v.content = limitInspection(v.entries[v.entry].content)
		}
		v.width = 0
		v.resize(m.width, m.inspectionHeight())
		v.viewport.GotoTop()
		return m, nil
	}
	var cmd tea.Cmd
	m.inspection.viewport, cmd = m.inspection.viewport.Update(msg)
	return m, cmd
}

func limitInspection(s string) string {
	if len(s) <= maxInspectionBytes {
		return s
	}
	s = s[:maxInspectionBytes]
	for n := 0; n < utf8.UTFMax-1 && len(s) > 0 && !utf8.ValidString(s); n++ {
		s = s[:len(s)-1]
	}
	s = strings.ToValidUTF8(s, "�")
	return s + "\n[inspection capped at 1 MiB; retrieve source/artifact paths for remaining data]"
}

// boundedTextTail returns a view onto the existing string. It never copies or
// scans the omitted prefix, including a multi-megabyte single-line response.
func boundedTextTail(s string, byteLimit, lineLimit int) string {
	start := max(0, len(s)-max(0, byteLimit))
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	lines := 0
	for i := len(s) - 1; i >= start; i-- {
		if s[i] == '\n' {
			lines++
			if lines >= lineLimit {
				start = i + 1
				break
			}
		}
	}
	return s[start:]
}

func boundedTranscriptJoin(parts []string) string {
	var tails []string
	remaining, lines := maxRenderBytes, 0
	for i := len(parts) - 1; i >= 0 && remaining > 0 && lines < maxViewportLines; i-- {
		part := boundedTextTail(parts[i], remaining, maxViewportLines-lines)
		tails = append(tails, part)
		remaining -= len(part) + 2
		lines += strings.Count(part, "\n") + 2
	}
	for i, j := 0, len(tails)-1; i < j; i, j = i+1, j-1 {
		tails[i], tails[j] = tails[j], tails[i]
	}
	return boundedTextTail(strings.Join(tails, "\n\n"), maxRenderBytes, maxViewportLines)
}

func inspectionValue(value any) string {
	if value == nil {
		return ""
	}
	if err, ok := value.(error); ok {
		return limitInspection(err.Error())
	}
	if text, ok := value.(string); ok {
		return limitInspection(text)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return limitInspection(fmt.Sprint(value))
	}
	return limitInspection(string(data))
}

// Lifecycle summaries must not retain full edit bodies after the bounded
// detail snapshot has been captured. Delegation/path fields stay structured.
func activitySummaryArgs(value any) any {
	args, ok := value.(map[string]any)
	if !ok {
		return summarizeActivityValue(value)
	}
	copy := make(map[string]any, len(args))
	for name, value := range args {
		switch v := value.(type) {
		case string:
			if len(v) > 1024 {
				v = strings.Clone(v[:1024]) + "…"
			}
			copy[name] = v
		case nil, bool, int, int64, float64:
			copy[name] = value
		default:
			copy[name] = summarizeActivityValue(value)
		}
	}
	return copy
}

func activityTitle(kind activityKind) string {
	switch kind {
	case activityModel:
		return "model"
	case activityRetry:
		return "retry"
	case activityContext:
		return "context"
	case activityThinking:
		return "thinking"
	case activityProgress:
		return "progress"
	default:
		return "tool"
	}
}

func activityDetails(item turnItem) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", activityTitle(item.activity), ansi.Strip(item.content))
	if item.toolName != "" {
		fmt.Fprintf(&b, "tool: %s\nagent: %s\ncall: %s\n", item.toolName, item.agentID, item.callID)
	}
	if item.duration > 0 {
		label := "duration"
		if item.observedTime {
			label = "observed elapsed (event delivery)"
		}
		fmt.Fprintf(&b, "%s: %s\n", label, item.duration)
	}
	for _, field := range []struct{ name, text string }{{"arguments", item.args}, {"result", item.result}, {"error", item.failure}} {
		if field.text != "" {
			fmt.Fprintf(&b, "\n%s:\n%s\n", field.name, field.text)
		}
	}
	if item.editApplied && item.editPreview != "" {
		fmt.Fprintf(&b, "\nedit diff (captured replacement):\n%s\n", item.editPreview)
	}
	return limitInspection(b.String())
}

func (m *appModel) inspectTurn(filter string) {
	if filter == "context" {
		content := "No context report yet."
		if m.lastContextReport != nil {
			content = RenderContextReport(*m.lastContextReport, m.lastMemoryIntent, 0)
		}
		m.openInspection("Context · read-only", content)
		return
	}
	items := m.lastTurnItems
	if m.sending {
		items = m.activeTurnItems
	}
	var b strings.Builder
	b.WriteString("Read-only turn snapshot. Reopen /inspect to refresh.\n")
	if filter == "changes" {
		b.WriteString("Captured edit arguments/results; this is not a working-tree diff. /rewind undoes the last supported edit.\n")
	}
	count := 0
	for _, item := range items {
		if filter == "changes" && item.toolName != "file_write" && item.toolName != "file_edit" && item.toolName != "apply_patch" {
			continue
		}
		count++
		b.WriteString("\n---\n")
		if item.kind == turnItemActivity {
			b.WriteString(activityDetails(item))
		} else {
			b.WriteString(limitInspection(item.content))
		}
		if b.Len() > maxInspectionBytes {
			break
		}
	}
	if count == 0 {
		b.WriteString("\nNo captured details for this view.")
	}
	m.openInspection("Turn details · read-only · "+filter, b.String())
	// Each captured field remains independently reachable even when the
	// aggregate overview exceeds its display cap. Navigation does not mutate
	// tools or query storage and uses immutable string snapshots.
	for _, item := range items {
		if filter == "changes" && item.toolName != "file_write" && item.toolName != "file_edit" && item.toolName != "apply_patch" {
			continue
		}
		if item.kind != turnItemActivity {
			m.inspection.entries = append(m.inspection.entries, inspectionEntry{"Text / receipt", item.content})
			continue
		}
		heading := activityTitle(item.activity)
		if item.toolName != "" {
			heading = item.toolName + " " + item.callID
		}
		metadata := item
		metadata.args, metadata.result, metadata.failure, metadata.editPreview = "", "", "", ""
		m.inspection.entries = append(m.inspection.entries, inspectionEntry{heading, activityDetails(metadata)})
		edit := ""
		if item.editApplied {
			edit = item.editPreview
		}
		for _, field := range []struct{ name, text string }{{"edit", edit}, {"arguments", item.args}, {"result", item.result}, {"error", item.failure}} {
			if field.text != "" {
				m.inspection.entries = append(m.inspection.entries, inspectionEntry{heading + " · " + field.name, field.text})
			}
		}
	}
}

// RenderMarkdownLite converts a small, deliberately restricted markdown
// subset (headers, **bold**, _italic_, `inline code`, fenced code blocks,
// bullet/numbered lists, blockquotes) to ANSI via lipgloss, in place of
// pulling in a full markdown+syntax-highlighting engine (glamour) — see
// ROADMAP.md's binary-size goal. Fence delimiters are hidden and the
// language tag (if any) is shown as a small label; code content itself is
// rendered verbatim (no inline processing) in a shaded "card" and truncated
// (not wrapped) to width, since re-wrapping code corrupts it. Everything
// else is word-wrapped to width when width > 0 — every branch must wrap,
// since the interactive REPL's viewport does not wrap long lines itself and
// an overflowing line can visually corrupt the fixed-height layout around it.
func RenderMarkdownLite(s string, width int) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	var out []string
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if !inFence {
				if lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```")); lang != "" {
					out = append(out, truncateToWidth(styleCodeLang.Render(lang), width))
				}
			}
			inFence = !inFence
			continue
		}
		if inFence {
			out = append(out, styleCodeBlock.Render(truncateToWidth(" "+line, width)))
			continue
		}
		if m := reHeader.FindStringSubmatch(line); m != nil {
			out = append(out, wrapText(styleHeader.Render(m[2]), width))
			continue
		}
		if m := reBlockquote.FindStringSubmatch(line); m != nil {
			out = append(out, wrapText(styleBlockquote.Render("│ "+inlineStyle(m[1])), width))
			continue
		}
		if m := reBullet.FindStringSubmatch(line); m != nil {
			indent, marker, rest := m[1], m[2], m[3]
			bullet := marker
			if marker == "-" || marker == "*" {
				bullet = "•"
			}
			line = indent + styleBullet.Render(bullet) + " " + inlineStyle(rest)
			out = append(out, wrapText(line, width))
			continue
		}
		out = append(out, wrapText(inlineStyle(line), width))
	}
	return strings.Join(out, "\n")
}

// extractFencedBlocks returns the body of each markdown ``` fence in s, in
// order. Language tags are discarded. An unclosed fence at EOF is kept if it
// has any content so a still-streaming reply can be copied.
func extractFencedBlocks(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	var blocks []string
	var body strings.Builder
	inFence := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inFence {
				blocks = append(blocks, strings.TrimSuffix(body.String(), "\n"))
				body.Reset()
				inFence = false
			} else {
				inFence = true
			}
			continue
		}
		if inFence {
			body.WriteString(line)
			body.WriteByte('\n')
		}
	}
	if inFence {
		if text := strings.TrimSuffix(body.String(), "\n"); text != "" {
			blocks = append(blocks, text)
		}
	}
	return blocks
}

// inlineStyle applies inline markdown (bold, italic, code) to a single line.
// Order matters only in that ** is checked before _ so "**_x_**" nests
// correctly; overlapping markers in adversarial input aren't guaranteed to
// render perfectly, which is an accepted limit of a "lite" renderer.
//
// Fix Issue 4: the original code used ReplaceAllStringFunc + FindStringSubmatch
// inside the closure, running the regex TWICE per match (once to find the match,
// once to extract the capture group). We now extract the inner text with simple
// O(1) string slicing of the already-matched string instead:
//   - **text** → match[2 : len-2]
//   - _text_   → match[1 : len-1]
//   - `text`   → match[1 : len-1]
func inlineStyle(s string) string {
	var code []string
	s = reInlineCode.ReplaceAllStringFunc(s, func(match string) string {
		code = append(code, styleInlineCode.Render(match[1:len(match)-1]))
		return fmt.Sprintf("\x00C%d\x00", len(code)-1)
	})
	s = reBold.ReplaceAllStringFunc(s, func(match string) string {
		// match is "**inner**" — slice off the two leading/trailing asterisks
		return styleBold.Render(match[2 : len(match)-2])
	})
	s = reItalic.ReplaceAllStringFunc(s, func(match string) string {
		// match is "_inner_" — slice off the one leading/trailing underscore
		return styleItalic.Render(match[1 : len(match)-1])
	})
	for i, rendered := range code {
		s = strings.ReplaceAll(s, fmt.Sprintf("\x00C%d\x00", i), rendered)
	}
	return s
}

// wrapText word-wraps s to width columns via lipgloss (which is ANSI-aware,
// so it wraps around the escape codes inlineStyle already inserted rather
// than counting them as visible characters). width <= 0 disables wrapping.
//
// Fix Issue 3: reuses the package-level wrapBaseStyle instead of allocating
// a fresh lipgloss.Style on every call.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	return lipgloss.Wrap(s, width, " ")
}

// truncateToWidth clips s to width columns (ANSI-aware) instead of wrapping
// it — used for code block lines, where word-wrapping would corrupt the
// code. width <= 0 disables truncation.
//
// Fix Issue 3: reuses the package-level truncateBaseStyle instead of allocating
// a fresh lipgloss.Style on every call.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	return truncateBaseStyle.MaxWidth(width).Render(s)
}

// SummarizeArgs compacts a tool call's JSON arguments string for a one-line
// preview: newlines flattened to spaces, capped at 80 characters.
func SummarizeArgs(args string) string {
	args = strings.TrimSpace(args)
	args = strings.ReplaceAll(args, "\n", " ")
	if len(args) > 80 {
		args = args[:77] + "..."
	}
	return args
}

// RenderToolCall renders a single tool-call line (name + summarized args).
func RenderToolCall(name, argSummary string) string {
	return fmt.Sprintf("  %s %s %s", styleTool.Render("⎿"), styleBold.Render(name), styleDim.Render(argSummary))
}

func RenderToolActivity(agent, name string, args any, done bool, eventErr any) string {
	if name == "spawn_subagent" {
		return RenderSubagentActivity(agent, args, done, eventErr)
	}
	state := "running"
	marker := "⎿"
	if done {
		state = "done"
		marker = "✓"
	}
	if eventErr != nil {
		state = "failed"
		marker = "✗"
	}
	details := summarizeToolArgs(name, args)
	if eventErr != nil {
		if failure := summarizeActivityValue(eventErr); failure != "" {
			details = "error=" + failure
		}
	}
	if details != "" {
		details = "  " + details
	}
	return fmt.Sprintf("  %s %s%s %s%s", styleTool.Render(marker), agent,
		styleBold.Render(name), styleDim.Render("· "+state), styleDim.Render(details))
}

// RenderSubagentActivity keeps delegated work visible without exposing a raw
// JSON argument blob. The task is intentionally concise so the activity line
// remains readable on narrow terminals.
func RenderSubagentActivity(parent string, args any, done bool, eventErr any) string {
	values, _ := args.(map[string]any)
	name, _ := values["agent"].(string)
	if name == "" {
		name, _ = values["name"].(string)
	}
	if name == "" {
		name = "dynamic"
	}
	task, _ := values["task"].(string)
	task = SummarizeArgs(task)
	state, marker := "working", "◇"
	if done {
		state, marker = "completed", "✓"
	}
	if eventErr != nil {
		state, marker = "failed", "✗"
		task = summarizeActivityValue(eventErr)
	}
	if task != "" {
		task = "  " + task
	}
	return fmt.Sprintf("  %s %s%s %s%s", styleTool.Render(marker), parent,
		styleAgentName.Render("@"+name), styleDim.Render("· "+state), styleDim.Render(task))
}

func RenderModelActivity(agent, modelName string) string {
	return RenderModelActivityCount(agent, modelName, 1)
}

func RenderModelActivityCount(agent, modelName string, calls int) string {
	label := "1 call"
	if calls != 1 {
		label = fmt.Sprintf("%d calls", calls)
	}
	return fmt.Sprintf("  %s %s%s %s", styleTool.Render("•"), agent,
		styleBold.Render("model"), styleDim.Render("· "+label+" · "+modelName))
}

// RenderContextSummary returns one metadata-only activity row for a turn.
func RenderContextSummary(report orchestrator.ContextReport, intent *memory.IntentResult) string {
	selected := 0
	for _, source := range report.Sources {
		if source.SelectedCount > 0 {
			selected++
		}
	}
	line := fmt.Sprintf("  %s %s", styleTool.Render("•"), styleDim.Render(fmt.Sprintf(
		"context · %d/%d sources · %s/%s", selected, len(report.Sources), formatContextBytes(report.TotalBytes), formatContextBytes(report.BudgetBytes))))
	if report.Truncated {
		line += styleDim.Render(" · truncated")
	}
	if intent != nil {
		line += styleDim.Render(" · memory " + renderMemoryIntent(intent))
	}
	return line
}

// RenderContextReport renders only the stable metadata exposed by ContextReport.
func RenderContextReport(report orchestrator.ContextReport, intent *memory.IntentResult, width int) string {
	lines := []string{styleBold.Render("context sources:")}
	for _, source := range report.Sources {
		state := fmt.Sprintf("selected %d · bytes %s · budget %s", source.SelectedCount,
			formatContextBytes(source.Bytes), formatContextBytes(source.BudgetBytes))
		if source.OmissionReason != "" {
			state += " · omitted: " + strings.ReplaceAll(source.OmissionReason, "_", " ")
		}
		if source.Truncated {
			state += " · truncated"
		}
		line := fmt.Sprintf("  %s [%s] · %s", source.Title, source.Kind, state)
		lines = append(lines, wrapText(line, width))
	}
	total := fmt.Sprintf("total: selected %d · bytes %s · budget %s", report.TotalCount,
		formatContextBytes(report.TotalBytes), formatContextBytes(report.BudgetBytes))
	if report.Truncated {
		total += " · truncated"
	}
	lines = append(lines, wrapText(total, width), wrapText("memory intent: "+renderMemoryIntent(intent), width))
	return strings.Join(lines, "\n")
}

func renderMemoryIntent(intent *memory.IntentResult) string {
	if intent == nil {
		return "none"
	}
	result := "not applied"
	if intent.Applied {
		result = "applied"
	} else if reason := safeMemoryIntentReason(intent.Reason); reason != "" {
		result += " (" + reason + ")"
	}
	detail := string(intent.Action)
	if intent.Category != "" {
		detail += " " + string(intent.Category)
	}
	return detail + " · " + result
}

func safeMemoryIntentReason(reason string) string {
	switch reason {
	case "auto_extract_disabled":
		return "auto extract disabled"
	case "memory_disabled":
		return "memory disabled"
	default:
		return ""
	}
}

func formatContextBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f KiB", float64(n)/1024)
}

func summarizeToolArgs(name string, value any) string {
	args, ok := value.(map[string]any)
	if !ok {
		return summarizeActivityValue(value)
	}
	switch name {
	case "file_read":
		path, _ := args["path"].(string)
		details := compactPath(path)
		if line, ok := args["line"]; ok {
			details += fmt.Sprintf(" :%v", line)
		}
		if start, ok := args["start_line"]; ok {
			details += fmt.Sprintf(" :%v", start)
		}
		if end, ok := args["end_line"]; ok {
			details += fmt.Sprintf("-%v", end)
		}
		return details
	case "file_grep":
		path, _ := args["path"].(string)
		pattern, _ := args["regex"].(string)
		if pattern == "" {
			pattern, _ = args["pattern"].(string)
		}
		return SummarizeArgs(fmt.Sprintf("%s  /%s/", compactPath(path), pattern))
	case "shell", "shell_auto":
		command, _ := args["command"].(string)
		return SummarizeArgs(command)
	default:
		return SummarizeArgs(FormatArgs(args))
	}
}

func compactPath(path string) string {
	clean := filepath.ToSlash(filepath.Clean(path))
	parts := strings.Split(clean, "/")
	if len(parts) <= 3 {
		return clean
	}
	return "…/" + strings.Join(parts[len(parts)-3:], "/")
}

func summarizeActivityValue(value any) string {
	if value == nil {
		return ""
	}
	if args, ok := value.(map[string]any); ok {
		return SummarizeArgs(FormatArgs(args))
	}
	if text, ok := value.(string); ok {
		return SummarizeArgs(text)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return SummarizeArgs(fmt.Sprint(value))
	}
	return SummarizeArgs(string(encoded))
}

// RenderTurnHeader renders a turn header line: icon + styled name + trailing separator.
func RenderTurnHeader(icon, name string, nameStyle terminalStyle, width int) string {
	prefix := icon + " " + nameStyle.Render(name) + " "
	if width > 0 && lipgloss.Width(prefix) >= width {
		return truncateToWidth(prefix, width)
	}
	prefixWidth := lipgloss.Width(prefix)
	remaining := width - prefixWidth
	if remaining < 0 {
		remaining = 0
	}
	return prefix + styleSeparator.Render(strings.Repeat("─", remaining))
}

// RenderFileWriteDiff renders a file_write tool call as a compact diff:
// old_content lines prefixed "-", new_content lines prefixed "+" (or a
// "new file" banner when create=true / old_content is empty). This mirrors a
// block-replace, not a positional line diff — it matches file_write's actual
// contract (replace old_content with new_content).
func RenderFileWriteDiff(args map[string]any) string {
	path, _ := args["path"].(string)
	oldContent, _ := args["old_content"].(string)
	newContent, _ := args["new_content"].(string)
	create, _ := args["create"].(bool)

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", styleDim.Render("Path:"), path)
	if create || oldContent == "" {
		b.WriteString(styleDiffAdded.Render("+++ new file") + "\n")
		for _, line := range truncatedLines(newContent, 40) {
			b.WriteString(styleDiffAdded.Render("+ "+line) + "\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}
	for _, line := range truncatedLines(oldContent, 20) {
		b.WriteString(styleDiffRemoved.Render("- "+line) + "\n")
	}
	for _, line := range truncatedLines(newContent, 20) {
		b.WriteString(styleDiffAdded.Render("+ "+line) + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// file_write replaces an exact block, so its successful arguments describe
// the edit without consulting the (possibly already changed) working tree.
func editDiffPreview(args map[string]any) string {
	oldContent, _ := args["old_content"].(string)
	create, _ := args["create"].(bool)
	if !create && oldContent == "" {
		return ""
	}
	bounded := make(map[string]any, len(args))
	for k, v := range args {
		bounded[k] = v
	}
	cut := false
	for _, key := range []string{"old_content", "new_content"} {
		text, _ := bounded[key].(string)
		if len(text) > 4096 {
			text = text[:4096]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
			bounded[key] = text
			cut = true
		}
	}
	preview := ansi.Strip(RenderFileWriteDiff(bounded))
	if cut {
		preview += "\n[preview truncated; /inspect changes shows captured arguments]"
	}
	return preview
}

// Inline details are intentionally short; /inspect keeps the captured full
// fields available with independent scrolling and copying.
func (m *appModel) renderToolExcerpt(label, text string) string {
	const maxBytes = 1400
	cut := len(text) > maxBytes
	if cut {
		text = text[:maxBytes]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	lines := strings.Split(ansi.Strip(text), "\n")
	if len(lines) > 10 {
		lines = lines[:10]
		cut = true
	}
	var b strings.Builder
	b.WriteString(truncateToWidth(styleDim.Render("    "+label), m.viewport.Width()))
	for _, line := range lines {
		b.WriteByte('\n')
		style := styleDim
		if strings.HasPrefix(line, "+") {
			style = styleDiffAdded
		} else if strings.HasPrefix(line, "-") {
			style = styleDiffRemoved
		}
		b.WriteString(truncateToWidth(style.Render("    "+line), m.viewport.Width()))
	}
	if cut {
		b.WriteByte('\n')
		b.WriteString(truncateToWidth(styleDim.Render("    … /inspect for more"), m.viewport.Width()))
	}
	return b.String()
}

// RenderShellPreview renders a shell tool call, surfacing the command and
// working directory prominently since it's the highest-risk tool, while
// still showing any other args (e.g. timeout_sec) so nothing the model set
// is hidden from the approver.
func RenderShellPreview(args map[string]any) string {
	command, _ := args["command"].(string)
	workingDir, _ := args["working_dir"].(string)

	var b strings.Builder
	fmt.Fprintf(&b, "%s  %s\n", styleDim.Render("Command:"), styleBold.Render(command))
	if workingDir != "" {
		fmt.Fprintf(&b, "%s      %s\n", styleDim.Render("Dir:"), workingDir)
	}
	if rest := FormatArgsExcept(args, "command", "working_dir"); rest != "" {
		fmt.Fprintf(&b, "%s    %s\n", styleDim.Render("Other:"), rest)
	}
	return strings.TrimRight(b.String(), "\n")
}

// truncatedLines splits s into lines, capping the total at maxLines (with a
// "... (N more lines)" marker) so a huge file write doesn't flood the
// terminal.
func truncatedLines(s string, maxLines int) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= maxLines {
		return lines
	}
	out := append([]string{}, lines[:maxLines]...)
	out = append(out, fmt.Sprintf("... (%d more lines)", len(lines)-maxLines))
	return out
}

// FormatArgs formats a tool call's arguments as "key=value, key=value", for
// tools without a dedicated preview.
func FormatArgs(args map[string]any) string {
	return FormatArgsExcept(args)
}

// FormatArgsExcept is FormatArgs but skipping keys already rendered
// explicitly by a tool-specific preview (e.g. "command", "working_dir"), so
// nothing the model set is silently hidden from the approver while avoiding
// duplicate display of fields already shown.
func FormatArgsExcept(args map[string]any, skip ...string) string {
	if len(args) == 0 {
		return ""
	}
	skipSet := make(map[string]bool, len(skip))
	for _, k := range skip {
		skipSet[k] = true
	}
	parts := make([]string, 0, len(args))
	for k, v := range args {
		if skipSet[k] {
			continue
		}
		s := fmt.Sprintf("%v", v)
		if len(s) > 60 {
			s = s[:57] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
