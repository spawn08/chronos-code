package tui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/orchestrator"
)

// autoRemember best-effort extracts a standing instruction/correction from
// the user's message (PRD P2-002's heuristic auto-extraction) and saves it to
// the orchestrator's memory store. Errors and a disabled/nil memory store are
// both silently ignored — this is a background convenience, never something
// that should interrupt the conversation.
func autoRemember(orch *orchestrator.Orchestrator, message string) {
	store := orch.MemoryStore()
	if store == nil {
		return
	}
	if category, content, ok := memory.ExtractFromMessage(message); ok {
		_, _ = store.Add(category, content)
	}
}

type REPL struct {
	orch    *orchestrator.Orchestrator
	scanner *bufio.Scanner
	reader  *bufio.Reader
	stream  bool
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewREPL(orch *orchestrator.Orchestrator, stream bool) *REPL {
	ctx, cancel := context.WithCancel(context.Background())
	// Both the main input loop and the tool-approval prompt (installed by
	// installApprovalHandlers, via InteractiveApproval) must read from a
	// single shared *bufio.Reader over os.Stdin. Two independent buffered
	// readers over the same os.Stdin can each over-read into their own
	// buffer, so a line typed for one can be silently consumed by the other,
	// hanging or misreading the next prompt.
	reader := bufio.NewReader(os.Stdin)
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	return &REPL{
		orch:    orch,
		scanner: scanner,
		reader:  reader,
		stream:  stream,
		ctx:     ctx,
		cancel:  cancel,
	}
}

func (r *REPL) Start() error {
	r.printBanner()
	r.installApprovalHandlers()

	for {
		agentID := r.orch.ActiveID()
		fmt.Printf("\033[1m%s>\033[0m ", agentID)

		if !r.scanner.Scan() {
			break
		}
		line := strings.TrimSpace(r.scanner.Text())
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "/") {
			if quit := r.handleSlashCommand(line); quit {
				break
			}
			continue
		}

		if strings.HasPrefix(line, "!") {
			r.handleShellEscape(line[1:])
			continue
		}

		if strings.HasPrefix(line, "@") {
			parts := strings.SplitN(line[1:], " ", 2)
			if len(parts) == 2 {
				if err := r.orch.SwitchAgent(parts[0]); err != nil {
					fmt.Fprintf(os.Stderr, "error: %v\n", err)
					continue
				}
				line = parts[1]
			}
		} else if agentID, matched := r.orch.Route(r.ctx, line); matched {
			_ = r.orch.SwitchAgent(agentID)
		}

		autoRemember(r.orch, line)

		if err := r.sendMessage(line); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		if status := r.orch.BudgetStatusLine(); status != "" {
			fmt.Fprintf(os.Stderr, "\033[2m%s\033[0m\n", status)
		}
	}

	r.cancel()
	return nil
}

func (r *REPL) sendMessage(message string) error {
	if r.stream {
		ch, err := r.orch.ChatStream(r.ctx, message)
		if err != nil {
			return err
		}
		usage, err := StreamResponse(ch, os.Stdout)
		if err != nil {
			return err
		}
		PrintUsage(usage, os.Stderr)
		return nil
	}

	resp, err := r.orch.Chat(r.ctx, message)
	if err != nil {
		return err
	}
	PrintResponse(resp, os.Stdout)
	PrintUsage(resp.Usage, os.Stderr)
	return nil
}

func (r *REPL) handleSlashCommand(line string) (quit bool) {
	parts := strings.SplitN(line, " ", 2)
	cmd := strings.ToLower(parts[0])
	arg := ""
	if len(parts) > 1 {
		arg = strings.TrimSpace(parts[1])
	}

	switch cmd {
	case "/quit", "/exit", "/q":
		fmt.Println("goodbye")
		return true
	case "/help", "/h":
		r.printHelp()
	case "/agents":
		for _, id := range r.orch.ListAgents() {
			marker := "  "
			if id == r.orch.ActiveID() {
				marker = "* "
			}
			a, _ := r.orch.GetAgent(id)
			fmt.Printf("%s%s — %s\n", marker, id, a.Name)
		}
	case "/agent":
		if arg == "" {
			fmt.Printf("active: %s\n", r.orch.ActiveID())
			return false
		}
		if err := r.orch.SwitchAgent(arg); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		} else {
			fmt.Printf("switched to %s\n", arg)
		}
	case "/stream":
		r.stream = !r.stream
		fmt.Printf("streaming: %v\n", r.stream)
	case "/clear":
		fmt.Print("\033[H\033[2J")
	case "/session":
		fmt.Printf("current session: %s\n", r.orch.CurrentSessionID())
		if sessions, err := r.orch.SessionManager().List(context.Background(), r.orch.ActiveID(), 10, 0); err == nil {
			for _, s := range sessions {
				fmt.Printf("  %s  %-10s updated %s\n", s.ID, s.Status, s.UpdatedAt.Format("2006-01-02 15:04"))
			}
		}
	case "/memory":
		store := r.orch.MemoryStore()
		if store == nil {
			fmt.Println("memory is disabled")
			return false
		}
		records, err := store.List("")
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return false
		}
		if len(records) == 0 {
			fmt.Println("no memory records")
			return false
		}
		for _, rec := range records {
			fmt.Printf("  %s  [%-8s] %s\n", rec.ID, rec.Category, rec.Content)
		}
	case "/budget":
		if status := r.orch.BudgetStatusLine(); status != "" {
			fmt.Println(status)
		}
	case "/workspace":
		if ws := r.orch.Workspace(); ws != nil {
			fmt.Println(ws.Banner())
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s (try /help)\n", cmd)
	}
	return false
}

func (r *REPL) handleShellEscape(cmd string) {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return
	}
	c := exec.CommandContext(r.ctx, "bash", "-c", cmd)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	if err := c.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "shell: %v\n", err)
	}
}

func (r *REPL) installApprovalHandlers() {
	handler := InteractiveApproval(r.reader, os.Stderr)
	for _, id := range r.orch.ListAgents() {
		if a, ok := r.orch.GetAgent(id); ok {
			a.Tools.SetApprovalHandler(handler)
		}
	}
}

func (r *REPL) printBanner() {
	fmt.Println("\033[1mchronos-code\033[0m — AI coding agent harness")
	fmt.Printf("active agent: %s | type /help for commands\n\n", r.orch.ActiveID())
}

func (r *REPL) printHelp() {
	fmt.Print(`Commands:
  /agents            List all agents
  /agent <id>        Switch to agent
  /stream            Toggle streaming on/off
  /session           Show current session and recent history
  /memory            List remembered project/user/feedback notes
  /budget            Show token budget status
  /workspace         Show detected workspace info
  /clear             Clear screen
  /quit              Exit

  @<agent> <msg>     Send message to specific agent
  !<cmd>             Execute shell command
`)
}
