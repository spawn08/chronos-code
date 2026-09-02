//go:build lsp

package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFormatRequest(t *testing.T) {
	data, err := FormatRequest(1, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": "file:///test.go"},
		"position":     map[string]any{"line": 10, "character": 5},
	})
	if err != nil {
		t.Fatalf("FormatRequest error: %v", err)
	}

	s := string(data)
	if !strings.HasPrefix(s, "Content-Length: ") {
		t.Fatalf("missing Content-Length header, got: %q", s[:40])
	}

	parts := strings.SplitN(s, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatalf("expected header + body separated by \\r\\n\\r\\n, got %d parts", len(parts))
	}

	var req jsonrpcRequest
	if err := json.Unmarshal([]byte(parts[1]), &req); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if req.JSONRPC != "2.0" {
		t.Errorf("jsonrpc=%q, want 2.0", req.JSONRPC)
	}
	if req.ID == nil || *req.ID != 1 {
		t.Errorf("id=%v, want 1", req.ID)
	}
	if req.Method != "textDocument/hover" {
		t.Errorf("method=%q, want textDocument/hover", req.Method)
	}
}

func TestFormatRequest_ContentLengthMatchesBody(t *testing.T) {
	data, err := FormatRequest(42, "initialize", map[string]any{"processId": 1234})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	parts := strings.SplitN(s, "\r\n\r\n", 2)
	if len(parts) != 2 {
		t.Fatal("bad framing")
	}
	var claimedLen int
	fmt.Sscanf(parts[0], "Content-Length: %d", &claimedLen)
	if claimedLen != len(parts[1]) {
		t.Errorf("Content-Length=%d but body is %d bytes", claimedLen, len(parts[1]))
	}
}

func TestParseResponse_Success(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"result":{"contents":"func Foo()"}}`
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	r := bufio.NewReader(strings.NewReader(msg))

	resp, err := ParseResponse(r)
	if err != nil {
		t.Fatalf("ParseResponse error: %v", err)
	}
	if id, err := responseID(resp.ID); err != nil || id != 1 {
		t.Errorf("id=%s, want 1", resp.ID)
	}
	if resp.Error != nil {
		t.Errorf("unexpected error: %v", resp.Error)
	}
	if len(resp.Result) == 0 {
		t.Fatal("empty result")
	}
}

func TestParseResponse_Error(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":2,"error":{"code":-32601,"message":"method not found"}}`
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	r := bufio.NewReader(strings.NewReader(msg))

	resp, err := ParseResponse(r)
	if err != nil {
		t.Fatalf("ParseResponse error: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response")
	}
	if resp.Error.Code != -32601 {
		t.Errorf("error code=%d, want -32601", resp.Error.Code)
	}
	if resp.Error.Message != "method not found" {
		t.Errorf("error message=%q", resp.Error.Message)
	}
}

func TestParseResponse_Notification(t *testing.T) {
	body := `{"jsonrpc":"2.0","method":"textDocument/publishDiagnostics","params":{"uri":"file:///test.go","diagnostics":[]}}`
	msg := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	r := bufio.NewReader(strings.NewReader(msg))

	resp, err := ParseResponse(r)
	if err != nil {
		t.Fatalf("ParseResponse error: %v", err)
	}
	if resp.Method != "textDocument/publishDiagnostics" {
		t.Errorf("method=%q, want textDocument/publishDiagnostics", resp.Method)
	}
	if len(resp.ID) != 0 {
		t.Errorf("notification should not have id, got %v", resp.ID)
	}
}

func TestParseResponse_MultipleMessages(t *testing.T) {
	body1 := `{"jsonrpc":"2.0","method":"window/logMessage","params":{"type":3,"message":"hello"}}`
	body2 := `{"jsonrpc":"2.0","id":1,"result":null}`
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Content-Length: %d\r\n\r\n%s", len(body1), body1)
	fmt.Fprintf(&buf, "Content-Length: %d\r\n\r\n%s", len(body2), body2)

	r := bufio.NewReader(&buf)

	msg1, err := ParseResponse(r)
	if err != nil {
		t.Fatalf("first message: %v", err)
	}
	if msg1.Method != "window/logMessage" {
		t.Errorf("first method=%q", msg1.Method)
	}

	msg2, err := ParseResponse(r)
	if err != nil {
		t.Fatalf("second message: %v", err)
	}
	if id, err := responseID(msg2.ID); err != nil || id != 1 {
		t.Errorf("second id=%v, want 1", msg2.ID)
	}
}

