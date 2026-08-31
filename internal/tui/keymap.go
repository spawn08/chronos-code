package tui

import "github.com/charmbracelet/bubbles/key"

// keyMap declares the interactive REPL's top-level key bindings — the ones
// intercepted before falling through to the input textarea's own bindings
// (see textarea.DefaultKeyMap, which otherwise treats "enter" as
// insert-newline and "up"/"down" as line navigation).
type keyMap struct {
	Submit        key.Binding
	HistoryPrev   key.Binding
	HistoryNext   key.Binding
	ReverseSearch key.Binding
	Quit          key.Binding
}

var keys = keyMap{
	Submit:        key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send message")),
	HistoryPrev:   key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "previous message")),
	HistoryNext:   key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "next message")),
	ReverseSearch: key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "search history")),
	Quit:          key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
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
  /quit              Exit

  @<agent> <msg>     Send message to specific agent
  !<cmd>             Execute shell command

Keys:
  enter              Send message
  alt+enter, ctrl+j  Insert newline
  up / down          Recall previous/next message (single-line input only)
  ctrl+r             Search message history
  ctrl+c             Quit`
