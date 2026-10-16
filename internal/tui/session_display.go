package tui

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
)

func sessionResumeHint(id string) string {
	if id == "" {
		return ""
	}
	if strings.IndexFunc(id, unicode.IsControl) >= 0 {
		return "Session: " + strconv.Quote(id) + "\nResume this session with /session list."
	}
	arg := id
	if strings.IndexFunc(id, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.')
	}) >= 0 {
		arg = "'" + strings.ReplaceAll(id, "'", `'"'"'`) + "'"
	}
	return "Session: " + id + "\nResume: chronos-code --resume " + arg
}

func (m *appModel) announceSession() bool {
	if m.orch == nil || !m.ready {
		return false
	}
	id := m.orch.CurrentSessionID()
	if id == "" || id == m.announcedSession {
		return false
	}
	m.announcedSession = id
	m.appendSystem(sessionResumeHint(id))
	return true
}

func writeSessionResumeHint(w io.Writer, id string) error {
	hint := sessionResumeHint(id)
	if hint == "" {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\n"+hint); err != nil {
		return fmt.Errorf("write session resume hint: %w", err)
	}
	return nil
}
