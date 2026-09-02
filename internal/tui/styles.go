package tui

import "charm.land/lipgloss/v2"

type terminalStyle struct {
	lipgloss.Style
}

func newTerminalStyle(style lipgloss.Style) terminalStyle {
	return terminalStyle{Style: style}
}

func (s terminalStyle) Render(strs ...string) string {
	// Lip Gloss v2 emits true color from Style.Render; Sprint restores the
	// output-profile filtering that v1 applied during rendering.
	return lipgloss.Sprint(s.Style.Render(strs...))
}

func (s terminalStyle) Width(width int) terminalStyle {
	s.Style = s.Style.Width(width)
	return s
}

var (
	colorPrimary  = lipgloss.Color("#7C8CFF")
	colorAccent   = lipgloss.Color("#7C8CFF")
	colorAgent    = lipgloss.Color("#4EC9B0")
	colorUser     = lipgloss.Color("#569CD6")
	colorTool     = lipgloss.Color("#C586C0")
	colorError    = lipgloss.Color("#F44747")
	colorDim      = lipgloss.Color("#9A9A9A")
	colorSubtle   = lipgloss.Color("#3C3C3C")
	colorText     = lipgloss.Color("#D4D4D4")
	colorAdded    = lipgloss.Color("#4EC9B0")
	colorRemoved  = lipgloss.Color("#F44747")
	colorCodeBg   = lipgloss.Color("#1A1A1A")
	colorCodeFg   = lipgloss.Color("#CE9178")
	colorChromeBg = lipgloss.Color("#16161E")
)

var (
	styleUserPrefix = newTerminalStyle(lipgloss.NewStyle().Bold(true).Foreground(colorUser))
	styleAgentName  = newTerminalStyle(lipgloss.NewStyle().Bold(true).Foreground(colorAgent))
	styleTool       = newTerminalStyle(lipgloss.NewStyle().Foreground(colorTool))
	styleError      = newTerminalStyle(lipgloss.NewStyle().Bold(true).Foreground(colorError))
	styleDim        = newTerminalStyle(lipgloss.NewStyle().Foreground(colorDim))

	styleHeader     = newTerminalStyle(lipgloss.NewStyle().Bold(true).Foreground(colorPrimary))
	styleBold       = newTerminalStyle(lipgloss.NewStyle().Bold(true))
	styleItalic     = newTerminalStyle(lipgloss.NewStyle().Italic(true))
	styleInlineCode = newTerminalStyle(lipgloss.NewStyle().Foreground(colorCodeFg).Background(colorCodeBg))
	styleCodeBlock  = newTerminalStyle(lipgloss.NewStyle().Foreground(colorText).Background(colorCodeBg))
	styleCodeLang   = newTerminalStyle(lipgloss.NewStyle().Italic(true).Foreground(colorDim))
	styleBlockquote = newTerminalStyle(lipgloss.NewStyle().Italic(true).Foreground(colorDim))
	styleBullet     = newTerminalStyle(lipgloss.NewStyle().Foreground(colorPrimary))

	styleDiffAdded   = newTerminalStyle(lipgloss.NewStyle().Foreground(colorAdded))
	styleDiffRemoved = newTerminalStyle(lipgloss.NewStyle().Foreground(colorRemoved))

	styleInputBox = newTerminalStyle(lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(colorSubtle).
			Padding(0, 1))
	styleModal = newTerminalStyle(lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(colorPrimary).
			Padding(1, 2))
	styleApprovalModal = newTerminalStyle(lipgloss.NewStyle().
				Border(lipgloss.DoubleBorder()).
				BorderForeground(colorPrimary).
				Padding(0, 1))

	styleHeaderBar = newTerminalStyle(lipgloss.NewStyle().
			Bold(true).
			Foreground(colorText).
			Background(colorChromeBg))
	styleStatusLeft = newTerminalStyle(lipgloss.NewStyle().
			Bold(true).
			Foreground(colorAgent).
			Background(colorChromeBg))
	styleStatusRight = newTerminalStyle(lipgloss.NewStyle().
				Foreground(colorDim).
				Background(colorChromeBg))
	styleStatusFill = newTerminalStyle(lipgloss.NewStyle().
			Background(colorChromeBg))

	styleSeparator = newTerminalStyle(lipgloss.NewStyle().Foreground(colorSubtle))
	styleKeyHint   = newTerminalStyle(lipgloss.NewStyle().Foreground(colorDim))
)
