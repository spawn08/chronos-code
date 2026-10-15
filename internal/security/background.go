package security

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/storage"
)

// Background shells: a command that is still running when its yield point
// passes is not killed. Control returns to the model with a shell id while the
// process keeps running and streams its output to a log file. await_shell
// waits for exit or an output pattern; kill_shell stops it. No wall clock ends
// a background command, so long builds, test suites and migrations finish;
// stuck ones are visible through their idle time and the model decides.
const (
	// defaultYield is how long shell blocks before backgrounding a command.
	defaultYield = 30 * time.Second
	// defaultAwait bounds one await_shell call, not the process.
	defaultAwait = 2 * time.Minute
	maxAwait     = 10 * time.Minute
	// shellOutputTail bounds the output returned by one call; the full log
	// stays in log_path.
	shellOutputTail = 30 << 10
)

// BackgroundShells tracks the background processes of every session.
type BackgroundShells struct {
	dir string // log directory

	mu     sync.Mutex
	next   int
	shells map[string]*backgroundShell // id -> shell
}

type backgroundShell struct {
	id       string
	session  string
	command  string
	dir      string
	logPath  string
	cmd      *exec.Cmd
	started  time.Time
	output   *shellOutput
	done     chan struct{}
	exitCode int
	exitErr  error

	readMu    sync.Mutex // serializes reads that advance readCount
	readCount int        // output bytes already returned to the model
}

// patternOverlap is how far before the last scanned byte a pattern scan
// restarts, so a match spanning two ticks' output is still found.
const patternOverlap = 4 << 10

// unreadFrom returns unread output from max(offset, readCount) and the total
// length, so repeated pattern scans read only new bytes.
func (sh *backgroundShell) unreadFrom(offset int) (string, int) {
	sh.readMu.Lock()
	defer sh.readMu.Unlock()
	text, total, _ := sh.output.since(max(offset, sh.readCount))
	return text, total
}

// NewBackgroundShells stores logs under dir (created on first use).
func NewBackgroundShells(dir string) *BackgroundShells {
	return &BackgroundShells{dir: dir, shells: make(map[string]*backgroundShell)}
}

// shellOutput is an append-only, concurrency-safe output buffer that also
// streams to the shell's log file and records when output last arrived.
type shellOutput struct {
	mu       sync.Mutex
	buf      []byte // the last shellBufferLimit bytes; the log file has all
	base     int    // absolute offset of buf[0] in the whole output
	file     *os.File
	lastByte time.Time
}

// exitedShellRetention is how long an exited shell stays awaitable before it
// is forgotten.
const exitedShellRetention = 10 * time.Minute

// shellBufferLimit bounds the output kept in memory per shell. A call never
// returns more than shellOutputTail, so older bytes are only in log_path.
const shellBufferLimit = 4 * shellOutputTail

func (o *shellOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.buf = append(o.buf, p...)
	if drop := len(o.buf) - shellBufferLimit; drop > 0 {
		o.buf = append(o.buf[:0], o.buf[drop:]...)
		o.base += drop
	}
	o.lastByte = time.Now()
	if o.file != nil {
		_, _ = o.file.Write(p)
	}
	return len(p), nil
}

// since returns output after absolute byte offset n (from the oldest byte
// still in memory when n is older) and the new total length.
func (o *shellOutput) since(n int) (string, int, time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	total := o.base + len(o.buf)
	start := min(max(n-o.base, 0), len(o.buf))
	return string(o.buf[start:]), total, o.lastByte
}

