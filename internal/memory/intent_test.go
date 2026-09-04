package memory

import "testing"

func TestParseIntentExplicitCommands(t *testing.T) {
	tests := []struct {
		message  string
		action   IntentAction
		category Category
		payload  string
		recordID string
	}{
		{message: "remember: use tabs", action: IntentRemember, category: CategoryFeedback, payload: "use tabs"},
		{message: " REMEMBER project: run make test ", action: IntentRemember, category: CategoryProject, payload: "run make test"},
		{message: "remember user: prefers concise answers", action: IntentRemember, category: CategoryUser, payload: "prefers concise answers"},
		{message: "remember feedback: do not amend commits", action: IntentRemember, category: CategoryFeedback, payload: "do not amend commits"},
		{message: "forget: mem_0123abcd", action: IntentForget, recordID: "mem_0123abcd"},
		{message: "recall-past: parser decisions", action: IntentRecallPast, payload: "parser decisions"},
		{message: "recall past: parser decisions", action: IntentRecallPast, payload: "parser decisions"},
	}
	for _, tt := range tests {
		t.Run(tt.message, func(t *testing.T) {
			got, ok, err := ParseIntent(tt.message)
			if err != nil || !ok {
				t.Fatalf("ParseIntent() = (%+v, %t, %v), want explicit intent", got, ok, err)
			}
			if got.Action != tt.action || got.Category != tt.category || got.Payload != tt.payload || got.RecordID != tt.recordID {
				t.Fatalf("ParseIntent() = %+v", got)
			}
		})
	}
}

func TestParseIntentRejectsMalformedExplicitCommands(t *testing.T) {
	for _, message := range []string{
		"remember",
		"remember:",
		"remember team: use tabs",
		"remember: use tabs?",
		"remember: first\nforget: mem_second",
		"forget",
		"forget:",
		"forget: use tabs",
		"forget: mem_one mem_two",
		"recall-past:",
		"recall-past: should I use tabs?",
	} {
		t.Run(message, func(t *testing.T) {
			if _, recognized, err := ParseIntent(message); !recognized || err == nil {
				t.Fatalf("ParseIntent(%q) = recognized %t, error %v", message, recognized, err)
			}
		})
	}
}

func TestParseIntentIgnoresIncidentalTriggerWords(t *testing.T) {
	for _, message := range []string{
		"Can you remember what the parser returned?",
		"What do you always run before a release?",
		"Explain why we never mutate that map",
		"I don't understand this function",
		"Please note the error in your response",
		"The method name is forget: but this is not a command",
		"Can you recall-past: parser decisions?",
		"",
	} {
		t.Run(message, func(t *testing.T) {
			if got, recognized, err := ParseIntent(message); err != nil || recognized || got != (Intent{}) {
				t.Fatalf("ParseIntent(%q) = (%+v, %t, %v), want no intent", message, got, recognized, err)
			}
		})
	}
}
