package memory

import (
	"fmt"
	"strings"
)

// IntentAction identifies an explicit memory command.
type IntentAction string

const (
	IntentRemember   IntentAction = "remember"
	IntentForget     IntentAction = "forget"
	IntentRecallPast IntentAction = "recall_past"
)

// Intent is the parsed, explicit memory operation. Payload is populated for
// remember and recall-past; RecordID is populated for forget.
type Intent struct {
	Action   IntentAction
	Category Category
	Payload  string
	RecordID string
}

// IntentResult is safe execution metadata for callers. It deliberately omits
// remembered content and recall queries so reporting cannot disclose them.
type IntentResult struct {
	Action   IntentAction
	Category Category
	RecordID string
	Applied  bool
	Reason   string
}

// ParseIntent recognizes only complete, single-line memory commands. The
// narrow grammar prevents trigger words embedded in ordinary prose from
// causing persistence.
func ParseIntent(message string) (Intent, bool, error) {
	text := strings.TrimSpace(message)
	lower := strings.ToLower(text)

	rememberPrefixes := []struct {
		prefix   string
		category Category
	}{
		{prefix: "remember:", category: CategoryFeedback},
		{prefix: "remember project:", category: CategoryProject},
		{prefix: "remember user:", category: CategoryUser},
		{prefix: "remember feedback:", category: CategoryFeedback},
	}
	for _, candidate := range rememberPrefixes {
		if strings.HasPrefix(lower, candidate.prefix) {
			payload, err := explicitPayload(text[len(candidate.prefix):], "remember")
			if err != nil {
				return Intent{}, true, err
			}
			return Intent{Action: IntentRemember, Category: candidate.category, Payload: payload}, true, nil
		}
	}
	if lower == "remember" || (strings.HasPrefix(lower, "remember ") && strings.Contains(lower, ":")) {
		return Intent{}, true, fmt.Errorf("remember syntax is 'remember: <text>' or 'remember <project|user|feedback>: <text>'")
	}

	if strings.HasPrefix(lower, "forget:") {
		id, err := explicitPayload(text[len("forget:"):], "forget")
		if err != nil {
			return Intent{}, true, err
		}
		if !validRecordID(id) {
			return Intent{}, true, fmt.Errorf("forget requires one memory ID beginning with mem_")
		}
		return Intent{Action: IntentForget, RecordID: id}, true, nil
	}
	if lower == "forget" {
		return Intent{}, true, fmt.Errorf("forget syntax is 'forget: <mem_ID>'")
	}

	for _, prefix := range []string{"recall-past:", "recall past:"} {
		if strings.HasPrefix(lower, prefix) {
			payload, err := explicitPayload(text[len(prefix):], "recall-past")
			if err != nil {
				return Intent{}, true, err
			}
			return Intent{Action: IntentRecallPast, Payload: payload}, true, nil
		}
	}
	if lower == "recall-past" || lower == "recall past" {
		return Intent{}, true, fmt.Errorf("recall-past syntax is 'recall-past: <query>'")
	}

	return Intent{}, false, nil
}

func explicitPayload(value, action string) (string, error) {
	payload := strings.TrimSpace(value)
	if payload == "" {
		return "", fmt.Errorf("%s requires a non-empty payload", action)
	}
	if strings.ContainsAny(payload, "\r\n") {
		return "", fmt.Errorf("%s accepts exactly one command line", action)
	}
	if strings.HasSuffix(payload, "?") {
		return "", fmt.Errorf("%s payload must be declarative, not a question", action)
	}
	return payload, nil
}

func validRecordID(id string) bool {
	if !strings.HasPrefix(id, "mem_") || len(id) == len("mem_") {
		return false
	}
	for _, r := range id[len("mem_"):] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return false
		}
	}
	return true
}