func TestParseResponse_MissingContentLength(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\r\n"))
	_, err := ParseResponse(r)
	if err == nil {
		t.Fatal("expected error for missing Content-Length")
	}
}

func TestSeverityString(t *testing.T) {
	tests := []struct {
		sev  int
		want string
	}{
		{1, "error"},
		{2, "warning"},
		{3, "info"},
		{4, "hint"},
		{99, "unknown"},
	}
	for _, tt := range tests {
		if got := SeverityString(tt.sev); got != tt.want {
			t.Errorf("SeverityString(%d)=%q, want %q", tt.sev, got, tt.want)
		}
	}
}

func TestFileURI(t *testing.T) {
	path := "/tmp/space and 世界/test.go"
	uri := FileURI(path)
	parsed, err := url.Parse(uri)
	if err != nil {
		t.Fatalf("parse FileURI: %v", err)
	}
	if parsed.Scheme != "file" || parsed.Host != "" || parsed.Path != path {
		t.Errorf("FileURI(%q)=%q, parsed as %#v", path, uri, parsed)
	}
	if strings.Contains(uri, " ") || !strings.Contains(uri, "%20") {
		t.Errorf("FileURI should encode spaces, got %q", uri)
	}
}

func TestDiagnosticTimestampRoundTrip(t *testing.T) {
	want := time.Date(2026, 9, 2, 12, 34, 56, 789, time.UTC)
	data, err := json.Marshal(Diagnostic{Message: "broken", ReceivedAt: want})
	if err != nil {
		t.Fatal(err)
	}
	var got Diagnostic
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !got.ReceivedAt.Equal(want) {
		t.Fatalf("ReceivedAt=%s, want %s", got.ReceivedAt, want)
	}
}

