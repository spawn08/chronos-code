package tui

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/spawn08/chronos-code/internal/orchestrator"
)

// toolPreviewLines is how many output lines show under a collapsed tool line.
const toolPreviewLines = 4

// toolOutput is a tool result rendered for people instead of as JSON.
type toolOutput struct {
	text    string   // readable output for ctrl+o and /inspect
	preview []string // short inline excerpt shown under the tool line
	summary string   // appended to the status line, e.g. "exit 1", "12 matches"
	failed  bool     // the call completed but reported failure (non-zero exit)
}

// formatToolOutput renders result (the tool's own result when the
// tool_output event arrived, else the compressed model-facing form) per tool.
// root makes absolute workspace paths relative.
func formatToolOutput(name string, result any, root string) toolOutput {
	if result == nil {
		return toolOutput{}
	}
	if text, ok := result.(string); ok {
		out := toolOutput{text: text}
		if name != "file_read" {
			out.preview = headLines(text, toolPreviewLines)
		}
		return out
	}
	values, ok := result.(map[string]any)
	if !ok {
		return toolOutput{text: formatReadable(result)}
	}
	if compressed, _ := values["compressed"].(bool); compressed {
		return formatCompressedOutput(values)
	}
	switch name {
	case "shell", "shell_auto":
		return formatShellOutput(values)
	case "file_read":
		return formatReadOutput(values)
	case "file_write":
		return formatWriteOutput(values, root)
	case "file_list":
		return formatListOutput(values)
	case "file_glob":
		return formatGlobOutput(values, root)
	case "file_grep":
		return formatGrepOutput(values, root)
	case orchestrator.ExitPlanModeToolName:
		status, _ := values["status"].(string)
		message, _ := values["message"].(string)
		return toolOutput{text: message, summary: strings.ReplaceAll(status, "_", " ")}
	case "read_stored_result":
		if content, ok := values["content"].(string); ok {
			return toolOutput{text: content, preview: headLines(content, toolPreviewLines)}
		}
	}
	text := formatReadable(values)
	return toolOutput{text: text, preview: headLines(text, toolPreviewLines)}
}

func formatShellOutput(values map[string]any) toolOutput {
	stdout, _ := values["stdout"].(string)
	stderr, _ := values["stderr"].(string)
	combined := strings.TrimRight(stdout, "\n")
	if stderr = strings.TrimRight(stderr, "\n"); stderr != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += stderr
	}
	out := toolOutput{text: combined}
	if combined == "" {
		out.text = "(no output)"
	}
	out.preview = tailLines(out.text, toolPreviewLines)
	if code, ok := toInt(values["exit_code"]); ok && code != 0 {
		out.failed = true
		out.summary = fmt.Sprintf("exit %d", code)
		out.text += fmt.Sprintf("\n[exit code %d]", code)
	}
	return out
}

func formatReadOutput(values map[string]any) toolOutput {
	if outline, _ := values["outline"].(bool); outline {
		declarations := stringItems(values["declarations"])
		return toolOutput{text: strings.Join(declarations, "\n"), summary: fmt.Sprintf("outline · %d declarations", len(declarations))}
	}
	content, _ := values["content"].(string)
	out := toolOutput{text: content}
	start, hasStart := toInt(values["start_line"])
	end, hasEnd := toInt(values["end_line"])
	total, hasTotal := toInt(values["total_lines"])
	switch {
	case hasStart && hasEnd && hasTotal && (start > 1 || end < total):
		out.summary = fmt.Sprintf("lines %d-%d of %d", start, end, total)
	case hasStart && hasEnd && !hasTotal:
		out.summary = fmt.Sprintf("lines %d-%d", start, end)
	case content != "":
		out.summary = pluralize(strings.Count(strings.TrimRight(content, "\n"), "\n")+1, "line", "lines")
	}
	if truncated, _ := values["truncated"].(bool); truncated {
		out.summary += " · truncated"
	}
	return out
}

func formatWriteOutput(values map[string]any, root string) toolOutput {
	path, _ := values["path"].(string)
	out := toolOutput{}
	if replacements, ok := toInt(values["replacements"]); ok {
		out.summary = pluralize(replacements, "replacement", "replacements")
	} else if written, ok := toInt(values["bytes_written"]); ok {
		out.summary = "wrote " + formatBytes(uint64(max(written, 0)))
	}
	out.text = strings.TrimSpace(relativePath(path, root) + "\n" + out.summary)
	return out
}

