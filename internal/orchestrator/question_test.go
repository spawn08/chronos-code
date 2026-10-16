package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
)

func userQuestionRegistry(o *Orchestrator) *tool.Registry {
	a := &agent.Agent{ID: "reader", Tools: tool.NewRegistry()}
	installUserQuestion(map[string]*agent.Agent{a.ID: a}, o)
	return a.Tools
}

func userQuestionArgs(t *testing.T, data string) map[string]any {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(data), &args); err != nil {
		t.Fatal(err)
	}
	return args
}

func TestAskUserQuestionRegistryRoundtrip(t *testing.T) {
	o := &Orchestrator{}
	o.SetPlanMode(true)
	want := []UserQuestion{
		{Question: "Backend?", Options: []string{"Go", "Rust"}, Recommended: "Go"},
		{Question: "Features?", Options: []string{"API", "CLI"}, MultiSelect: true},
		{Question: "Anything else?"},
		{Question: "Deployment?", Options: []string{"Cloud", "Local"}},
	}
	answers := []UserAnswer{
		{Question: "Backend?", Choices: []string{"Rust"}},
		{Question: "Features?", Choices: []string{"API", "CLI"}},
		{Question: "Anything else?", Text: "Keep it simple"},
		{Question: "Deployment?", Text: "Something custom"},
	}
	type contextKey struct{}
	ctx := context.WithValue(executionEffectContext(context.Background(), true), contextKey{}, "present")
	o.SetUserQuestionHandler(func(gotCtx context.Context, questions []UserQuestion) (UserQuestionResult, error) {
		if gotCtx.Value(contextKey{}) != "present" || !reflect.DeepEqual(questions, want) {
			t.Fatalf("handler input = %+v, context = %v", questions, gotCtx)
		}
		return UserQuestionResult{Status: "answered", Answers: answers}, nil
	})
	registry := userQuestionRegistry(o)
	out, err := registry.Execute(ctx, AskUserQuestionToolName, userQuestionArgs(t, `{"questions":[{"question":" Backend? ","options":["Go","Rust"],"recommended":"Go"},{"question":"Features?","options":["API","CLI"],"multi_select":true},{"question":"Anything else?","options":[]},{"question":"Deployment?","options":["Cloud","Local"]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	var decoded UserQuestionResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Status != "answered" || !reflect.DeepEqual(decoded.Answers, answers) {
		t.Fatalf("result = %s", data)
	}
	if !o.PlanMode() {
		t.Fatal("asking questions changed plan mode")
	}
	def, _ := registry.Get(AskUserQuestionToolName)
	if def.ParallelSafe || !reflect.DeepEqual(def.Effects, []tool.Effect{tool.EffectScratchWrite}) {
		t.Fatalf("unexpected execution flags: %+v", def)
	}
	if _, err := registry.Execute(tool.WithEffectGrant(context.Background(), tool.EffectRead), AskUserQuestionToolName, map[string]any{"questions": want}); err == nil {
		t.Fatal("read-only grant allowed scratch write")
	}
}

func TestAskUserQuestionSchemaAndOptionLimit(t *testing.T) {
	def := newUserQuestionTool(&Orchestrator{})
	properties := def.Parameters["properties"].(map[string]any)
	questions := properties["questions"].(map[string]any)
	item := questions["items"].(map[string]any)
	options := item["properties"].(map[string]any)["options"].(map[string]any)
	if questions["minItems"] != 1 || questions["maxItems"] != 4 || options["maxItems"] != 8 || options["items"].(map[string]any)["type"] != "string" {
		t.Fatalf("unexpected schema: %+v", def.Parameters)
	}
	out, err := userQuestionRegistry(&Orchestrator{}).Execute(context.Background(), AskUserQuestionToolName, map[string]any{
		"questions": []UserQuestion{{Question: "Q", Options: []string{"1", "2", "3", "4", "5", "6", "7", "8"}, Recommended: "8"}},
	})
	if err != nil || out.(UserQuestionResult).Status != "input_required" {
		t.Fatalf("eight options rejected: out=%+v, err=%v", out, err)
	}
}

func TestAskUserQuestionInvalidArguments(t *testing.T) {
	cases := []string{
		`{}`, `{"questions":null}`, `{"questions":[]}`, `{"questions":"wrong"}`,
		`{"questions":[null]}`, `{"questions":[{}]}`, `{"questions":[{"question":"  "}]}`,
		`{"questions":[{"question":1}]}`, `{"questions":[{"question":"Q","unknown":true}]}`,
		`{"questions":[{"question":"Q"}],"unknown":true}`,
		`{"questions":[{"question":"Q","options":"a"}]}`,
		`{"questions":[{"question":"Q","options":[1]}]}`,
		`{"questions":[{"question":"Q","options":[null]}]}`,
		`{"questions":[{"question":"Q","options":[" "]}]}`,
		`{"questions":[{"question":"Q","options":["a"," a "]}]}`,
		`{"questions":[{"question":"Q","options":["1","2","3","4","5","6","7","8","9"]}]}`,
		`{"questions":[{"question":"Q","recommended":"a"}]}`,
		`{"questions":[{"question":"Q","options":["a"],"recommended":"b"}]}`,
		`{"questions":[{"question":"Q","options":["a"],"recommended":""}]}`,
		`{"questions":[{"question":"Q","multi_select":"true"}]}`,
		`{"questions":[{"question":"Q","multi_select":null}]}`,
		`{"questions":[{"question":"Q","options":null}]}`,
		`{"questions":[{"question":"Q","recommended":null}]}`,
		`{"questions":[{"question":"Q"},{"question":" Q "}]}`,
		`{"questions":[{"question":"1"},{"question":"2"},{"question":"3"},{"question":"4"},{"question":"5"}]}`,
	}
	o := &Orchestrator{}
	o.SetUserQuestionHandler(func(context.Context, []UserQuestion) (UserQuestionResult, error) {
		t.Error("invalid arguments reached handler")
		return UserQuestionResult{}, nil
	})
	registry := userQuestionRegistry(o)
	for _, data := range cases {
		t.Run(data, func(t *testing.T) {
			if _, err := registry.Execute(context.Background(), AskUserQuestionToolName, userQuestionArgs(t, data)); err == nil {
				t.Fatal("accepted invalid arguments")
			}
		})
	}
	if _, err := registry.Execute(context.Background(), AskUserQuestionToolName, map[string]any{"questions": make(chan int)}); err == nil {
		t.Fatal("accepted non-JSON arguments")
	}
}

func TestAskUserQuestionHeadlessAndNormalizedPermissions(t *testing.T) {
	o := &Orchestrator{}
	a := &agent.Agent{ID: "reader", Tools: tool.NewRegistry()}
	o.agents = map[string]*agent.Agent{a.ID: a}
	installUserQuestion(o.agents, o)
	normalizeToolPermissions(o.agents)
	o.SetApprovalHandler(nil)
	out, err := a.Tools.Execute(executionEffectContext(context.Background(), true), AskUserQuestionToolName, map[string]any{"questions": []UserQuestion{{Question: "Q"}}})
	if err != nil {
		t.Fatal(err)
	}
	result := out.(UserQuestionResult)
	if result.Status != "input_required" || len(result.Answers) != 0 || !strings.Contains(result.Message, "Stop") || !strings.Contains(result.Message, "request input") {
		t.Fatalf("headless result = %+v", result)
	}
}

func TestAskUserQuestionCancellation(t *testing.T) {
	for _, name := range []string{"handler_cancelled", "cancel_error", "deadline_error", "pre_cancelled", "during_handler", "expired_deadline"} {
		t.Run(name, func(t *testing.T) {
			o := &Orchestrator{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if name == "pre_cancelled" {
				cancel()
			}
			if name == "expired_deadline" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer deadlineCancel()
			}
			o.SetUserQuestionHandler(func(ctx context.Context, questions []UserQuestion) (UserQuestionResult, error) {
				switch name {
				case "handler_cancelled":
					return UserQuestionResult{Status: "cancelled", Message: "continue", Answers: []UserAnswer{{Question: "Q", Text: "ignored"}}}, nil
				case "cancel_error":
					return UserQuestionResult{}, fmtQuestionCancellation(context.Canceled)
				case "deadline_error":
					return UserQuestionResult{}, fmtQuestionCancellation(context.DeadlineExceeded)
				case "during_handler":
					cancel()
					<-ctx.Done()
					return UserQuestionResult{Status: "answered", Answers: []UserAnswer{{Question: "Q", Text: "stale"}}}, nil
				default:
					t.Error("already cancelled request reached handler")
					return UserQuestionResult{}, nil
				}
			})
			execute := newUserQuestionTool(o).Handler
			if name == "handler_cancelled" || name == "cancel_error" || name == "deadline_error" {
				registry := userQuestionRegistry(o)
				execute = func(ctx context.Context, args map[string]any) (any, error) {
					return registry.Execute(ctx, AskUserQuestionToolName, args)
				}
			}
			out, err := execute(ctx, map[string]any{"questions": []UserQuestion{{Question: "Q"}}})
			if err != nil {
				t.Fatal(err)
			}
			result := out.(UserQuestionResult)
			if result.Status != "cancelled" || len(result.Answers) != 0 || !strings.Contains(result.Message, "Stop") || !strings.Contains(result.Message, "feedback") {
				t.Fatalf("cancelled result = %+v", result)
			}
		})
	}
}

func fmtQuestionCancellation(err error) error {
	return errors.Join(errors.New("prompt interrupted"), err)
}

func TestAskUserQuestionInvalidHandlerResults(t *testing.T) {
	questions := []UserQuestion{{Question: "Q", Options: []string{"a", "b"}}}
	cases := []UserQuestionResult{
		{}, {Status: "unknown"}, {Status: "answered"},
		{Status: "answered", Answers: []UserAnswer{{Question: "wrong", Text: "text"}}},
		{Status: "answered", Answers: []UserAnswer{{Question: "Q"}}},
		{Status: "answered", Answers: []UserAnswer{{Question: "Q", Text: " "}}},
		{Status: "answered", Answers: []UserAnswer{{Question: "Q", Choices: []string{"custom"}}}},
		{Status: "answered", Answers: []UserAnswer{{Question: "Q", Choices: []string{"a", "b"}}}},
	}
	for _, result := range cases {
		o := &Orchestrator{}
		o.SetUserQuestionHandler(func(context.Context, []UserQuestion) (UserQuestionResult, error) { return result, nil })
		if _, err := userQuestionRegistry(o).Execute(context.Background(), AskUserQuestionToolName, map[string]any{"questions": questions}); err == nil {
			t.Fatalf("accepted invalid result %+v", result)
		}
	}
	if err := validateUserAnswers([]UserQuestion{{Question: "Q", Options: []string{"a", "b"}, MultiSelect: true}}, []UserAnswer{{Question: "Q", Choices: []string{"a", "a"}}}); err == nil {
		t.Fatal("accepted duplicate multi-select choice")
	}
	if err := validateUserAnswers([]UserQuestion{{Question: "Q"}, {Question: "R"}}, []UserAnswer{{Question: "Q", Text: "a"}, {Question: "Q", Text: "b"}}); err == nil {
		t.Fatal("accepted repeated answer")
	}
	if err := validateUserAnswers([]UserQuestion{{Question: "Q"}}, []UserAnswer{{Question: "Q", Choices: []string{"custom"}}}); err == nil {
		t.Fatal("open question accepted a choice instead of text")
	}
	o := &Orchestrator{}
	sentinel := errors.New("prompt failed")
	o.SetUserQuestionHandler(func(context.Context, []UserQuestion) (UserQuestionResult, error) {
		return UserQuestionResult{}, sentinel
	})
	if _, err := userQuestionRegistry(o).Execute(context.Background(), AskUserQuestionToolName, map[string]any{"questions": questions}); !errors.Is(err, sentinel) {
		t.Fatalf("handler error not wrapped: %v", err)
	}
	o.SetUserQuestionHandler(func(_ context.Context, qs []UserQuestion) (UserQuestionResult, error) {
		qs[0].Options[0] = "tampered"
		return UserQuestionResult{Status: "answered", Answers: []UserAnswer{{Question: "Q", Choices: []string{"tampered"}}}}, nil
	})
	if _, err := userQuestionRegistry(o).Execute(context.Background(), AskUserQuestionToolName, map[string]any{"questions": questions}); err == nil {
		t.Fatal("handler mutation bypassed validation")
	}
}

func TestAskUserQuestionInstallationAndSupervision(t *testing.T) {
	o := &Orchestrator{toolSupervisor: &toolSupervisor{}}
	agents := map[string]*agent.Agent{
		"reader":   {ID: "reader", Tools: tool.NewRegistry()},
		"writer":   {ID: "writer", Tools: tool.NewRegistry()},
		"no_tools": {ID: "no_tools"}, "nil": nil,
	}
	agents["writer"].Tools.Register(&tool.Definition{Name: "file_write"})
	installUserQuestion(agents, o)
	for _, id := range []string{"reader", "writer"} {
		a := agents[id]
		if _, ok := a.Tools.Get(AskUserQuestionToolName); !ok {
			t.Fatalf("%s lacks question tool", id)
		}
		o.installToolSupervision(a)
		out, err := a.Tools.Execute(context.Background(), AskUserQuestionToolName, map[string]any{"questions": []UserQuestion{{Question: "Q"}}})
		if err != nil || out.(UserQuestionResult).Status != "input_required" {
			t.Fatalf("question was supervised: out=%+v, err=%v", out, err)
		}
	}
	if supervisedName(AskUserQuestionToolName) {
		t.Fatal("question must remain synchronous")
	}
}

func TestAskUserQuestionHandlerReplacementThreadSafe(t *testing.T) {
	o := &Orchestrator{}
	registry := userQuestionRegistry(o)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 50; j++ {
				o.SetUserQuestionHandler(func(context.Context, []UserQuestion) (UserQuestionResult, error) {
					// Replacement during a prompt must not hold the handler lock.
					o.SetUserQuestionHandler(nil)
					return UserQuestionResult{Status: "input_required"}, nil
				})
				if _, err := registry.Execute(context.Background(), AskUserQuestionToolName, map[string]any{"questions": []UserQuestion{{Question: "Q"}}}); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Wait()
	var nilOrchestrator *Orchestrator
	nilOrchestrator.SetUserQuestionHandler(nil)
}
