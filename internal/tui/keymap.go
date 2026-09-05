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
	LoginWizard    key.Binding
	CommandPalette key.Binding
	CopyLast       key.Binding
	CopyCode       key.Binding
	ToggleTools    key.Binding
	Paste          key.Binding
}

var keys = keyMap{
	Submit:         key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "send message")),
	HistoryPrev:    key.NewBinding(key.WithKeys("up"), key.WithHelp("↑", "previous message")),
	HistoryNext:    key.NewBinding(key.WithKeys("down"), key.WithHelp("↓", "next message")),
	ReverseSearch:  key.NewBinding(key.WithKeys("ctrl+r"), key.WithHelp("ctrl+r", "search history")),
	Quit:           key.NewBinding(key.WithKeys("ctrl+c"), key.WithHelp("ctrl+c", "quit")),
	AgentPicker:    key.NewBinding(key.WithKeys("ctrl+a"), key.WithHelp("ctrl+a", "agent picker")),
	ModelPicker:    key.NewBinding(key.WithKeys("ctrl+m"), key.WithHelp("ctrl+m", "model picker")),
	LoginWizard:    key.NewBinding(key.WithKeys("ctrl+l"), key.WithHelp("ctrl+l", "login")),
	CommandPalette: key.NewBinding(key.WithKeys("ctrl+/"), key.WithHelp("ctrl+/", "command palette")),
	CopyLast:       key.NewBinding(key.WithKeys("ctrl+y", "ctrl+shift+c"), key.WithHelp("ctrl+shift+c", "copy last response")),
	CopyCode:       key.NewBinding(key.WithKeys("ctrl+shift+x"), key.WithHelp("ctrl+shift+x", "copy last code block")),
	ToggleTools:    key.NewBinding(key.WithKeys("ctrl+o"), key.WithHelp("ctrl+o", "expand or collapse tool calls")),
	Paste:          key.NewBinding(key.WithKeys("ctrl+v"), key.WithHelp("ctrl+v", "paste from clipboard")),
}

const helpText = `You are talking to Chronos Code (primary). Specialists run as subagents unless you @mention or /agent switch.

Commands:
  /agents            List all agents (* active, primary marked)
  /agent <id>        Switch to a specialist (or back to chronos-code)
  /model [name]      Show or switch the active model
  /think [level]     Show or set native thinking (off|low|medium|high)
  /login [provider]  Sign in (Claude Code / Codex / API key / enterprise OAuth)
  /logout <provider> Remove provider authentication
  /whoami [provider] Show authentication status
  /context           Show model context and usage
  /usage             Show token and USD usage
  /stream            Toggle streaming on/off
  /session           Show current session and recent history
  /resume [id]       Resume the latest (or given) session
  /compact           Summarize session history and reset the token budget
  /rewind            Undo the last file_write (alias: /undo)
  /plan [on|off]     Plan-only mode: block writes and shell
  /learn             List pending learning suggestions
  /learn accept <id> Apply a pending suggestion (next start)
  /learn reject <id> Discard a pending suggestion
  /sandbox           Show OS sandbox helper status
  /memory            List remembered project/user/feedback notes
  /skills            List discovered skills and winning sources
  /mcp               List discovered MCP servers and connection status
  /mcp connect <name>
                     Approve and connect a discovered MCP server for this session
  /<skill> <task>    Run a task with an explicitly selected skill
  /subagent <name> <task>
                     Run a registered subagent in an isolated context
  /subagent {JSON}   Run a dynamic subagent (task, system_prompt, tools)
  /budget            Show token budget status
  /workspace         Show detected workspace info
  /copy              Copy the last assistant response
  /copy visible      Copy the currently visible transcript
  /copy all          Copy the full conversation transcript
  /copy code [n]     Copy the last (or nth) fenced code block
  /mouse             Opt out of wheel scrolling so unshifted drag-select works
  /clear             Start a new session and clear the screen
  /perf              Show frame timing stats (p50/p95/p99)
  /quit              Exit

  @<agent> <msg>     Send message to a specific agent
  @<path>            Attach a workspace file to the message
  !<cmd>             Run a local shell command in the workspace (output in chat)

Keys:
  enter              Send; while running, interrupt and replace
  alt+enter          Queue a follow-up while running
  ctrl+j             Insert newline
  up / down          Select completion, otherwise recall message history
  ctrl+r             Search message history
  ctrl+y, ctrl+shift+c Copy the last assistant response (visible output if none)
  ctrl+shift+x       Copy the last fenced code block from the reply
  ctrl+o             Expand or collapse tool-call details
  tab                Complete the selected slash command, /model, agent, or @file
  mouse wheel        Scroll conversation history
  pgup / pgdown      Scroll conversation history
  ctrl+up / ctrl+down Scroll half a page
  ctrl+home / ctrl+end Jump to top / resume live output
  shift+drag         Select output, then copy with the terminal (cmd+c)
  /mouse             Disable wheel capture for unshifted drag-select
  cmd+c               Copy selected text with the terminal
  cmd+v, ctrl+v      Native clipboard paste; multiline stays in the composer
  permission: y       Allow this call once
  permission: a       Always allow this tool in the current session
  permission: A       Allow all policy-approved tools in the current session
  ctrl+a             Agent picker
  ctrl+m             Model picker (assigns a model to the current agent)
  ctrl+l             Login (Claude Code, Codex subscription, API key, enterprise OAuth)
  ctrl+/             Command palette
  ctrl+c             Interrupt active work; quit while idle`
