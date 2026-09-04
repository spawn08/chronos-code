package server

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spawn08/chronos/engine/model"
)

func TestChatHandlersDelegateToOrchestratorExecute(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	source, err := os.ReadFile(filepath.Join(filepath.Dir(file), "handlers.go"))
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	parsed, err := parser.ParseFile(token.NewFileSet(), "handlers.go", source, 0)
	if err != nil {
		t.Fatalf("parse handlers.go: %v", err)
	}

	executeCalls := 0
	verificationModes := 0
	ast.Inspect(parsed, func(node ast.Node) bool {
		if field, ok := node.(*ast.KeyValueExpr); ok {
			if key, ok := field.Key.(*ast.Ident); ok && key.Name == "VerificationMode" {
				verificationModes++
			}
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "Chat", "ChatWithSession", "ChatStream", "ChatStreamWithSession":
			t.Errorf("handlers.go directly invokes Agent.%s", selector.Sel.Name)
		case "Execute":
			executeCalls++
		}
		return true
	})
	if executeCalls != 1 {
		t.Fatalf("common adapter Execute calls = %d, want 1", executeCalls)
	}
	if verificationModes != 2 {
		t.Fatalf("ExecutionRequest verification modes = %d, want 2", verificationModes)
	}
}

func TestWriteChatStreamEmitsTerminalErrorWithoutDone(t *testing.T) {
	stream := make(chan *model.ChatResponse, 1)
	stream <- &model.ChatResponse{Err: errors.New("verification failed")}
	close(stream)
	recorder := httptest.NewRecorder()

	WriteEventStream(context.Background(), recorder, recorder, stream, "session-1")

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error\n") || !strings.Contains(body, `"error":"verification failed"`) || !strings.Contains(body, `"session_id":"session-1"`) {
		t.Fatalf("SSE body = %q, want explicit JSON error event", body)
	}
	if strings.Contains(body, "[DONE]") {
		t.Fatalf("SSE body = %q, must not report successful completion after an error", body)
	}
}

func TestChatResponseJSONContract(t *testing.T) {
	encoded, err := json.Marshal(chatResponse{
		Content: "answer",
		Usage: usagePayload{
			PromptTokens:     3,
			CompletionTokens: 5,
		},
		SessionID: "session-1",
	})
	if err != nil {
		t.Fatalf("marshal chat response: %v", err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("unmarshal chat response: %v", err)
	}
	if len(payload) != 3 || payload["content"] == nil || payload["usage"] == nil || payload["session_id"] == nil {
		t.Fatalf("chat response fields = %s, want content, usage, and session_id", encoded)
	}
}
