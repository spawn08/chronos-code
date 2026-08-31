//go:build lsp

package lsp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
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
	if resp.ID == nil || *resp.ID != 1 {
		t.Errorf("id=%v, want 1", resp.ID)
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
	if resp.ID != nil {
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
	if msg2.ID == nil || *msg2.ID != 1 {
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
	uri := FileURI("/tmp/test.go")
	if !strings.HasPrefix(uri, "file://") {
		t.Errorf("FileURI should start with file://, got %q", uri)
	}
	if !strings.Contains(uri, "test.go") {
		t.Errorf("FileURI should contain filename, got %q", uri)
	}
}