// start runs command in dir and returns once it exits or yield passes,
// whichever is first. A yield of 0 returns immediately.
func (b *BackgroundShells) start(ctx context.Context, command, dir string, yield time.Duration) (map[string]any, error) {
	if err := os.MkdirAll(b.dir, 0o755); err != nil {
		return nil, fmt.Errorf("shell: create log directory: %w", err)
	}
	b.mu.Lock()
	b.next++
	id := "sh-" + strconv.Itoa(b.next)
	b.mu.Unlock()
	logPath := filepath.Join(b.dir, id+".log")
	file, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("shell: create log: %w", err)
	}
	fmt.Fprintf(file, "# command: %s\n# cwd: %s\n# started: %s\n", command, dir, time.Now().Format(time.RFC3339))
	out := &shellOutput{file: file, lastByte: time.Now()}
	// The process outlives this call, so it is not bound to the request
	// context; kill_shell and Close end it.
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = dir
	configureProcessGroup(cmd)
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		file.Close()
		return nil, fmt.Errorf("shell: %w", err)
	}
	sh := &backgroundShell{id: id, session: storage.SessionFromContext(ctx), command: command, dir: dir,
		logPath: logPath, cmd: cmd, started: time.Now(), output: out, done: make(chan struct{})}
	b.mu.Lock()
	b.shells[id] = sh
	b.mu.Unlock()
	go func() {
		err := cmd.Wait()
		sh.exitErr, sh.exitCode = err, exitCode(err)
		out.mu.Lock()
		fmt.Fprintf(file, "\n# exit_code: %d\n# elapsed_ms: %d\n", sh.exitCode, time.Since(sh.started).Milliseconds())
		file.Close()
		out.file = nil
		out.mu.Unlock()
		close(sh.done)
		// A shell whose exit nobody awaits (the turn was interrupted) is
		// dropped after a grace period; its full output stays in log_path.
		time.AfterFunc(exitedShellRetention, func() { b.forget(id) })
	}()

	timer := time.NewTimer(yield)
	defer timer.Stop()
	select {
	case <-sh.done:
		return b.report(sh, true), nil
	case <-timer.C:
	case <-ctx.Done():
		// The turn was interrupted; the command keeps running in the
		// background and the next turn can await or kill it.
	}
	return b.report(sh, false), nil
}

// report renders a shell's state and the output not yet returned. A finished
// shell is forgotten once its final output has been reported.
func (b *BackgroundShells) report(sh *backgroundShell, final bool) map[string]any {
	// Decide exit before reading: output written up to exit is then all in
	// the buffer, so an exited result never drops the final lines.
	exited := false
	select {
	case <-sh.done:
		exited = true
	default:
	}
	sh.readMu.Lock()
	text, total, last := sh.output.since(sh.readCount)
	sh.readCount = total
	sh.readMu.Unlock()
	result := map[string]any{
		"shell_id":   sh.id,
		"log_path":   sh.logPath,
		"elapsed_ms": time.Since(sh.started).Milliseconds(),
	}
	text, truncated := tailOutput(text)
	result["output"] = text
	if truncated {
		result["output_truncated"] = true
	}
	if exited {
		result["status"] = "exited"
		result["exit_code"] = sh.exitCode
		if final {
			b.forget(sh.id)
		}
	} else {
		result["status"] = "running"
		result["idle_ms"] = time.Since(last).Milliseconds()
		result["hint"] = "Still running in the background. Use await_shell with this shell_id (optionally a pattern) to wait for it, or kill_shell to stop it. Do not poll with sleep or by re-reading the log."
	}
	return result
}

func (b *BackgroundShells) forget(id string) {
	b.mu.Lock()
	delete(b.shells, id)
	b.mu.Unlock()
}

func (b *BackgroundShells) get(ctx context.Context, id string) (*backgroundShell, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sh, ok := b.shells[id]
	if !ok || sh.session != storage.SessionFromContext(ctx) {
		return nil, fmt.Errorf("no background shell %q in this session", id)
	}
	return sh, nil
}

