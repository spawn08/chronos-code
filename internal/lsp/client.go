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
	"time"
)

const (
	shutdownTimeout = 2 * time.Second
	killTimeout     = 2 * time.Second
)

// Client is a minimal LSP JSON-RPC client communicating with a language
// server over stdio.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader

	writeMu sync.Mutex
	stateMu sync.Mutex
	nextID  int
	pending map[int]chan requestResult
	readErr error

	processDone chan error
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error

	diagMu      sync.Mutex
	diagnostics map[string][]Diagnostic // uri -> diagnostics from notifications
}

type requestResult struct {
	response *jsonrpcResponse
	err      error
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
		pending:     make(map[int]chan requestResult),
		processDone: make(chan error, 1),
		closeDone:   make(chan struct{}),
		diagnostics: make(map[string][]Diagnostic),
	}
	go c.readLoop()
	go func() {
		c.processDone <- cmd.Wait()
	}()
	return c, nil
}

// Initialize sends the LSP initialize request and the initialized
// notification.
func (c *Client) Initialize(ctx context.Context, rootURI string) error {
	if !strings.HasPrefix(rootURI, "file://") {
		rootURI = FileURI(rootURI)
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
	c.closeOnce.Do(func() {
		c.closeErr = c.close()
		close(c.closeDone)
	})
	<-c.closeDone
	return c.closeErr
}

func (c *Client) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	shutdownDone := make(chan struct{})
	go func() {
		_ = c.Shutdown(ctx)
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-ctx.Done():
	}
	// os.File.Close is safe concurrently and interrupts a blocked pipe write.
	_ = c.stdin.Close()

	timer := time.NewTimer(shutdownTimeout)
	defer timer.Stop()
	select {
	case err := <-c.processDone:
		return err
	case <-timer.C:
	}

	if err := c.cmd.Process.Kill(); err != nil {
		return fmt.Errorf("lsp: kill server: %w", err)
	}
	timer.Reset(killTimeout)
	select {
	case err := <-c.processDone:
		return err
	case <-timer.C:
		return fmt.Errorf("lsp: server did not exit after kill")
	}
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
	return append([]Diagnostic(nil), c.diagnostics[uri]...), nil
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
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Clean(abs))}).String()
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
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonrpcReply struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *Client) request(ctx context.Context, method string, params any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	c.stateMu.Lock()
	if c.readErr != nil {
		err := c.readErr
		c.stateMu.Unlock()
		return nil, err
	}
	c.nextID++
	id := c.nextID
	result := make(chan requestResult, 1)
	c.pending[id] = result
	c.stateMu.Unlock()

	sent := make(chan error, 1)
	go func() {
		sent <- c.send(&jsonrpcRequest{
			JSONRPC: "2.0",
			ID:      &id,
			Method:  method,
			Params:  params,
		})
	}()
	select {
	case err := <-sent:
		if err == nil {
			break
		}
		c.removePending(id)
		return nil, err
	case <-ctx.Done():
		if c.removePending(id) {
			go func() { _ = c.notify("$/cancelRequest", map[string]any{"id": id}) }()
		}
		return nil, ctx.Err()
	}

	select {
	case received := <-result:
		if received.err != nil {
			return nil, received.err
		}
		if received.response.Error != nil {
			return nil, fmt.Errorf("lsp error %d: %s", received.response.Error.Code, received.response.Error.Message)
		}
		var value any
		if len(received.response.Result) > 0 {
			if err := json.Unmarshal(received.response.Result, &value); err != nil {
				return nil, fmt.Errorf("lsp: unmarshal result: %w", err)
			}
		}
		return value, nil
	case <-ctx.Done():
		if c.removePending(id) {
			go func() { _ = c.notify("$/cancelRequest", map[string]any{"id": id}) }()
		}
		return nil, ctx.Err()
	}
}

func (c *Client) removePending(id int) bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	if _, ok := c.pending[id]; !ok {
		return false
	}
	delete(c.pending, id)
	return true
}

func (c *Client) notify(method string, params any) error {
	return c.send(&jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	})
}

func (c *Client) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("lsp: marshal: %w", err)
	}
	header := fmt.Sprintf("Content-Length: %d\r\n\r\n", len(data))
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := io.WriteString(c.stdin, header); err != nil {
		return fmt.Errorf("lsp: write header: %w", err)
	}
	if _, err := c.stdin.Write(data); err != nil {
		return fmt.Errorf("lsp: write body: %w", err)
	}
	return nil
}

func (c *Client) readLoop() {
	for {
		msg, err := c.readMessage()
		if err != nil {
			c.failPending(err)
			return
		}
		if msg.Method != "" {
			if len(msg.ID) > 0 {
				if err := c.replyToServerRequest(msg); err != nil {
					c.failPending(err)
					return
				}
			} else if msg.Method == "textDocument/publishDiagnostics" {
				c.handleDiagnostics(msg.Params)
			}
			continue
		}

		id, err := responseID(msg.ID)
		if err != nil {
			continue
		}
		c.stateMu.Lock()
		result := c.pending[id]
		delete(c.pending, id)
		c.stateMu.Unlock()
		if result != nil {
			result <- requestResult{response: msg}
		}
	}
}

func responseID(raw json.RawMessage) (int, error) {
	var id int
	if len(raw) == 0 || json.Unmarshal(raw, &id) != nil {
		return 0, fmt.Errorf("lsp: invalid response id %s", raw)
	}
	return id, nil
}

func (c *Client) failPending(err error) {
	err = fmt.Errorf("lsp: reader: %w", err)
	c.stateMu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	results := make([]chan requestResult, 0, len(c.pending))
	for id, result := range c.pending {
		results = append(results, result)
		delete(c.pending, id)
	}
	c.stateMu.Unlock()
	for _, result := range results {
		result <- requestResult{err: err}
	}
}

func (c *Client) replyToServerRequest(msg *jsonrpcResponse) error {
	reply := &jsonrpcReply{JSONRPC: "2.0", ID: msg.ID}
	switch msg.Method {
	case "client/registerCapability", "client/unregisterCapability", "window/workDoneProgress/create":
		reply.Result = json.RawMessage("null")
	case "workspace/configuration":
		var params struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			reply.Error = &jsonrpcError{Code: -32602, Message: "invalid params"}
			break
		}
		reply.Result = make([]any, len(params.Items))
	default:
		reply.Error = &jsonrpcError{Code: -32601, Message: "method not found"}
	}
	if err := c.send(reply); err != nil {
		return fmt.Errorf("lsp: reply to %s: %w", msg.Method, err)
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
		name, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			n, err := strconv.Atoi(strings.TrimSpace(value))
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
		receivedAt := time.Now().UTC()
		for i := range params.Diagnostics {
			params.Diagnostics[i].ReceivedAt = receivedAt
		}
		c.diagMu.Lock()
		c.diagnostics[params.URI] = append([]Diagnostic(nil), params.Diagnostics...)
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
