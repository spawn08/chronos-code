package session

import (
	"context"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spawn08/chronos/storage"
)

const summarySessionPageSize = 100

// Keep enough context for ranking even when the final output budget is tiny.
const summaryCandidateMinBytes = 2000

// Optional adapter capability; storage.Storage remains compatible with older adapters.
type sessionSummaryReader interface {
	ReadSessionSummary(ctx context.Context, sessionID string, maxBytes int) (text, source string, sourceSeq int64, truncated bool, err error)
}

// Summary is bounded context selected from one prior session.
type Summary struct {
	SessionID string
	AgentID   string
	UpdatedAt time.Time
	Source    string
	Text      string
	SourceSeq int64
	Truncated bool
}

// RecallSummaries selects relevant prior-session context for agentID. The
// storage calls retain the tenant carried by ctx. Non-positive limits disable
// recall.
func (m *Manager) RecallSummaries(ctx context.Context, agentID, activeSessionID, query string, maxSessions, maxBytes int) ([]Summary, error) {
	if maxSessions <= 0 || maxBytes <= 0 {
		return nil, nil
	}

	sessions, err := m.store.ListSessions(ctx, agentID, summarySessionPageSize, 0)
	if err != nil {
		return nil, err
	}
	queryTerms := summaryTerms(query)
	type scoredSummary struct {
		Summary
		score int
	}
	var candidates []scoredSummary
	reader, projected := m.store.(sessionSummaryReader)
	candidateBytes := max(maxBytes, summaryCandidateMinBytes)
	for _, stored := range sessions[:min(len(sessions), summarySessionPageSize)] {
		if stored == nil || stored.ID == activeSessionID || stored.AgentID != agentID {
			continue
		}
		var candidate Summary
		if projected {
			candidate = Summary{SessionID: stored.ID, AgentID: stored.AgentID, UpdatedAt: stored.UpdatedAt}
			candidate.Text, candidate.Source, candidate.SourceSeq, candidate.Truncated, err = reader.ReadSessionSummary(ctx, stored.ID, candidateBytes)
		} else {
			// ListEvents has no limit parameter. Only legacy adapters use this path.
			var events []*storage.Event
			events, err = m.store.ListEvents(ctx, stored.ID, 0)
			if err == nil {
				candidate, _ = summaryFromEvents(stored, events)
			}
		}
		if err != nil {
			return nil, err
		}
		candidate.Text, candidate.Truncated = truncateSummary(candidate.Text, candidateBytes, candidate.Truncated)
		if candidate.Text != "" {
			candidates = append(candidates, scoredSummary{candidate, summaryRelevance(candidate.Text, queryTerms)})
		}
	}

	sort.Slice(candidates, func(i, j int) bool {
		iScore := candidates[i].score
		jScore := candidates[j].score
		if iScore != jScore {
			return iScore > jScore
		}
		if !candidates[i].UpdatedAt.Equal(candidates[j].UpdatedAt) {
			return candidates[i].UpdatedAt.After(candidates[j].UpdatedAt)
		}
		return candidates[i].SessionID < candidates[j].SessionID
	})

	selected := make([]Summary, 0, min(maxSessions, len(candidates)))
	remaining := maxBytes
	for _, scored := range candidates {
		candidate := scored.Summary
		if len(selected) == maxSessions || remaining == 0 {
			break
		}
		candidate.Text, candidate.Truncated = truncateSummary(candidate.Text, remaining, candidate.Truncated)
		if candidate.Text == "" {
			break
		}
		remaining -= len(candidate.Text)
		selected = append(selected, candidate)
	}
	return selected, nil
}

func summaryFromEvents(stored *storage.Session, events []*storage.Event) (Summary, bool) {
	events = append([]*storage.Event(nil), events...)
	sort.Slice(events, func(i, j int) bool {
		if events[i] == nil {
			return false
		}
		if events[j] == nil {
			return true
		}
		if events[i].SeqNum != events[j].SeqNum {
			return events[i].SeqNum < events[j].SeqNum
		}
		return events[i].ID < events[j].ID
	})
	var latest Summary
	var fallback []string
	var fallbackSeq int64
	for _, event := range events {
		if event == nil {
			continue
		}
		payload, ok := event.Payload.(map[string]any)
		if !ok {
			continue
		}
		switch event.Type {
		case "chat_summary":
			text, _ := payload["summary"].(string)
			text = strings.TrimSpace(text)
			if text != "" && event.SeqNum >= latest.SourceSeq {
				latest = Summary{Source: "chat_summary", Text: text, SourceSeq: event.SeqNum}
			}
		case "chat_message":
			role, _ := payload["role"].(string)
			text, _ := payload["content"].(string)
			text = strings.TrimSpace(text)
			if (role == "user" || role == "assistant") && text != "" {
				fallback = append(fallback, role+": "+text)
				if event.SeqNum > fallbackSeq {
					fallbackSeq = event.SeqNum
				}
			}
		}
	}
	if latest.Text == "" {
		latest = Summary{Source: "chat_message", Text: strings.Join(fallback, "\n"), SourceSeq: fallbackSeq}
	}
	if latest.Text == "" {
		return Summary{}, false
	}
	latest.SessionID = stored.ID
	latest.AgentID = stored.AgentID
	latest.UpdatedAt = stored.UpdatedAt
	return latest, true
}

func summaryTerms(text string) map[string]struct{} {
	terms := make(map[string]struct{})
	for _, term := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len(term) > 1 {
			terms[term] = struct{}{}
		}
	}
	return terms
}

func summaryRelevance(text string, queryTerms map[string]struct{}) int {
	if len(queryTerms) == 0 {
		return 0
	}
	documentTerms := summaryTerms(text)
	score := 0
	for term := range queryTerms {
		if _, ok := documentTerms[term]; ok {
			score++
		}
	}
	return score
}

func truncateSummary(text string, maxBytes int, alreadyTruncated bool) (string, bool) {
	if len(text) <= maxBytes {
		return text, alreadyTruncated
	}
	text = text[:maxBytes]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text, true
}
