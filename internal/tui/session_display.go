package tui

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"

	"github.com/spawn08/chronos/engine/model"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/orchestrator"
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

func (m *appModel) restoreSessionHistory() {
	messages, err := m.orch.SessionHistory(m.ctx, m.orch.CurrentSessionID())
	m.resetDisplayedSession()
	m.announcedSession = ""
	m.followOutput = true
	if err != nil {
		m.appendSystem("session history unavailable: " + err.Error())
		m.announceSession()
		m.refreshViewport()
		return
	}
	for _, message := range messages {
		if message.Role == model.RoleUser {
			// Batch rendering avoids rebuilding the full transcript per message.
			m.appendBlock(RenderTurnHeader("❯", "you", styleUserPrefix, m.viewport.Width()) + "\n" +
				wrapText(boundedTextTail(message.Content, maxItemRenderBytes, maxViewportLines), m.viewport.Width()))
			m.setBlockSource(&transcriptSource{user: message.Content, bytes: len(message.Content), width: m.viewport.Width()})
			continue
		}
		items := []turnItem{{kind: turnItemText, content: message.Content}}
		name := m.displayAgentName()
		m.appendBlock(m.buildAssistantBlockNamed(items, false, nil, name))
		m.setBlockSource(&transcriptSource{items: items, name: name, bytes: len(message.Content), width: m.viewport.Width()})
		m.lastAssistantText = message.Content
	}
	m.announceSession()
	m.refreshViewport()
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

func (m *appModel) resetDisplayedSession() {
	m.blocks = nil
	m.blockSources = nil
	m.rawBlockBytes = 0
	m.blockBytes = 0
	m.trimmedBlocks = 0
	m.invalidateRenderCache()
	m.lastKnownUsage = model.Usage{}
	m.lastContextReport = nil
	m.lastMemoryIntent = nil
	m.lastTurnCost = budget.SessionCost{}
	m.lastModelCalls = 0
	m.lastSubagents = 0
	m.lastAssistantText = ""
	m.lastTurnItems = nil
	m.lastTurnErr = nil
	m.lastTurnInterrupted = false
	m.lastExecution = orchestrator.ExecutionSnapshot{}
	m.hasLastTurn = false
	m.lastTurnBlockIdx = -1
}
