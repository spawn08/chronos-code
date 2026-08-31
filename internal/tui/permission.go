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

		argStr := formatApprovalArgs(args)
		fmt.Fprintf(w, "\n\033[33m--- Permission Required ---\033[0m\n")
		fmt.Fprintf(w, "Tool:  %s\n", toolName)
		if argStr != "" {
			fmt.Fprintf(w, "Args:  %s\n", argStr)
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
