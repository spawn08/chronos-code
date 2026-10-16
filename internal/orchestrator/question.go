package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

const AskUserQuestionToolName = "ask_user_question"

type UserQuestion struct {
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"`
	Recommended string   `json:"recommended,omitempty"`
	MultiSelect bool     `json:"multi_select,omitempty"`
}

type UserAnswer struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices,omitempty"`
	Text     string   `json:"text,omitempty"`
}

type UserQuestionResult struct {
	Status  string       `json:"status"`
	Answers []UserAnswer `json:"answers,omitempty"`
	Message string       `json:"message,omitempty"`
}

type UserQuestionFunc func(context.Context, []UserQuestion) (UserQuestionResult, error)

// SetUserQuestionHandler replaces the interactive handler. Nil requires input
// without guessing an answer. The handler must honor context cancellation.
func (o *Orchestrator) SetUserQuestionHandler(handler UserQuestionFunc) {
	if o == nil {
		return
	}
	o.userQuestionMu.Lock()
	o.userQuestion = handler
	o.userQuestionMu.Unlock()
}

func (o *Orchestrator) userQuestionHandler() UserQuestionFunc {
	if o == nil {
		return nil
	}
	o.userQuestionMu.RLock()
	defer o.userQuestionMu.RUnlock()
	return o.userQuestion
}

func installUserQuestion(agents map[string]*agent.Agent, o *Orchestrator) {
	for _, a := range agents {
		if a != nil && a.Tools != nil {
			a.Tools.Register(newUserQuestionTool(o))
		}
	}
}

func newUserQuestionTool(o *Orchestrator) *tool.Definition {
	return &tool.Definition{
		Name:        AskUserQuestionToolName,
		Description: "Ask the user 1–4 questions and wait for their answers. Provide up to 8 options per question, or omit options for an open question. Custom text answers are always allowed. Recommended must match an option. Its own interactive handler collects answers; the host automatically handles any normalized require_approval label. Repository Claude settings do not control this tool. Diagnose actual errors before claiming it is blocked. If input is required or cancelled, stop and wait for user feedback; do not guess or repeat the call this turn.",
		Permission:  tool.PermAllow,
		Effects:     []tool.Effect{tool.EffectScratchWrite},
		Parameters: map[string]any{
			"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"questions": map[string]any{
					"type": "array", "minItems": 1, "maxItems": 4,
					"items": map[string]any{
						"type": "object", "additionalProperties": false,
						"properties": map[string]any{
							"question":     map[string]any{"type": "string", "minLength": 1},
							"options":      map[string]any{"type": "array", "maxItems": 8, "uniqueItems": true, "items": map[string]any{"type": "string", "minLength": 1}},
							"recommended":  map[string]any{"type": "string"},
							"multi_select": map[string]any{"type": "boolean"},
						},
						"required": []string{"question"},
					},
				},
			},
			"required": []string{"questions"},
		},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			questions, err := parseUserQuestions(args)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", AskUserQuestionToolName, err)
			}
			if ctx.Err() != nil {
				return userQuestionStopped("cancelled"), nil
			}
			handler := o.userQuestionHandler()
			if handler == nil {
				return userQuestionStopped("input_required"), nil
			}
			// Keep validation independent of changes made by the handler.
			prompt := append([]UserQuestion(nil), questions...)
			for i := range prompt {
				prompt[i].Options = append([]string(nil), questions[i].Options...)
			}
			result, err := handler(ctx, prompt)
			if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return userQuestionStopped("cancelled"), nil
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", AskUserQuestionToolName, err)
			}
			switch result.Status {
			case "cancelled", "input_required":
				return userQuestionStopped(result.Status), nil
			case "answered":
				if err := validateUserAnswers(questions, result.Answers); err != nil {
					return nil, fmt.Errorf("%s: invalid handler result: %w", AskUserQuestionToolName, err)
				}
				if result.Message != "" {
					result.Message += "\n"
				}
				result.Message += "Answers received through the interactive handler. The host auto-allows this built-in tool; any require_approval metadata is internal routing, with no separate permission confirmation."
				return result, nil
			default:
				return nil, fmt.Errorf("%s: invalid handler status %q", AskUserQuestionToolName, result.Status)
			}
		},
	}
}

func userQuestionStopped(status string) UserQuestionResult {
	message := "User input is required. Stop this turn and request input from the user; do not guess answers or continue until they respond."
	if status == "cancelled" {
		message = "The question was cancelled. Stop this turn and wait for user feedback; do not continue or ask again until the user responds."
	}
	return UserQuestionResult{Status: status, Message: message}
}

func parseUserQuestions(args map[string]any) ([]UserQuestion, error) {
	data, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("encode arguments: %w", err)
	}
	var input struct {
		Questions []UserQuestion `json:"questions"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("decode arguments: %w", err)
	}
	if len(input.Questions) < 1 || len(input.Questions) > 4 {
		return nil, fmt.Errorf("questions must contain 1–4 questions")
	}
	// JSON null otherwise silently becomes a string or boolean zero value.
	var raw struct {
		Questions []map[string]json.RawMessage `json:"questions"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("decode question fields: %w", err)
	}
	seen := make(map[string]bool)
	for i := range input.Questions {
		q := &input.Questions[i]
		for field, value := range raw.Questions[i] {
			if bytes.Equal(value, []byte("null")) {
				return nil, fmt.Errorf("questions[%d].%s must not be null", i, field)
			}
		}
		q.Question = strings.TrimSpace(q.Question)
		if q.Question == "" || seen[q.Question] {
			return nil, fmt.Errorf("questions[%d].question must be nonempty and unique", i)
		}
		seen[q.Question] = true
		if len(q.Options) > 8 {
			return nil, fmt.Errorf("questions[%d].options must contain at most 8 choices", i)
		}
		options := make(map[string]bool)
		for j := range q.Options {
			q.Options[j] = strings.TrimSpace(q.Options[j])
			option := q.Options[j]
			if option == "" || options[option] {
				return nil, fmt.Errorf("questions[%d].options must be nonempty and unique", i)
			}
			options[option] = true
		}
		q.Recommended = strings.TrimSpace(q.Recommended)
		if _, supplied := raw.Questions[i]["recommended"]; supplied && !options[q.Recommended] {
			return nil, fmt.Errorf("questions[%d].recommended must match an option", i)
		}
	}
	return input.Questions, nil
}

func validateUserAnswers(questions []UserQuestion, answers []UserAnswer) error {
	if len(answers) != len(questions) {
		return fmt.Errorf("expected an answer for each question")
	}
	pending := make(map[string]UserQuestion, len(questions))
	for _, q := range questions {
		pending[q.Question] = q
	}
	for _, answer := range answers {
		q, ok := pending[answer.Question]
		if !ok {
			return fmt.Errorf("unknown or repeated question %q", answer.Question)
		}
		delete(pending, answer.Question)
		if len(answer.Choices) == 0 && strings.TrimSpace(answer.Text) == "" {
			return fmt.Errorf("question %q has no answer", q.Question)
		}
		if !q.MultiSelect && len(answer.Choices) > 1 {
			return fmt.Errorf("question %q allows only one choice", q.Question)
		}
		options := make(map[string]bool, len(q.Options))
		for _, option := range q.Options {
			options[option] = true
		}
		for _, choice := range answer.Choices {
			if !options[choice] {
				return fmt.Errorf("question %q has an invalid or repeated choice %q; custom answers belong in text", q.Question, choice)
			}
			delete(options, choice)
		}
	}
	return nil
}
