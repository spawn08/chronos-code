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
	styleCodeBlock   = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(colorCodeBg)
	styleCodeLang    = lipgloss.NewStyle().Italic(true).Foreground(colorDim)
	styleBlockquote  = lipgloss.NewStyle().Italic(true).Foreground(colorDim)
	styleBullet      = lipgloss.NewStyle().Foreground(colorAccent)
	styleDiffAdded   = lipgloss.NewStyle().Foreground(colorAdded)
	styleDiffRemoved = lipgloss.NewStyle().Foreground(colorRemoved)
	styleInputBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorAccent).Padding(0, 1)
	styleModal       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorError).Padding(1, 2)

	// Header/status bar segments are rendered by directly concatenating
	// already-fully-sized strings (see app.go's renderHeaderBar/
	// renderStatusBar) rather than lipgloss's own Width()-based auto-fill —
	// mixing manual width bookkeeping with Width()'s "content area excludes
	// padding, but the total includes it" rule is exactly what caused the
	// status bar to overflow by its own padding and wrap onto a second line,
	// corrupting the fixed-height layout below it. These styles therefore
	// intentionally carry no Width()/Padding of their own.
	styleHeaderBar   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(colorAccent)
	styleStatusLeft  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("16")).Background(colorAccent)
	styleStatusRight = lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Background(lipgloss.Color("238"))
	styleStatusFill  = lipgloss.NewStyle().Background(lipgloss.Color("238"))
)
