package tui

import (
	"fmt"
	"io"
	"strings"

	"github.com/spawn08/chronos/engine/model"
)

func StreamResponse(ch <-chan *model.ChatResponse, w io.Writer) (model.Usage, error) {
	var usage model.Usage
	var lastContent string

	for resp := range ch {
		if resp.Err != nil {
			return usage, resp.Err
		}
		if resp.Usage.PromptTokens > 0 {
			usage = resp.Usage
		}
		if resp.Usage.CompletionTokens > usage.CompletionTokens {
			usage.CompletionTokens = resp.Usage.CompletionTokens
		}

		for _, tc := range resp.ToolCalls {
			argSummary := summarizeArgs(tc.Arguments)
			fmt.Fprintf(w, "\n  \033[36m> %s(%s)\033[0m\n", tc.Name, argSummary)
		}

		if resp.Content != "" && resp.Content != lastContent {
			if resp.Delta {
				fmt.Fprint(w, resp.Content)
			} else {
				fmt.Fprint(w, resp.Content)
			}
			lastContent = resp.Content
		}
	}
	fmt.Fprintln(w)
	return usage, nil
}

func PrintResponse(resp *model.ChatResponse, w io.Writer) {
	if resp == nil {
		return
	}
	for _, tc := range resp.ToolCalls {
		argSummary := summarizeArgs(tc.Arguments)
		fmt.Fprintf(w, "\n  \033[36m> %s(%s)\033[0m\n", tc.Name, argSummary)
	}
	if resp.Content != "" {
		fmt.Fprintln(w, resp.Content)
	}
}

func PrintUsage(usage model.Usage, w io.Writer) {
	if usage.PromptTokens > 0 || usage.CompletionTokens > 0 {
		fmt.Fprintf(w, "\033[2m[tokens: %d prompt + %d completion = %d total]\033[0m\n",
			usage.PromptTokens, usage.CompletionTokens,
			usage.PromptTokens+usage.CompletionTokens)
	}
}

func summarizeArgs(args string) string {
	args = strings.TrimSpace(args)
	if len(args) > 80 {
		args = args[:77] + "..."
	}
	args = strings.ReplaceAll(args, "\n", " ")
	return args
}
