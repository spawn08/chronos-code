package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap declares the interactive REPL's top-level key bindings — the ones
// intercepted before falling through to the input textarea's own bindings
// (see textarea.DefaultKeyMap, which otherwise treats "enter" as
// insert-newline and "up"/"down" as line navigation).
type keyMap struct {
	Submit         key.Binding
	HistoryPrev    key.Binding
	HistoryNext    key.Binding
	ReverseSearch  key.Binding
	Quit           key.Binding
	AgentPicker    key.Binding
	ModelPicker    key.Binding
	CommandPalette key.Binding
}

var keys = keyMap{
	Submit:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send message")),
	HistoryPrev:   key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "previous message")),
	HistoryNext:   key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "next message")),
	ReverseSearch: key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "search history")),
	Quit:          key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	AgentPicker: key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("ctrl+a", "agent picker")),
	// ModelPicker's ctrl+m binding can never actually fire: a terminal sends
	// the same \r byte for Ctrl+M as for Enter, and bubbletea v1.3.10 (see
	// go.mod) decodes \r unconditionally as KeyEnter — there is no Kitty
	// keyboard protocol support in this bubbletea version to disambiguate
	// them (nor any other mechanism this library offers). The picker itself
	// (newModelPicker) works; only this specific key trigger is dead until
	// bubbletea gains that support. /model and the ctrl+/ command palette
	// remain the reachable ways to switch models. See plan_07's AC-1/AC-3
	// notes in .ppd/chronos-code-v1/plans/plan_07.md.
	ModelPicker:    key.NewBinding(key.WithKeys("ctrl+m"), key.WithHelp("ctrl+m", "model picker (currently unreachable, see comment)")),
	CommandPalette: key.NewBinding(key.WithKeys("ctrl+/"), key.WithHelp("ctrl+/", "command palette")),
}

const helpText = `Commands:
  /agents            List all agents
  /agent <id>        Switch to agent
  /stream            Toggle streaming on/off
  /session           Show current session and recent history
  /memory            List remembered project/user/feedback notes
  /budget            Show token budget status
  /workspace         Show detected workspace info
  /clear             Clear screen
  /perf              Show frame timing stats (p50/p95/p99)
  /quit              Exit

  @<agent> <msg>     Send message to specific agent
  !<cmd>             Execute shell command

Keys:
  enter              Send message
  alt+enter, ctrl+j  Insert newline (queues as a follow-up if a turn is streaming)
  up / down          Recall previous/next message (single-line input only)
  ctrl+r             Search message history
  ctrl+a             Agent picker
  ctrl+/             Command palette (includes /model to switch models)
  ctrl+m             Model picker (not currently reachable — see /help in source; use /model or ctrl+/ instead)
  ctrl+c             Quit`
