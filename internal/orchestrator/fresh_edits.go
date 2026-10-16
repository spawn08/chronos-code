package orchestrator

import (
	"context"
	"fmt"
	"github.com/spawn08/chronos-code/internal/security"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
)

type editReadState struct {
	mu    sync.Mutex
	reads map[string]string
}

func editReadKey(ctx context.Context, path string) string {
	id, _ := agent.RunIdentityFromContext(ctx)
	return toolOwner(ctx) + "\x00" + id.InvocationID + "\x00" + path
}

func (o *Orchestrator) installFreshEdits(a *agent.Agent, root string) {
	if o.cfg == nil || !o.cfg.Tools.FreshEdits || a.Tools == nil {
		return
	}
	state := &editReadState{reads: map[string]string{}}
	if read, ok := a.Tools.Get("file_read"); ok {
		original := read.Handler
		read.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			path, _ := args["path"].(string)
			before := readFileState(builtins.WorkspaceRoot(ctx, root), path)
			out, err := original(ctx, args)
			values, ok := out.(map[string]any)
			if err != nil || !ok || truthy(values["outline"]) || truthy(values["document"]) {
				return out, err
			}
			resolved, _ := values["path"].(string)
			after := readFileState(builtins.WorkspaceRoot(ctx, root), resolved)
			if before != after {
				return nil, fmt.Errorf("file changed while reading; read current source again")
			}
			state.mu.Lock()
			if len(state.reads) >= 512 {
				state.reads = map[string]string{}
			}
			state.reads[editReadKey(ctx, resolved)] = after.hash
			state.mu.Unlock()
			values["content_hash"] = after.hash
			return values, nil
		}
	}
	write, ok := a.Tools.Get("file_write")
	if !ok {
		return
	}
	props, _ := write.Parameters["properties"].(map[string]any)
	if props != nil {
		props["expected_hash"] = map[string]any{"type": "string", "description": "content_hash from your source read; rejects a changed file before editing"}
	}
	original := write.Handler
	write.Handler = func(ctx context.Context, args map[string]any) (any, error) {
		path, _ := args["path"].(string)
		resolved := path
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(builtins.WorkspaceRoot(ctx, root), resolved)
		}
		resolved = filepath.Clean(resolved)
		current := readFileState(builtins.WorkspaceRoot(ctx, root), resolved)
		if current.exists {
			expected, _ := args["expected_hash"].(string)
			if expected == "" {
				state.mu.Lock()
				expected = state.reads[editReadKey(ctx, resolved)]
				state.mu.Unlock()
			}
			if expected == "" || expected != current.hash {
				return nil, fmt.Errorf("file_write: read the current source before editing %s (missing or stale content_hash)", path)
			}
		}
		out, err := original(ctx, args)
		if err != nil {
			return out, err
		}
		after := readFileState(builtins.WorkspaceRoot(ctx, root), resolved)
		state.mu.Lock()
		state.reads[editReadKey(ctx, resolved)] = after.hash
		state.mu.Unlock()
		values, ok := out.(map[string]any)
		if ok {
			values["content_hash"] = after.hash
			if o.cfg.Tools.PostEditDiagnostics {
				if filepath.Ext(path) == ".go" {
					_, parseErr := parser.ParseFile(token.NewFileSet(), resolved, nil, parser.AllErrors)
					values["syntax_error"] = errorText(parseErr)
				} else if diagnostics, ok := a.Tools.Get("lsp_diagnostics"); ok {
					checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
					_ = diagnostics
					result, checkErr := a.Tools.Execute(checkCtx, "lsp_diagnostics", map[string]any{"file": path})
					cancel()
					if checkErr == nil {
						values["diagnostics"] = result
					} else {
						values["diagnostics_unavailable"] = checkErr.Error()
					}
				}
			}
		}
		return out, nil
	}
	// Multiple exact replacements are validated in memory, then committed once
	// through file_write's authorization, checkpoint and evidence path.
	a.Tools.Register(&tool.Definition{Name: "file_edit", Permission: write.Permission, Effects: write.Effects, Description: "Atomically apply several unique exact replacements to one file. Read source first and supply expected_hash=content_hash. Every replacement must match once; no partial edits are committed.", Parameters: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "expected_hash": map[string]any{"type": "string"}, "edits": map[string]any{"type": "array", "minItems": 1, "maxItems": 50, "items": map[string]any{"type": "object", "properties": map[string]any{"old_content": map[string]any{"type": "string"}, "new_content": map[string]any{"type": "string"}}, "required": []string{"old_content", "new_content"}}}}, "required": []string{"path", "expected_hash", "edits"}}, Handler: func(ctx context.Context, args map[string]any) (any, error) {
		path, _ := args["path"].(string)
		resolved := path
		if !filepath.IsAbs(path) {
			resolved = filepath.Join(builtins.WorkspaceRoot(ctx, root), path)
		}
		// Guard runs before reading even though the outer alias has a new name.
		readEvent := &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_read", Input: map[string]any{"path": path}}
		for _, hook := range a.Hooks {
			if guard, ok := hook.(*security.Guard); ok {
				if err := guard.Before(ctx, readEvent); err != nil {
					return nil, err
				}
			}
		}
		data, err := os.ReadFile(resolved)
		if err != nil {
			return nil, err
		}
		content := string(data)
		edits, ok := args["edits"].([]any)
		if !ok || len(edits) == 0 || len(edits) > 50 {
			return nil, fmt.Errorf("file_edit requires 1–50 edits")
		}
		for _, raw := range edits {
			edit, ok := raw.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid edit")
			}
			old, _ := edit["old_content"].(string)
			next, ok := edit["new_content"].(string)
			if old == "" || !ok || strings.Count(content, old) != 1 {
				return nil, fmt.Errorf("file_edit: old_content must match exactly once")
			}
			content = strings.Replace(content, old, next, 1)
		}
		writeArgs := map[string]any{"path": path, "content": content, "expected_hash": args["expected_hash"]}
		evt := &hooks.Event{Type: hooks.EventToolCallBefore, Name: "file_write", Input: writeArgs}
		if err := a.Hooks.Before(ctx, evt); err != nil {
			return nil, err
		}
		result, writeErr := a.Tools.Execute(ctx, "file_write", writeArgs)
		evt.Type, evt.Output, evt.Error = hooks.EventToolCallAfter, result, writeErr
		if err := a.Hooks.After(ctx, evt); err != nil {
			return result, err
		}
		return result, writeErr
	}})
}
