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
- `/mouse` — opt out of wheel scrolling so unshifted drag-select works
- Ctrl+O — expand or collapse tool-call details
- `/resume`, `/compact`, `/rewind`, `/plan`, `/learn` — session, undo, plan mode, learning review

## Mouse & Scrolling (do not flip this default)
The transcript lives on the alt screen, so the terminal wheel does nothing
unless Bubble Tea captures mouse events.

- Default: `mouseCapture = true` (wheel scrolls). Copy = shift+drag, Cmd+C,
  Ctrl+Shift+C, or `/copy`.
- `/mouse` opts out for unshifted drag-select. Page Up/Down still scroll.
- Never default `mouseCapture` to false to "fix copy" — that regresses scrolling.
- Never enable `MouseModeAllMotion` — motion events make the TUI sluggish.
- While `followOutput` is false, skip stream viewport repaints so selection
  is not wiped. Ctrl+End resumes live output.

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
