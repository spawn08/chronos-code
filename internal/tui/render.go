package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"charm.land/lipgloss/v2"
)

var (
	reBold       = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reItalic     = regexp.MustCompile(`_(.+?)_`)
	reInlineCode = regexp.MustCompile("`([^`]+)`")
	reHeader     = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet     = regexp.MustCompile(`^(\s*)([-*]|\d+\.)\s+(.*)$`)
	reBlockquote = regexp.MustCompile(`^>\s?(.*)$`)
)

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

// inlineStyle applies inline markdown (bold, italic, code) to a single line.
// Order matters only in that ** is checked before _ so "**_x_**" nests
// correctly; overlapping markers in adversarial input aren't guaranteed to
// render perfectly, which is an accepted limit of a "lite" renderer.
func inlineStyle(s string) string {
	s = reBold.ReplaceAllStringFunc(s, func(m string) string {
		return styleBold.Render(reBold.FindStringSubmatch(m)[1])
	})
	s = reItalic.ReplaceAllStringFunc(s, func(m string) string {
		return styleItalic.Render(reItalic.FindStringSubmatch(m)[1])
	})
	s = reInlineCode.ReplaceAllStringFunc(s, func(m string) string {
		return styleInlineCode.Render(reInlineCode.FindStringSubmatch(m)[1])
	})
	return s
}

// wrapText word-wraps s to width columns via lipgloss (which is ANSI-aware,
// so it wraps around the escape codes inlineStyle already inserted rather
// than counting them as visible characters). width <= 0 disables wrapping.
func wrapText(s string, width int) string {
	if width <= 0 {
		return s
	}
	return lipgloss.NewStyle().Width(width).Render(s)
}

// truncateToWidth clips s to width columns (ANSI-aware) instead of wrapping
// it — used for code block lines, where word-wrapping would corrupt the
// code. width <= 0 disables truncation.
func truncateToWidth(s string, width int) string {
	if width <= 0 {
		return s
	}
	return lipgloss.NewStyle().MaxWidth(width).Render(s)
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
	details := ""
	if args != nil {
		if encoded, err := json.Marshal(args); err == nil {
			details = " " + SummarizeArgs(string(encoded))
		}
	}
	return fmt.Sprintf("  %s %s%s %s%s", styleTool.Render(marker), agent,
		styleBold.Render(name), styleDim.Render(state), styleDim.Render(details))
}

// RenderTurnHeader renders a turn header line: icon + styled name + trailing separator.
func RenderTurnHeader(icon, name string, nameStyle terminalStyle, width int) string {
	prefix := icon + " " + nameStyle.Render(name) + " "
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
