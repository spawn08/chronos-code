package tui

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spawn08/chronos-code/internal/orchestrator"
)

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
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	return &REPL{
		orch:    orch,
		scanner: scanner,
		reader:  bufio.NewReader(os.Stdin),
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
		}

		if err := r.sendMessage(line); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
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
  /clear             Clear screen
  /quit              Exit

  @<agent> <msg>     Send message to specific agent
  !<cmd>             Execute shell command
`)
}
