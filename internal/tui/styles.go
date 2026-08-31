package tui

import "github.com/charmbracelet/lipgloss"

// Theme colors. Kept in one place so the interactive REPL reads as one
// consistent visual system instead of the old ad hoc ANSI escape codes
// scattered across display.go/permission.go.
var (
	colorUser    = lipgloss.Color("39")  // blue
	colorAccent  = lipgloss.Color("214") // gold — agent name, headers, borders
	colorTool    = lipgloss.Color("36")  // cyan — tool-call lines
	colorError   = lipgloss.Color("196") // red
	colorDim     = lipgloss.Color("240") // gray — status text, blockquotes, code blocks
	colorAdded   = lipgloss.Color("42")  // green — diff additions
	colorRemoved = lipgloss.Color("203") // red-orange — diff removals
	colorCodeBg  = lipgloss.Color("236")
	colorCodeFg  = lipgloss.Color("219")
)

var (
	styleUserPrefix  = lipgloss.NewStyle().Bold(true).Foreground(colorUser)
	styleAgentName   = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleTool        = lipgloss.NewStyle().Foreground(colorTool)
	styleError       = lipgloss.NewStyle().Bold(true).Foreground(colorError)
	styleDim         = lipgloss.NewStyle().Foreground(colorDim)
	styleHeader      = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleBold        = lipgloss.NewStyle().Bold(true)
	styleItalic      = lipgloss.NewStyle().Italic(true)
	styleInlineCode  = lipgloss.NewStyle().Foreground(colorCodeFg).Background(colorCodeBg)
	styleCodeBlock   = lipgloss.NewStyle().Foreground(colorDim)
	styleBlockquote  = lipgloss.NewStyle().Italic(true).Foreground(colorDim)
	styleBullet      = lipgloss.NewStyle().Foreground(colorAccent)
	styleDiffAdded   = lipgloss.NewStyle().Foreground(colorAdded)
	styleDiffRemoved = lipgloss.NewStyle().Foreground(colorRemoved)
	styleStatusBar   = lipgloss.NewStyle().Foreground(lipgloss.Color("235")).Background(lipgloss.Color("252")).Padding(0, 1)
	styleInputBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorAccent).Padding(0, 1)
	styleModal       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorError).Padding(1, 2)
)
