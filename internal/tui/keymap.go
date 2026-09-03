package tui

import "charm.land/bubbles/v2/key"

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
	CopyLast       key.Binding
}

var keys = keyMap{
	Submit:         key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send message")),
	HistoryPrev:    key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "previous message")),
	HistoryNext:    key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "next message")),
	ReverseSearch:  key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "search history")),
	Quit:           key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	AgentPicker:    key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("ctrl+a", "agent picker")),
	ModelPicker:    key.NewBinding(key.WithKeys("ctrl+m"), key.WithHelp("ctrl+m", "model picker")),
	CommandPalette: key.NewBinding(key.WithKeys("ctrl+/"), key.WithHelp("ctrl+/", "command palette")),
	CopyLast:       key.NewBinding(key.WithKeys("ctrl+y"), key.WithHelp("ctrl+y", "copy last response")),
}

const helpText = `Commands:
  /agents            List all agents
  /agent <id>        Switch to agent
  /model [name]      Show or switch the active model
  /login [provider]  Configure provider authentication
  /logout <provider> Remove provider authentication
  /whoami [provider] Show authentication status
  /context           Show model context and usage
  /usage             Show token and USD usage
  /stream            Toggle streaming on/off
  /session           Show current session and recent history
  /memory            List remembered project/user/feedback notes
  /skills            List discovered skills and winning sources
  /<skill> <task>    Run a task with an explicitly selected skill
  /subagent <name> <task>
                     Run a registered subagent in an isolated context
  /subagent {JSON}   Run a dynamic subagent (task, system_prompt, tools)
  /budget            Show token budget status
  /workspace         Show detected workspace info
  /copy              Copy the last assistant response
  /clear             Start a new session and clear the screen
  /perf              Show frame timing stats (p50/p95/p99)
  /quit              Exit

  @<agent> <msg>     Send message to specific agent
  !<cmd>             Execute shell command

Keys:
  enter              Send; while running, interrupt and replace
  alt+enter          Queue a follow-up while running
  ctrl+j             Insert newline
  up / down          Select completion, otherwise recall message history
  ctrl+r             Search message history
  ctrl+y             Copy the last assistant response
  tab                Complete the selected slash command
  pgup / pgdown      Scroll conversation history
  ctrl+up / ctrl+down Scroll half a page
  ctrl+home / ctrl+end Jump to top / resume live output
  mouse wheel        Scroll conversation history
  shift+drag         Select visible terminal text for native copy
  cmd+v, ctrl+v      Paste text; multiline paste stays in the composer
  permission: y       Allow this call once
  permission: a       Always allow this tool in the current session
  permission: A       Allow all policy-approved tools in the current session
  ctrl+a             Agent picker
  ctrl+/             Command palette (includes /model to switch models)
  ctrl+m             Model picker (use /model or ctrl+/ if terminal key enhancements are unavailable)
  ctrl+c             Interrupt active work; quit while idle`
