package tui

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spawn08/chronos/engine/tool"
)

func InteractiveApproval(reader *bufio.Reader, w io.Writer) tool.ApprovalFunc {
	autoApproved := make(map[string]bool)
	return func(ctx context.Context, toolName string, args map[string]any) (bool, error) {
		if autoApproved[toolName] {
			return true, nil
		}

		fmt.Fprintf(w, "\n\033[33m--- Permission Required ---\033[0m\n")
		fmt.Fprintf(w, "Tool:  %s\n", toolName)
		switch toolName {
		case "file_write":
			printFileWritePreview(w, args)
		case "shell", "shell_auto":
			printShellPreview(w, args)
		default:
			if argStr := formatApprovalArgs(args); argStr != "" {
				fmt.Fprintf(w, "Args:  %s\n", argStr)
			}
		}
		fmt.Fprintf(w, "Allow? [\033[32my\033[0m]es / [\033[31mn\033[0m]o / [\033[34ma\033[0m]lways: ")

		line, err := reader.ReadString('\n')
		if err != nil {
			return false, err
		}
		line = strings.TrimSpace(strings.ToLower(line))

		switch line {
		case "y", "yes", "":
			return true, nil
		case "a", "always":
			autoApproved[toolName] = true
			return true, nil
		default:
			return false, nil
		}
	}
}

// printFileWritePreview renders a file_write tool call as a compact diff:
// old_content lines prefixed "-", new_content lines prefixed "+" (or a
// "new file" banner when create=true / old_content is empty). This is a
// block-replace preview, not a positional line diff — it matches file_write's
// actual contract (replace old_content with new_content).
func printFileWritePreview(w io.Writer, args map[string]any) {
	path, _ := args["path"].(string)
	oldContent, _ := args["old_content"].(string)
	newContent, _ := args["new_content"].(string)
	create, _ := args["create"].(bool)

	fmt.Fprintf(w, "Path:  %s\n", path)
	if create || oldContent == "" {
		fmt.Fprintf(w, "\033[32m+++ new file\033[0m\n")
		for _, line := range truncatedLines(newContent, 40) {
			fmt.Fprintf(w, "\033[32m+ %s\033[0m\n", line)
		}
		return
	}
	for _, line := range truncatedLines(oldContent, 20) {
		fmt.Fprintf(w, "\033[31m- %s\033[0m\n", line)
	}
	for _, line := range truncatedLines(newContent, 20) {
		fmt.Fprintf(w, "\033[32m+ %s\033[0m\n", line)
	}
}

// printShellPreview renders a shell tool call, surfacing the command and
// working directory prominently since it's the highest-risk tool.
func printShellPreview(w io.Writer, args map[string]any) {
	command, _ := args["command"].(string)
	workingDir, _ := args["working_dir"].(string)
	fmt.Fprintf(w, "\033[31mCommand:\033[0m %s\n", command)
	if workingDir != "" {
		fmt.Fprintf(w, "Dir:     %s\n", workingDir)
	}
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

func formatApprovalArgs(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	parts := make([]string, 0, len(args))
	for k, v := range args {
		s := fmt.Sprintf("%v", v)
		if len(s) > 60 {
			s = s[:57] + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%s", k, s))
	}
	return strings.Join(parts, ", ")
}