func TestClientConcurrentOutOfOrderResponses(t *testing.T) {
	c := newFakeClient(t, "out-of-order")

	results := make(map[string]string)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, method := range []string{"slow", "fast"} {
		method := method
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := c.request(context.Background(), method, nil)
			if err != nil {
				t.Errorf("request %s: %v", method, err)
				return
			}
			mu.Lock()
			results[method], _ = result.(string)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if results["slow"] != "slow-result" || results["fast"] != "fast-result" {
		t.Fatalf("responses were miscorrelated: %#v", results)
	}
}

func TestClientRecordsUnsolicitedDiagnostics(t *testing.T) {
	c := newFakeClient(t, "diagnostics")
	uri := "file:///tmp/diagnostic.go"
	deadline := time.Now().Add(2 * time.Second)
	for {
		diagnostics, err := c.Diagnostics(context.Background(), uri)
		if err != nil {
			t.Fatal(err)
		}
		if len(diagnostics) == 1 {
			if diagnostics[0].Message != "unsolicited" {
				t.Fatalf("message=%q", diagnostics[0].Message)
			}
			if diagnostics[0].ReceivedAt.IsZero() {
				t.Fatal("diagnostic receipt timestamp is zero")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("unsolicited diagnostics were not recorded")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestClientCancellationDoesNotPoisonLaterRequest(t *testing.T) {
	c := newFakeClient(t, "cancel")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := c.request(ctx, "cancel-me", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled request error=%v", err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("canceled request did not return promptly")
	}

	result, err := c.request(context.Background(), "after-cancel", nil)
	if err != nil {
		t.Fatalf("request after cancellation: %v", err)
	}
	if result != "cancel-observed" {
		t.Fatalf("result=%v, want cancel-observed", result)
	}
}

func TestClientRepliesToServerRequests(t *testing.T) {
	c := newFakeClient(t, "server-request")
	result, err := c.request(context.Background(), "verify-server-reply", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != "reply-valid" {
		t.Fatalf("result=%v, want reply-valid", result)
	}
}

func TestClientCloseGraceful(t *testing.T) {
	c := newFakeClient(t, "graceful")
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestClientCloseKillsUnresponsiveServer(t *testing.T) {
	c := newFakeClient(t, "stubborn")
	started := time.Now()
	err := c.Close()
	if err == nil {
		t.Fatal("Close should report forced process termination")
	}
	if time.Since(started) > 2*shutdownTimeout+killTimeout {
		t.Fatalf("Close exceeded its shutdown bound: %v", time.Since(started))
	}
}

func newFakeClient(t *testing.T, mode string) *Client {
	t.Helper()
	c, err := NewClient(os.Args[0], "-test.run=^TestLSPHelperProcess$", "--", mode)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Close()
	})
	return c
}

func TestLSPHelperProcess(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	mode := os.Args[len(os.Args)-1]
	r := bufio.NewReader(os.Stdin)
	switch mode {
	case "out-of-order":
		first := fakeRead(t, r)
		second := fakeRead(t, r)
		fakeRespond(t, second.ID, second.Method+"-result")
		fakeRespond(t, first.ID, first.Method+"-result")
	case "diagnostics":
		fakeWrite(t, map[string]any{
			"jsonrpc": "2.0",
			"method":  "textDocument/publishDiagnostics",
			"params": map[string]any{
				"uri": "file:///tmp/diagnostic.go",
				"diagnostics": []map[string]any{{
					"range": map[string]any{
						"start": map[string]int{"line": 1, "character": 2},
						"end":   map[string]int{"line": 1, "character": 3},
					},
					"severity": 1, "message": "unsolicited", "source": "fake",
				}},
			},
		})
		fakeServeShutdown(t, r)
	case "cancel":
		_ = fakeRead(t, r)
		var canceled bool
		var after *jsonrpcResponse
		for !canceled || after == nil {
			msg := fakeRead(t, r)
			if msg.Method == "$/cancelRequest" {
				canceled = true
			} else if msg.Method == "after-cancel" {
				after = msg
			} else {
				t.Fatalf("unexpected method %q", msg.Method)
			}
		}
		fakeRespond(t, after.ID, "cancel-observed")
	case "server-request":
		fakeWrite(t, map[string]any{
			"jsonrpc": "2.0",
			"id":      "server-1",
			"method":  "workspace/configuration",
			"params":  map[string]any{"items": []map[string]any{{"section": "fake"}}},
		})
		first := fakeRead(t, r)
		second := fakeRead(t, r)
		reply, request := first, second
		if first.Method != "" {
			reply, request = second, first
		}
		if string(reply.ID) != `"server-1"` || string(reply.Result) != `[null]` {
			t.Fatalf("invalid server request reply: id=%s result=%s error=%v", reply.ID, reply.Result, reply.Error)
		}
		fakeRespond(t, request.ID, "reply-valid")
	case "graceful":
		fakeServeShutdown(t, r)
	case "stubborn":
		_ = fakeRead(t, r)
		time.Sleep(time.Hour)
	default:
		t.Fatalf("unknown fake LSP mode %q", mode)
	}
}

func fakeServeShutdown(t *testing.T, r *bufio.Reader) {
	for {
		msg := fakeRead(t, r)
		switch msg.Method {
		case "shutdown":
			fakeRespond(t, msg.ID, nil)
		case "exit":
			return
		}
	}
}

func fakeRead(t *testing.T, r *bufio.Reader) *jsonrpcResponse {
	t.Helper()
	msg, err := ParseResponse(r)
	if err != nil {
		t.Fatalf("read JSON-RPC message: %v", err)
	}
	return msg
}

func fakeRespond(t *testing.T, id json.RawMessage, result any) {
	t.Helper()
	fakeWrite(t, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func fakeWrite(t *testing.T, msg any) {
	t.Helper()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(data), data); err != nil {
		t.Fatalf("write JSON-RPC message: %v", err)
	}
}