func formatListOutput(values map[string]any) toolOutput {
	entries := sliceItems(values["entries"])
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		item, _ := entry.(map[string]any)
		name, _ := item["name"].(string)
		if dir, _ := item["is_dir"].(bool); dir {
			lines = append(lines, name+"/")
		} else if size, ok := toInt(item["size"]); ok {
			lines = append(lines, fmt.Sprintf("%s  %s", name, formatBytes(uint64(max(size, 0)))))
		} else {
			lines = append(lines, name)
		}
	}
	return listOutput(lines, pluralize(len(lines), "entry", "entries"))
}

func formatGlobOutput(values map[string]any, root string) toolOutput {
	matches := stringItems(values["matches"])
	for i, match := range matches {
		matches[i] = relativePath(match, root)
	}
	return listOutput(matches, pluralize(len(matches), "file", "files"))
}

func formatGrepOutput(values map[string]any, root string) toolOutput {
	matches := sliceItems(values["matches"])
	lines := make([]string, 0, len(matches))
	for _, match := range matches {
		item, _ := match.(map[string]any)
		content, _ := item["content"].(string)
		line, _ := toInt(item["line_number"])
		if file, _ := item["file"].(string); file != "" {
			lines = append(lines, fmt.Sprintf("%s:%d: %s", relativePath(file, root), line, content))
		} else {
			lines = append(lines, fmt.Sprintf("%d: %s", line, content))
		}
	}
	out := listOutput(lines, pluralize(len(lines), "match", "matches"))
	if truncated, _ := values["truncated"].(bool); truncated {
		out.summary += " · truncated"
	}
	return out
}

func formatCompressedOutput(values map[string]any) toolOutput {
	size, _ := toInt(values["full_size_bytes"])
	key, _ := values["storage_key"].(string)
	preview, _ := values["preview"].(string)
	text := fmt.Sprintf("output compressed for the model (%s, stored as %s)", formatBytes(uint64(max(size, 0))), key)
	if preview != "" {
		text += "\n\n" + preview
	}
	return toolOutput{text: text, summary: "compressed"}
}

func listOutput(lines []string, summary string) toolOutput {
	text := strings.Join(lines, "\n")
	if len(lines) == 0 {
		text = "(none)"
	}
	return toolOutput{text: text, preview: headLines(text, toolPreviewLines), summary: summary}
}

// RenderToolResultActivity is the finished tool line. Unlike
// RenderToolActivity it marks a completed call that reported failure (such as
// a non-zero shell exit) as failed and appends the output summary.
func RenderToolResultActivity(agent, name string, args any, eventErr any, out toolOutput) string {
	if name == "spawn_subagent" || eventErr != nil || (!out.failed && out.summary == "") {
		return RenderToolActivity(agent, name, args, true, eventErr)
	}
	state, marker := "done", "✓"
	summary := styleDim.Render(" · " + out.summary)
	if out.failed {
		state, marker = "failed", "✗"
		summary = styleError.Render(" · " + out.summary)
	}
	details := summarizeToolArgs(name, args)
	if details != "" {
		details = "  " + details
	}
	return fmt.Sprintf("  %s %s%s %s%s%s", styleTool.Render(marker), agent,
		styleBold.Render(name), styleDim.Render("· "+state), styleDim.Render(details), summary)
}

// renderToolPreview draws the inline output excerpt under a tool line.
func (m *appModel) renderToolPreview(lines []string) string {
	width := m.viewport.Width()
	rendered := make([]string, len(lines))
	for i, line := range lines {
		prefix := "      "
		if i == 0 {
			prefix = "    ⎿ "
		}
		rendered[i] = truncateToWidth(styleDim.Render(prefix+ansi.Strip(line)), width)
	}
	return strings.Join(rendered, "\n")
}

func indentLines(text, indent string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}

// formatToolArgs renders call arguments for ctrl+o and /inspect.
func formatToolArgs(name string, args any) string {
	values, ok := args.(map[string]any)
	if !ok {
		return formatReadable(args)
	}
	switch name {
	case "shell", "shell_auto":
		command, _ := values["command"].(string)
		text := "$ " + command
		rest := make(map[string]any, len(values))
		for key, value := range values {
			if key != "command" {
				rest[key] = value
			}
		}
		if len(rest) > 0 {
			text += "\n" + formatReadable(rest)
		}
		return text
	case orchestrator.ExitPlanModeToolName:
		return "" // The plan itself is rendered under the tool line.
	}
	return formatReadable(values)
}

