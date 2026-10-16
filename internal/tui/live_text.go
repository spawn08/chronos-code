package tui

import (
	"strings"
	"unicode/utf8"
)

const liveTextRenderBytes = 2 * maxItemRenderBytes

// liveTextCache caches hard-newline-delimited plain text, not Markdown. Keep one
// value per turnItem and call wrap with the raw text. The zero value is ready.
// ANSI styles, links, and incomplete parser input use the full wrapper because
// lipgloss carries state across newlines and closes that state at end of input.
type liveTextCache struct {
	source         string
	width          int
	valid          bool
	plain          bool
	completedBytes int
	completedLines int
	completed      string
	rendered       string
}

// wrap matches wrapText on the existing bounded item tail, then applies the
// render retention limits. It never retains a view onto a streaming builder.
func (c *liveTextCache) wrap(text string, width int) string {
	text = boundedTextTail(text, maxItemRenderBytes, maxViewportLines)
	if c.valid && c.width == width && c.source == text {
		return c.rendered
	}

	appendOnly := c.valid && c.width == width && strings.HasPrefix(text, c.source)
	plain := false
	if appendOnly && c.plain {
		plain = liveTextPlain(text[len(c.source):])
	} else {
		plain = liveTextPlain(text)
	}
	if !appendOnly || !plain {
		c.completedBytes, c.completedLines, c.completed = 0, 0, ""
	}

	var rendered string
	if width <= 0 {
		c.completedBytes, c.completedLines, c.completed = 0, 0, ""
		rendered = strings.Clone(text)
	} else if plain {
		end := strings.LastIndexByte(text, '\n') + 1
		if end > c.completedBytes {
			segment := wrapText(text[c.completedBytes:end], width)
			lines := c.completedLines + strings.Count(segment, "\n")
			if len(c.completed)+len(segment) <= liveTextRenderBytes && lines < maxViewportLines {
				c.completed += segment
				c.completedBytes, c.completedLines = end, lines
			} else {
				// A narrow viewport can exceed the line budget before the byte budget.
				c.completedBytes, c.completedLines, c.completed = 0, 0, ""
			}
		}
		rendered = c.completed + wrapText(text[c.completedBytes:], width)
	} else {
		rendered = wrapText(text, width)
	}

	c.source = strings.Clone(text)
	c.width, c.valid, c.plain = width, true, plain
	tail := boundedTextTail(rendered, liveTextRenderBytes, maxViewportLines)
	if len(tail) != len(rendered) {
		tail = strings.Clone(tail)
	}
	c.rendered = tail
	return c.rendered
}

// reset releases live-only data when an item settles into Markdown rendering.
func (c *liveTextCache) reset() {
	*c = liveTextCache{}
}

func liveTextPlain(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		b := s[i]
		if (b < ' ' && b != '\n' && b != '\t' && b != '\r') || b == '\x7f' {
			return false
		}
	}
	return true
}
