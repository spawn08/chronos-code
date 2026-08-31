package tui

import "github.com/charmbracelet/lipgloss"

var (
	colorPrimary  = lipgloss.Color("#7C8CFF")
	colorAccent   = lipgloss.Color("#7C8CFF")
	colorAgent    = lipgloss.Color("#4EC9B0")
	colorUser     = lipgloss.Color("#569CD6")
	colorTool     = lipgloss.Color("#C586C0")
	colorError    = lipgloss.Color("#F44747")
	colorDim      = lipgloss.Color("#6A6A6A")
	colorSubtle   = lipgloss.Color("#3C3C3C")
	colorText     = lipgloss.Color("#D4D4D4")
	colorAdded    = lipgloss.Color("#4EC9B0")
	colorRemoved  = lipgloss.Color("#F44747")
	colorCodeBg   = lipgloss.Color("#1A1A1A")
	colorCodeFg   = lipgloss.Color("#CE9178")
	colorChromeBg = lipgloss.Color("#16161E")
)

var (
	styleUserPrefix = lipgloss.NewStyle().Bold(true).Foreground(colorUser)
	styleAgentName  = lipgloss.NewStyle().Bold(true).Foreground(colorAgent)
	styleTool       = lipgloss.NewStyle().Foreground(colorTool)
	styleError      = lipgloss.NewStyle().Bold(true).Foreground(colorError)
	styleDim        = lipgloss.NewStyle().Foreground(colorDim)

	styleHeader     = lipgloss.NewStyle().Bold(true).Foreground(colorPrimary)
	styleBold       = lipgloss.NewStyle().Bold(true)
	styleItalic     = lipgloss.NewStyle().Italic(true)
	styleInlineCode = lipgloss.NewStyle().Foreground(colorCodeFg).Background(colorCodeBg)
	styleCodeBlock  = lipgloss.NewStyle().Foreground(colorText).Background(colorCodeBg)
	styleCodeLang   = lipgloss.NewStyle().Italic(true).Foreground(colorDim)
	styleBlockquote = lipgloss.NewStyle().Italic(true).Foreground(colorDim)
	styleBullet     = lipgloss.NewStyle().Foreground(colorPrimary)

	styleDiffAdded   = lipgloss.NewStyle().Foreground(colorAdded)
	styleDiffRemoved = lipgloss.NewStyle().Foreground(colorRemoved)

	styleInputBox = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorSubtle).
			Padding(0, 1)
	styleModal = lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colorPrimary).
			Padding(1, 2)

	styleHeaderBar = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorText).
			Background(colorChromeBg)
	styleStatusLeft = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAgent).
			Background(colorChromeBg)
	styleStatusRight = lipgloss.NewStyle().
				Foreground(colorDim).
				Background(colorChromeBg)
	styleStatusFill = lipgloss.NewStyle().
			Background(colorChromeBg)

	styleSeparator = lipgloss.NewStyle().Foreground(colorSubtle)
	styleKeyHint   = lipgloss.NewStyle().Foreground(colorDim)
)