// planSummary is the first meaningful line of a plan, for the status line.
func planSummary(plan string) string {
	for _, line := range strings.Split(plan, "\n") {
		if line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#")); line != "" {
			return SummarizeArgs(line)
		}
	}
	return ""
}

// formatReadable renders any value as indented "key: value" text instead of
// JSON. Multi-line strings keep their line breaks.
func formatReadable(value any) string {
	var b strings.Builder
	writeReadable(&b, value, "")
	return strings.TrimRight(b.String(), "\n")
}

func writeReadable(b *strings.Builder, value any, indent string) {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			field := v[key]
			if scalar, ok := readableScalar(field); ok {
				if strings.Contains(scalar, "\n") {
					fmt.Fprintf(b, "%s%s:\n", indent, key)
					for _, line := range strings.Split(strings.TrimRight(scalar, "\n"), "\n") {
						fmt.Fprintf(b, "%s  %s\n", indent, line)
					}
				} else {
					fmt.Fprintf(b, "%s%s: %s\n", indent, key, scalar)
				}
				continue
			}
			fmt.Fprintf(b, "%s%s:\n", indent, key)
			writeReadable(b, field, indent+"  ")
		}
	default:
		if scalar, ok := readableScalar(value); ok {
			fmt.Fprintf(b, "%s%s\n", indent, scalar)
			return
		}
		items := sliceItems(value)
		if items == nil {
			encoded, err := json.Marshal(value)
			if err != nil {
				fmt.Fprintf(b, "%s%v\n", indent, value)
			} else {
				fmt.Fprintf(b, "%s%s\n", indent, encoded)
			}
			return
		}
		for _, item := range items {
			if scalar, ok := readableScalar(item); ok {
				fmt.Fprintf(b, "%s- %s\n", indent, scalar)
				continue
			}
			var nested strings.Builder
			writeReadable(&nested, item, "")
			lines := strings.Split(strings.TrimRight(nested.String(), "\n"), "\n")
			for i, line := range lines {
				prefix := "  "
				if i == 0 {
					prefix = "- "
				}
				fmt.Fprintf(b, "%s%s%s\n", indent, prefix, line)
			}
		}
	}
}

func readableScalar(value any) (string, bool) {
	switch v := value.(type) {
	case nil:
		return "null", true
	case string:
		return v, true
	case bool:
		return strconv.FormatBool(v), true
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10), true
		}
		return strconv.FormatFloat(v, 'f', -1, 64), true
	case float32, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, json.Number:
		return fmt.Sprint(v), true
	case error:
		return v.Error(), true
	}
	return "", false
}

// sliceItems accepts []any and typed slices (native tool results) alike.
func sliceItems(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	rv := reflect.ValueOf(value)
	if !rv.IsValid() || rv.Kind() != reflect.Slice {
		return nil
	}
	items := make([]any, rv.Len())
	for i := range items {
		items[i] = rv.Index(i).Interface()
	}
	return items
}

func stringItems(value any) []string {
	items := sliceItems(value)
	out := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func toInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case int32:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func relativePath(path, root string) string {
	if path == "" || root == "" || !filepath.IsAbs(path) {
		return path
	}
	if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return path
}

func pluralize(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// headLines returns up to n leading lines plus a "+N lines" marker.
func headLines(text string, n int) []string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil
	}
	if len(lines) <= n {
		return clipLines(lines)
	}
	return append(clipLines(lines[:n]), fmt.Sprintf("… +%d lines", len(lines)-n))
}

// tailLines returns up to n trailing lines, where command results and errors
// usually are, after a "+N lines" marker.
func tailLines(text string, n int) []string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 1 && strings.TrimSpace(lines[0]) == "" {
		return nil
	}
	if len(lines) <= n {
		return clipLines(lines)
	}
	return append([]string{fmt.Sprintf("… +%d lines", len(lines)-n)}, clipLines(lines[len(lines)-n:])...)
}

// clipLines copies lines, bounding each so previews never pin a large
// output string; the viewport truncates to its width anyway.
func clipLines(lines []string) []string {
	const maxLineBytes = 512
	out := make([]string, len(lines))
	for i, line := range lines {
		if len(line) > maxLineBytes {
			line = line[:maxLineBytes]
			for len(line) > 0 && !utf8.ValidString(line) {
				line = line[:len(line)-1]
			}
		}
		out[i] = strings.ReplaceAll(strings.Clone(line), "\t", "    ")
	}
	return out
}
