package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/execution"
)

type fileState struct {
	hash   string
	size   int64
	exists bool
}

func wrapVerificationEvidence(a *agent.Agent) {
	if a == nil || a.Tools == nil {
		return
	}
	for _, definition := range a.Tools.List() {
		if definition == nil || definition.Handler == nil {
			continue
		}
		wrapped := *definition
		original := definition.Handler
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			runtime, enabled := taskRuntimeFromContext(ctx)
			if !enabled {
				return original(ctx, args)
			}
			if err := runtime.beforeToolCall(); err != nil {
				return nil, err
			}
			startedAt := time.Now().UTC()
			var before fileState
			var path string
			if wrapped.Name == "file_write" {
				path, _ = args["path"].(string)
				before = readFileState(runtime.workspaceRoot, path)
			}
			result, err := original(ctx, args)
			completedAt := time.Now().UTC()
			switch wrapped.Name {
			case "file_write":
				evidencePath := path
				if values, ok := result.(map[string]any); ok {
					if resolved, ok := values["path"].(string); ok && resolved != "" {
						evidencePath = resolved
					}
				}
				after := readFileState(runtime.workspaceRoot, evidencePath)
				if after.exists && (err == nil || after != before) {
					if _, recordErr := runtime.recordWrite(evidencePath, after.hash, after.size, execution.ProvenanceRuntime, completedAt); recordErr != nil {
						return result, errors.Join(err, fmt.Errorf("record file write evidence: %w", recordErr))
					}
					if refreshErr := runtime.refreshClaims(completedAt, evidencePath); refreshErr != nil {
						return result, errors.Join(err, refreshErr)
					}
				} else if err == nil && !after.exists {
					return result, fmt.Errorf("record file write evidence: written file %q is unavailable", path)
				}
			case "file_read":
				if err == nil {
					runtime.recordReadClaim(args, result)
				}
			case "shell", "shell_auto":
				if recordErr := recordShellEvidence(runtime, ctx, args, result, err, startedAt, completedAt); recordErr != nil {
					return result, errors.Join(err, recordErr)
				}
			}
			return result, err
		}
		a.Tools.Register(&wrapped)
	}
}

func readFileState(root, path string) fileState {
	if path == "" {
		return fileState{}
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileState{}
	}
	sum := sha256.Sum256(data)
	return fileState{hash: hex.EncodeToString(sum[:]), size: int64(len(data)), exists: true}
}

