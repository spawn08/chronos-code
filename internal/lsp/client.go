//go:build lsp

package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Client is a minimal LSP JSON-RPC client communicating with a language
// server over stdio.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	mu     sync.Mutex
	nextID int

	diagMu      sync.Mutex
	diagnostics map[string][]Diagnostic // uri -> diagnostics from notifications
}

// NewClient starts a language server process and returns a client connected
// to its stdin/stdout. Call Initialize before making any requests.
func NewClient(command string, args ...string) (*Client, error) {
	cmd := exec.Command(command, args...)
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdin pipe: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("lsp: stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start %s: %w", command, err)
	}

	c := &Client{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      bufio.NewReaderSize(stdoutPipe, 1<<20),
		diagnostics: make(map[string][]Diagnostic),
	}
	return c, nil
}

// Initialize sends the LSP initialize request and the initialized
// notification.
func (c *Client) Initialize(ctx context.Context, rootURI string) error {
	if !strings.HasPrefix(rootURI, "file://") {
		abs, err := filepath.Abs(rootURI)
		if err != nil {
			return fmt.Errorf("lsp: abs path: %w", err)
		}
		rootURI = "file://" + abs
	}

	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   rootURI,
		"capabilities": map[string]any{
			"textDocument": map[string]any{
				"hover":      map[string]any{},
				"references": map[string]any{},
				"rename":     map[string]any{"prepareSupport": true},
				"publishDiagnostics": map[string]any{
					"relatedInformation": true,
				},
			},
		},
	}

	_, err := c.request(ctx, "initialize", params)
	if err != nil {
		return fmt.Errorf("lsp: initialize: %w", err)
	}

	if err := c.notify("initialized", map[string]any{}); err != nil {
		return fmt.Errorf("lsp: initialized notification: %w", err)
	}
	return nil
}

// Shutdown sends the LSP shutdown request.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.request(ctx, "shutdown", nil)
	if err != nil {
		return fmt.Errorf("lsp: shutdown: %w", err)
	}
	return c.notify("exit", nil)
}

// Close shuts down the language server and waits for exit.
func (c *Client) Close() error {
	ctx := context.Background()
	_ = c.Shutdown(ctx)
	_ = c.stdin.Close()
	return c.cmd.Wait()
}

// DidOpen notifies the server that a file has been opened, which triggers
// diagnostics for that file.
func (c *Client) DidOpen(ctx context.Context, uri, languageID, text string) error {
	params := map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": languageID,
			"version":    1,
			"text":       text,
		},
	}
	return c.notify("textDocument/didOpen", params)
}

// Diagnostics returns the most recently received diagnostics for uri. The
// server pushes diagnostics asynchronously via textDocument/publishDiagnostics
// notifications; this method returns whatever has been collected so far. Call
// DidOpen first to trigger diagnostic computation.
func (c *Client) Diagnostics(_ context.Context, uri string) ([]Diagnostic, error) {
	c.diagMu.Lock()
	defer c.diagMu.Unlock()
	return c.diagnostics[uri], nil
}