// await blocks until the shell exits, its new output matches pattern, the
// timeout passes, or ctx ends.
func (b *BackgroundShells) await(ctx context.Context, id string, pattern *regexp.Regexp, timeout time.Duration) (map[string]any, error) {
	sh, err := b.get(ctx, id)
	if err != nil {
		return nil, err
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	scanned := 0 // absolute offset already scanned for pattern
	for {
		select {
		case <-sh.done:
			return b.report(sh, true), nil
		case <-deadline.C:
			return b.report(sh, false), nil
		case <-ctx.Done():
			return b.report(sh, false), nil
		case <-tick.C:
			if pattern != nil {
				text, total := sh.unreadFrom(scanned - patternOverlap)
				scanned = total
				if pattern.MatchString(text) {
					r := b.report(sh, false)
					r["matched"] = true
					return r, nil
				}
			}
		}
	}
}

func (b *BackgroundShells) kill(ctx context.Context, id string) (map[string]any, error) {
	sh, err := b.get(ctx, id)
	if err != nil {
		return nil, err
	}
	killProcessGroup(sh.cmd)
	select {
	case <-sh.done:
	case <-time.After(5 * time.Second):
	}
	r := b.report(sh, true)
	r["killed"] = true
	return r, nil
}

// Running lists a session's background shells for the per-turn context.
func (b *BackgroundShells) Running(sessionID string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var lines []string
	for _, sh := range b.shells {
		if sh.session != sessionID {
			continue
		}
		state := "running"
		select {
		case <-sh.done:
			state = fmt.Sprintf("exited %d (output not yet read)", sh.exitCode)
		default:
		}
		_, _, last := sh.output.since(0)
		lines = append(lines, fmt.Sprintf("%s %q %s for %s, last output %s ago", sh.id, sh.command, state,
			time.Since(sh.started).Round(time.Second), time.Since(last).Round(time.Second)))
	}
	sort.Strings(lines)
	return lines
}

// Close kills every background process; call it when the session host exits.
func (b *BackgroundShells) Close() {
	b.mu.Lock()
	shells := make([]*backgroundShell, 0, len(b.shells))
	for _, sh := range b.shells {
		shells = append(shells, sh)
	}
	b.shells = map[string]*backgroundShell{}
	b.mu.Unlock()
	for _, sh := range shells {
		killProcessGroup(sh.cmd)
	}
}

// tailOutput keeps the last shellOutputTail bytes.
func tailOutput(s string) (string, bool) {
	if len(s) <= shellOutputTail {
		return s, false
	}
	cut := len(s) - shellOutputTail
	for cut < len(s) && s[cut]&0xC0 == 0x80 {
		cut++
	}
	return "…[earlier output in log_path]\n" + s[cut:], true
}

// NewAwaitShellTool waits on a background shell without polling.
func NewAwaitShellTool(shells *BackgroundShells) *tool.Definition {
	return &tool.Definition{
		Name: "await_shell",
		Description: "Wait for a background shell started by shell. Returns when the process exits, when new output matches pattern, or after timeout_ms (default 120000, max 600000), whichever is first; the process keeps running after a timeout. " +
			"Returns only output produced since your last read, plus status, exit_code when exited, and idle_ms (time since the last output). A long idle_ms with no exit can mean a hang or a prompt waiting for input.",
		Permission:   tool.PermAllow,
		ParallelSafe: true,
		Effects:      []tool.Effect{tool.EffectRead},
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"shell_id":   map[string]any{"type": "string"},
				"pattern":    map[string]any{"type": "string", "description": "Regular expression; return as soon as new output matches (e.g. \"PASS|FAIL\", \"listening on\")"},
				"timeout_ms": map[string]any{"type": "integer", "minimum": 1000, "maximum": maxAwait.Milliseconds()},
			},
			"required": []string{"shell_id"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, _ := args["shell_id"].(string)
			if id == "" {
				return nil, fmt.Errorf("await_shell: 'shell_id' is required")
			}
			var pattern *regexp.Regexp
			if p, _ := args["pattern"].(string); p != "" {
				re, err := regexp.Compile(p)
				if err != nil {
					return nil, fmt.Errorf("await_shell: invalid pattern: %w", err)
				}
				pattern = re
			}
			timeout := defaultAwait
			if ms, ok := numberArg(args["timeout_ms"]); ok && ms > 0 {
				timeout = min(time.Duration(ms)*time.Millisecond, maxAwait)
			}
			result, err := shells.await(ctx, id, pattern, timeout)
			if err != nil {
				return nil, fmt.Errorf("await_shell: %w", err)
			}
			return result, nil
		},
	}
}

// NewKillShellTool stops a background shell's process group.
func NewKillShellTool(shells *BackgroundShells) *tool.Definition {
	return &tool.Definition{
		Name:        "kill_shell",
		Description: "Stop a background shell started by shell (its whole process group). Use it for a hung command or a server or watcher you no longer need. Returns the final output.",
		Permission:  tool.PermAllow,
		Effects:     []tool.Effect{tool.EffectProcessExecution},
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"shell_id": map[string]any{"type": "string"}},
			"required":   []string{"shell_id"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, _ := args["shell_id"].(string)
			if id == "" {
				return nil, fmt.Errorf("kill_shell: 'shell_id' is required")
			}
			result, err := shells.kill(ctx, id)
			if err != nil {
				return nil, fmt.Errorf("kill_shell: %w", err)
			}
			return result, nil
		},
	}
}

func numberArg(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}
