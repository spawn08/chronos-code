---
name: bubbletea-tui
description: BubbleTea TUI patterns for chronos-code — rendering, input, streaming, mouse, scrolling
version: 1.0.0
triggers: [tui, bubbletea, render, input, scroll, mouse, terminal, display, lipgloss, bubbles]
model_hint: sonnet
tools_required: [file_read, file_write, file_grep, shell]
---
# BubbleTea TUI Skill

## Library Versions (v2)
- `charm.land/bubbletea/v2` — the Elm architecture framework
- `charm.land/bubbles/v2` — standard components (textinput, viewport, etc.)
- `charm.land/lipgloss/v2` — styling and layout

## Architecture
- Main model: `internal/tui/app.go` — the root `tea.Model`
- Autocomplete: `internal/tui/autocomplete.go` — input completion for commands, agents, skills
- Picker: `internal/tui/picker.go` — slash command picker UI

## Key Patterns
1. **Msg/Update/View cycle**: all state changes go through Update; View is pure rendering
2. **Cmd batching**: return `tea.Batch(cmds...)` not sequential sends
3. **No goroutines in Update**: use `tea.Cmd` functions that return `tea.Msg`
4. **Differential rendering**: minimize full redraws; update only changed regions

## Slash Commands
Commands are dispatched in `app.go`'s Update method. Available:
- `/skills` — list discovered skill catalog
- `/agents` — list registered agents
- `/context` — show context sources, budgets, omission reasons
- `/copy` / Ctrl+Y / Ctrl+Shift+C — clipboard (last response, or visible output)
- `/copy visible`, `/copy all` — copy the on-screen pane or full transcript
- `/copy code` / Ctrl+Shift+X — copy the last fenced code block
- `/mouse` — toggle mouse-wheel scrolling; drag-select is the default
- Ctrl+O — expand or collapse tool-call details
- `/resume`, `/compact`, `/rewind`, `/plan`, `/learn` — session, undo, plan mode, learning review

## Mouse & Scrolling
- Mouse capture starts DISABLED so terminal drag-select and Cmd+C work
- `/mouse` enables wheel scrolling of the transcript (then use shift+drag to select)
- Page Up/Down, Ctrl+Home/End always available

## Streaming Display
- Agent responses stream token-by-token
- Tool calls render in collapsed sections
- Subagent progress shows in nested collapsed views

## Performance Targets
- <16ms frame time (60fps)
- No flicker on re-layout
- Works over SSH (no Kitty-only features required)

## Adding a New Slash Command
1. Add the command string to the picker list in `picker.go`
2. Add the `case "/yourcommand":` handler in `app.go`'s command dispatch
3. Add autocomplete support in `autocomplete.go` if needed
4. Add to `/help` output