// Hover sends a textDocument/hover request.
func (c *Client) Hover(ctx context.Context, uri string, line, col int) (*HoverResult, error) {
	params := textDocPosition(uri, line, col)
	raw, err := c.request(ctx, "textDocument/hover", params)
	if err != nil {
		return nil, fmt.Errorf("lsp: hover: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	var result HoverResult
	if err := remarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("lsp: parse hover: %w", err)
	}
	return &result, nil
}

// References sends a textDocument/references request.
func (c *Client) References(ctx context.Context, uri string, line, col int) ([]Location, error) {
	params := textDocPosition(uri, line, col)
	params["context"] = map[string]any{"includeDeclaration": true}
	raw, err := c.request(ctx, "textDocument/references", params)
	if err != nil {
		return nil, fmt.Errorf("lsp: references: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	var locs []Location
	if err := remarshal(raw, &locs); err != nil {
		return nil, fmt.Errorf("lsp: parse references: %w", err)
	}
	return locs, nil
}

// RenamePreview sends a textDocument/rename request and returns the
// resulting workspace edit without applying it.
func (c *Client) RenamePreview(ctx context.Context, uri string, line, col int, newName string) (*WorkspaceEdit, error) {
	params := textDocPosition(uri, line, col)
	params["newName"] = newName
	raw, err := c.request(ctx, "textDocument/rename", params)
	if err != nil {
		return nil, fmt.Errorf("lsp: rename: %w", err)
	}
	if raw == nil {
		return nil, nil
	}
	var edit WorkspaceEdit
	if err := remarshal(raw, &edit); err != nil {
		return nil, fmt.Errorf("lsp: parse rename: %w", err)
	}
	return &edit, nil
}

func textDocPosition(uri string, line, col int) map[string]any {
	return map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     map[string]any{"line": line, "character": col},
	}
}

// FileURI converts a filesystem path to a file:// URI.
func FileURI(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	return "file://" + url.PathEscape(abs)
}

// --- JSON-RPC transport ---

type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int   `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) request(_ context.Context, method string, params any) (any, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.nextID++
	id := c.nextID
	if err := c.send(&jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}); err != nil {
		return nil, err
	}

	for {
		resp, err := c.readMessage()
		if err != nil {
			return nil, err
		}
		if resp.Method == "textDocument/publishDiagnostics" {
			c.handleDiagnostics(resp.Params)
			continue
		}
		if resp.Method != "" {
			continue
		}
		if resp.ID != nil && *resp.ID == id {
			if resp.Error != nil {
				return nil, fmt.Errorf("lsp error %d: %s", resp.Error.Code, resp.Error.Message)
			}
			var result any
			if len(resp.Result) > 0 {
				_ = json.Unmarshal(resp.Result, &result)
			}
			return result, nil
		}
	}
}

func (c *Client) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.send(&jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}

func (c *Client) send(msg *jsonrpcRequest) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("lsp: marshal: %w", err)
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	if _, err := io.WriteString(c.stdin, header); err != nil {
		return fmt.Errorf("lsp: write header: %w", err)
	}
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("lsp: write body: %w", err)
	}
	return nil
}

func (c *Client) readMessage() (*jsonrpcResponse, error) {
	contentLen := 0
	for {
		line, err := c.stdout.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("lsp: read header: %w", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "Content-Length: ") {
			n, err := strconv.Atoi(strings.TrimPrefix(line, "Content-Length: "))
			if err != nil {
				return nil, fmt.Errorf("lsp: parse content-length: %w", err)
			}
			contentLen = n
		}
	}
	if contentLen == 0 {
		return nil, fmt.Errorf("lsp: missing Content-Length")
	}

	body := make([]byte, contentLen)
	if _, err := io.ReadFull(c.stdout, body); err != nil {
		return nil, fmt.Errorf("lsp: read body: %w", err)
	}

	var resp jsonrpcResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("lsp: unmarshal response: %w", err)
	}
	return &resp, nil
}

func (c *Client) handleDiagnostics(raw json.RawMessage) {
	var params struct {
		URI         string       `json:"uri"`
		Diagnostics []Diagnostic `json:"diagnostics"`
	}
	if json.Unmarshal(raw, &params) == nil {
		c.diagMu.Lock()
		c.diagnostics[params.URI] = params.Diagnostics
		c.diagMu.Unlock()
	}
}

func remarshal(src any, dst any) error {
	data, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

// FormatRequest is exported for testing the JSON-RPC framing layer.
func FormatRequest(id int, method string, params any) ([]byte, error) {
	req := &jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	return append([]byte(header), data...), nil
}

// ParseResponse is exported for testing the JSON-RPC framing layer.
func ParseResponse(r *bufio.Reader) (*jsonrpcResponse, error) {
	c := &Client{stdout: r}
	return c.readMessage()
}
