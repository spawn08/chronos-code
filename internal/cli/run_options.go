package cli

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Output formats accepted by `run --output-format`.
const (
	outputText       = "text"
	outputJSON       = "json"
	outputStreamJSON = "stream-json"
)

// runOptions are the flags specific to `chronos-code run`. They are parsed
// separately from the global flags so a prompt is never confused with them
// once it is read from stdin.
type runOptions struct {
	outputFormat     string
	promptStdin      bool
	systemPrompt     string
	systemPromptFile string
	maxTurns         int
	mcpConfigs       []string
	strictMCPConfig  bool
	thinking         string
	ephemeral        bool
}

// streamJSON reports whether events are written as JSONL.
func (o runOptions) streamJSON() bool { return o.outputFormat == outputStreamJSON }

// parseRunFlags extracts the run-only flags from args (the arguments after
// "run") and returns the remaining message words.
func parseRunFlags(args []string) (runOptions, []string, error) {
	var opts runOptions
	var rest []string
	value := func(i *int, name string) (string, error) {
		if *i+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", name)
		}
		*i++
		return args[*i], nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, inline, hasInline := strings.Cut(arg, "=")
		if !strings.HasPrefix(arg, "--") {
			rest = append(rest, arg)
			continue
		}
		get := func() (string, error) {
			if hasInline {
				return inline, nil
			}
			return value(&i, name)
		}
		var err error
		var v string
		switch name {
		case "--output-format":
			if v, err = get(); err == nil {
				switch v {
				case outputText, outputJSON, outputStreamJSON:
					opts.outputFormat = v
				default:
					err = fmt.Errorf("--output-format %q is invalid (want text, json, or stream-json)", v)
				}
			}
		case "--prompt-stdin":
			opts.promptStdin = true
		case "--system-prompt":
			opts.systemPrompt, err = get()
		case "--system-prompt-file":
			opts.systemPromptFile, err = get()
		case "--max-turns":
			if v, err = get(); err == nil {
				n, convErr := strconv.Atoi(strings.TrimSpace(v))
				if convErr != nil || n < 1 {
					err = fmt.Errorf("--max-turns %q must be a positive integer", v)
				}
				opts.maxTurns = n
			}
		case "--mcp-config":
			if v, err = get(); err == nil {
				opts.mcpConfigs = append(opts.mcpConfigs, v)
			}
		case "--strict-mcp-config":
			opts.strictMCPConfig = true
		case "--thinking":
			opts.thinking, err = get()
		case "--ephemeral":
			opts.ephemeral = true
		case "--":
			rest = append(rest, args[i+1:]...)
			i = len(args)
		default:
			rest = append(rest, arg)
		}
		if err != nil {
			return runOptions{}, nil, err
		}
	}
	return opts, rest, nil
}

// resolvePrompt returns the task message: all of stdin when --prompt-stdin
// or "-" is given, otherwise the remaining words.
func resolvePrompt(opts runOptions, words []string, stdin io.Reader) (string, error) {
	if opts.promptStdin || (len(words) == 1 && words[0] == "-") {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return "", fmt.Errorf("read prompt from stdin: %w", err)
		}
		message := strings.TrimSpace(string(data))
		if message == "" {
			return "", fmt.Errorf("prompt from stdin is empty")
		}
		return message, nil
	}
	if len(words) == 0 {
		return "", fmt.Errorf("usage: chronos-code run <message>")
	}
	return strings.Join(words, " "), nil
}

// resolveSystemPrompt combines --system-prompt and --system-prompt-file.
func resolveSystemPrompt(opts runOptions) (string, error) {
	text := strings.TrimSpace(opts.systemPrompt)
	if opts.systemPromptFile != "" {
		data, err := os.ReadFile(opts.systemPromptFile)
		if err != nil {
			return "", fmt.Errorf("read --system-prompt-file: %w", err)
		}
		if file := strings.TrimSpace(string(data)); file != "" {
			if text != "" {
				text += "\n\n"
			}
			text += file
		}
	}
	return text, nil
}