func recordShellEvidence(runtime *taskRuntime, ctx context.Context, args map[string]any, result any, callErr error, startedAt, completedAt time.Time) error {
	command, _ := args["command"].(string)
	terminal := execution.TerminalExited
	var exitCode *int
	if values, ok := result.(map[string]any); ok {
		if code, ok := values["exit_code"].(int); ok {
			exitCode = &code
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		terminal = execution.TerminalTimedOut
	case errors.Is(ctx.Err(), context.Canceled):
		terminal = execution.TerminalCancelled
	case callErr != nil:
		terminal = execution.TerminalSpawnFailed
	}
	mutated := false
	for _, step := range planShellEvidence(command) {
		if step.mutation {
			if _, err := runtime.recordWorkspaceMutation(step.command, completedAt); err != nil {
				return err
			}
			mutated = true
			continue
		}
		if _, err := runtime.recordCommand(step.command, step.class, exitCode, terminal, nil, execution.ProvenanceRuntime, startedAt, completedAt); err != nil {
			return err
		}
	}
	if mutated {
		// A mutating or unclassified command may have touched any file.
		return runtime.refreshClaims(completedAt)
	}
	return nil
}

// shellStep is one ledger effect of a shell command, in execution order:
// either a workspace mutation or a verification check whose pass/fail is
// given by the command's exit status.
type shellStep struct {
	mutation bool
	command  string
	class    execution.CommandClass
}

// shellSegment is one simple command and the operator that follows it.
type shellSegment struct {
	text       string
	op         string // "&&", "||", ";", "|" or "" for the last segment
	writesFile bool   // unquoted output redirection to a file other than /dev/null
}

// planShellEvidence maps a shell command to ledger steps. Compound commands
// are classified per simple command so ordinary agent habits count as
// evidence: `cd pkg && go test ./...`, `go test ./... 2>&1`, and read-only
// filters do not mutate the workspace. A check is recorded only when the exit
// status belongs to it: an all-`&&` chain (0 means every command succeeded)
// or the last command of a `;`/`||` sequence, never a check piped into
// another command. Unparseable commands (subshells, heredocs, background
// jobs) stay conservative: one mutation.
func planShellEvidence(command string) []shellStep {
	segments, ok := splitShell(command)
	if !ok || len(segments) == 0 {
		return []shellStep{{mutation: true, command: command}}
	}
	type pipeline struct {
		segments []shellSegment
		op       string
	}
	var pipelines []pipeline
	current := pipeline{}
	for _, segment := range segments {
		current.segments = append(current.segments, segment)
		if segment.op != "|" {
			current.op = segment.op
			pipelines = append(pipelines, current)
			current = pipeline{}
		}
	}
	allAnd := true
	for _, p := range pipelines[:len(pipelines)-1] {
		if p.op != "&&" {
			allAnd = false
		}
	}
	var steps []shellStep
	for i, p := range pipelines {
		exitOwned := allAnd || i == len(pipelines)-1
		for j, segment := range p.segments {
			class := classifySimpleCommand(segment.text)
			if segment.writesFile {
				steps = append(steps, shellStep{mutation: true, command: segment.text})
			}
			switch {
			case class == execution.CommandMutation || class == execution.CommandUnknown:
				steps = append(steps, shellStep{mutation: true, command: segment.text})
			case j > 0 && !readOnlyFilter(class):
				steps = append(steps, shellStep{mutation: true, command: segment.text})
			case j == 0 && len(p.segments) == 1 && exitOwned && verificationClass(class):
				steps = append(steps, shellStep{command: normalizeShellCommand(segment.text), class: class})
			}
		}
	}
	return steps
}

func verificationClass(class execution.CommandClass) bool {
	switch class {
	case execution.CommandTest, execution.CommandBuild, execution.CommandDiagnostics, execution.CommandDiff:
		return true
	}
	return false
}

// readOnlyFilter reports whether a command may appear after a pipe without
// changing the workspace.
func readOnlyFilter(class execution.CommandClass) bool {
	return class == execution.CommandRead
}

// classifyShellCommand summarizes a whole command: the class of a simple
// command, or mutation when any part of a compound command may mutate.
func classifyShellCommand(command string) execution.CommandClass {
	segments, ok := splitShell(command)
	if !ok {
		return execution.CommandMutation
	}
	if len(segments) == 1 && !segments[0].writesFile {
		return classifySimpleCommand(segments[0].text)
	}
	class := execution.CommandRead
	for _, step := range planShellEvidence(command) {
		if step.mutation {
			return execution.CommandMutation
		}
		class = step.class
	}
	return class
}

// classifySimpleCommand classifies one command without shell operators.
func classifySimpleCommand(command string) execution.CommandClass {
	command = strings.ToLower(normalizeShellCommand(command))
	if command == "" {
		return execution.CommandUnknown
	}
	for _, prefix := range []string{"go test", "pytest", "python -m pytest", "python3 -m pytest", "npm test", "npm run test", "bun test", "cargo test", "mvn test", "make test", "yarn test", "pnpm test", "gradle test", "./gradlew test"} {
		if hasCommandPrefix(command, prefix) {
			return execution.CommandTest
		}
	}
	for _, prefix := range []string{"go build", "npm run build", "bun run build", "cargo build", "mvn package", "make build", "yarn build", "pnpm build", "cargo check"} {
		if hasCommandPrefix(command, prefix) {
			return execution.CommandBuild
		}
	}
	for _, prefix := range []string{"go vet", "golangci-lint", "npm run lint", "bun run lint", "tsc", "make lint", "cargo clippy", "ruff check", "eslint", "staticcheck"} {
		if hasCommandPrefix(command, prefix) {
			return execution.CommandDiagnostics
		}
	}
	if hasCommandPrefix(command, "git diff") {
		return execution.CommandDiff
	}
	if readOnlyCommand(command) {
		return execution.CommandRead
	}
	return execution.CommandUnknown
}

func readOnlyCommand(command string) bool {
	fields := strings.Fields(command)
	switch fields[0] {
	case "cd", "pwd", "ls", "rg", "grep", "egrep", "fgrep", "cat", "head", "tail", "wc", "sort", "uniq", "cut", "tr",
		"echo", "printf", "which", "type", "file", "stat", "du", "df", "tree", "jq", "less", "more", "diff", "cmp",
		"true", "date", "basename", "dirname", "realpath", "nl", "column", "comm", "md5", "shasum", "sha256sum":
		return true
	case "find":
		for _, field := range fields[1:] {
			switch field {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fprintf", "-fls":
				return false
			}
		}
		return true
	case "sed":
		return len(fields) > 1 && fields[1] == "-n" && !strings.Contains(command, " -i")
	case "git":
		if len(fields) < 2 {
			return false
		}
		switch fields[1] {
		case "status", "log", "show", "blame", "rev-parse", "ls-files", "grep", "shortlog", "describe":
			return true
		case "branch", "remote", "tag":
			return len(fields) == 2 || strings.HasPrefix(fields[2], "-v") || fields[2] == "--list" || fields[2] == "-a"
		}
	case "go":
		return len(fields) > 1 && (fields[1] == "list" || fields[1] == "env" || fields[1] == "version" || fields[1] == "doc")
	}
	return false
}

func hasCommandPrefix(command, prefix string) bool {
	return command == prefix || strings.HasPrefix(command, prefix+" ")
}

// normalizeShellCommand trims whitespace, leading environment assignments,
// and a `time` prefix, which do not change what a command verifies.
func normalizeShellCommand(command string) string {
	fields := strings.Fields(command)
	for len(fields) > 0 {
		field := fields[0]
		if eq := strings.IndexByte(field, '='); eq > 0 && validEnvName(field[:eq]) {
			fields = fields[1:]
			continue
		}
		if field == "time" {
			fields = fields[1:]
			continue
		}
		break
	}
	return strings.Join(fields, " ")
}

func validEnvName(name string) bool {
	for i, r := range name {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (i == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// splitShell splits a command on unquoted &&, ||, ;, newline and | and strips
// redirections, recording output redirection to a real file. It returns false
// for constructs it cannot analyze safely.
func splitShell(command string) ([]shellSegment, bool) {
	var segments []shellSegment
	var text strings.Builder
	writes := false
	var quote rune
	escaped := false
	runes := []rune(command)
	flush := func(op string) {
		segment := strings.TrimSpace(text.String())
		if segment != "" {
			segments = append(segments, shellSegment{text: segment, op: op, writesFile: writes})
		} else if op != ";" && op != "" {
			segments = append(segments, shellSegment{op: op})
		}
		text.Reset()
		writes = false
	}
	readWord := func(i int) (string, int) {
		for i < len(runes) && (runes[i] == ' ' || runes[i] == '\t') {
			i++
		}
		start := i
		for i < len(runes) && !strings.ContainsRune(" \t;&|<>\n", runes[i]) {
			i++
		}
		return string(runes[start:i]), i
	}
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if escaped {
			text.WriteRune(r)
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			text.WriteRune(r)
			escaped = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else if quote == '"' && (r == '`' || (r == '$' && i+1 < len(runes) && runes[i+1] == '(')) {
				return nil, false
			}
			text.WriteRune(r)
			continue
		}
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		switch {
		case r == '\'' || r == '"':
			quote = r
			text.WriteRune(r)
		case r == '`' || (r == '$' && next == '(') || r == '(' || r == ')' || r == '{' && next == ' ':
			return nil, false
		case r == '&' && next == '&':
			flush("&&")
			i++
		case r == '|' && next == '|':
			flush("||")
			i++
		case r == '|':
			flush("|")
		case r == '&':
			if prev := strings.TrimRight(text.String(), " "); strings.HasSuffix(prev, ">") {
				text.WriteRune(r)
				continue
			}
			return nil, false // background job
		case r == ';' || r == '\n':
			flush(";")
		case r == '<':
			if next == '<' {
				return nil, false // heredoc
			}
			_, i = readWord(i + 1)
			i--
		case r == '>':
			// Drop an fd prefix such as the 2 in 2>&1.
			current := text.String()
			if n := len(current); n > 0 && current[n-1] >= '0' && current[n-1] <= '9' && (n == 1 || current[n-2] == ' ') {
				text.Reset()
				text.WriteString(current[:n-1])
			}
			j := i + 1
			if j < len(runes) && runes[j] == '>' {
				j++
			}
			if j < len(runes) && runes[j] == '&' {
				_, i = readWord(j + 1) // fd duplication, e.g. 2>&1
				i--
				continue
			}
			target, end := readWord(j)
			if target == "" {
				return nil, false
			}
			if target != "/dev/null" {
				writes = true
			}
			i = end - 1
		default:
			text.WriteRune(r)
		}
	}
	if quote != 0 || escaped {
		return nil, false
	}
	flush("")
	for _, segment := range segments {
		if segment.text == "" {
			return nil, false
		}
	}
	return segments, true
}
