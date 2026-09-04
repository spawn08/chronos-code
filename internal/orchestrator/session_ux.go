package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos/engine/hooks"

	"github.com/spawn08/chronos-code/internal/learning"
	"github.com/spawn08/chronos-code/internal/projectdocs"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
)

const planModePrompt = "PLAN MODE is on. Propose a numbered plan only. Do not call file_write, shell, or any mutating tool. Wait for the user to run /plan off before editing."

type fileCheckpoint struct {
	Path    string
	Prev    []byte
	Existed bool
}

type sessionUXHook struct {
	orchestrator *Orchestrator
}

func (h sessionUXHook) Before(_ context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventToolCallBefore || h.orchestrator == nil {
		return nil
	}
	if h.orchestrator.PlanMode() && mutatingTool(evt.Name) {
		return fmt.Errorf("plan mode: %s is blocked until /plan off", evt.Name)
	}
	if evt.Name == "file_write" {
		h.orchestrator.snapshotWrite(evt.Input)
	}
	return nil
}

func (h sessionUXHook) After(_ context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventToolCallAfter || h.orchestrator == nil {
		return nil
	}
	if evt.Name == "file_write" && evt.Error == nil {
		h.orchestrator.commitWrite(evt.Input)
	}
	return nil
}

func mutatingTool(name string) bool {
	switch name {
	case "file_write", "shell", "shell_auto":
		return true
	default:
		return false
	}
}

func (o *Orchestrator) recordRoute(classification router.Classification, agentID string) {
	if o == nil {
		return
	}
	o.lastExecMu.Lock()
	o.lastExecRoute = classification
	o.lastExecAgent = agentID
	o.lastExecMu.Unlock()
}

func (o *Orchestrator) LastRouteStatus() string {
	if o == nil {
		return "route:—"
	}
	o.lastExecMu.Lock()
	defer o.lastExecMu.Unlock()
	if o.lastExecRoute.Complexity == "" {
		return "route:—"
	}
	return fmt.Sprintf("route:%s/%s", o.lastExecRoute.Complexity, o.lastExecRoute.Kind)
}

func (o *Orchestrator) PlanMode() bool {
	return o != nil && o.planMode.Load()
}

func (o *Orchestrator) SetPlanMode(enabled bool) {
	if o != nil {
		o.planMode.Store(enabled)
	}
}

func (o *Orchestrator) ResumeSession(ctx context.Context, sessionID string) (string, error) {
	if o == nil || o.sessionMgr == nil {
		return "", fmt.Errorf("session manager unavailable")
	}
	agentID := o.active
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		latest, err := o.sessionMgr.Latest(ctx, agentID)
		if err != nil {
			return "", fmt.Errorf("find latest session: %w", err)
		}
		if latest == nil {
			return "", fmt.Errorf("no prior session to resume")
		}
		sessionID = latest.ID
	}
	if err := o.sessionMgr.Ensure(ctx, sessionID, agentID); err != nil {
		return "", fmt.Errorf("resume session %s: %w", sessionID, err)
	}
	o.sessionMu.Lock()
	o.sessions[agentID] = sessionID
	o.sessionMu.Unlock()
	return sessionID, nil
}

func (o *Orchestrator) StartupHints(ctx context.Context) string {
	if o == nil {
		return ""
	}
	var parts []string
	if o.sessionMgr != nil {
		if latest, err := o.sessionMgr.Latest(ctx, o.active); err == nil && latest != nil && latest.ID != o.CurrentSessionID() {
			parts = append(parts, "/resume continues "+shortSessionID(latest.ID))
		}
	}
	if store := o.suggestionStore(); store != nil {
		if pending, err := store.List(); err == nil && len(pending) > 0 {
			parts = append(parts, fmt.Sprintf("%d learning suggestion(s) · /learn", len(pending)))
		}
	}
	return strings.Join(parts, " · ")
}

func (o *Orchestrator) GraphStatus(ctx context.Context) string {
	if o == nil || o.graphStore == nil {
		return "graph: unavailable (T0 tools disabled)"
	}
	stats, err := o.graphStore.Stats(ctx)
	if err != nil {
		return "graph: " + err.Error()
	}
	return fmt.Sprintf("graph: %d files · %d symbols · %d edges · prefer codebase_search before file_read", stats.Files, stats.Symbols, stats.Edges)
}

func (o *Orchestrator) ProjectInstructionFiles() []string {
	if o == nil || o.cfg == nil {
		return nil
	}
	root := o.cfg.Workspace.Root
	if root == "" {
		return nil
	}
	bundle, err := projectdocs.Load(root, root)
	if err != nil || bundle == nil {
		return nil
	}
	names := make([]string, 0, len(bundle.Docs))
	for _, doc := range bundle.Docs {
		names = append(names, doc.RelPath)
	}
	return names
}

func (o *Orchestrator) SandboxStatus() string {
	root := ""
	if o != nil && o.cfg != nil {
		root = o.cfg.Workspace.Root
	}
	if root == "" {
		root = "."
	}
	if _, err := security.NewOSSandbox(root, false); err != nil {
		return "sandbox: unavailable (" + err.Error() + ")"
	}
	return "sandbox: helper ready (opt-in; shell still uses host policy until a runtime bind ships)"
}

func (o *Orchestrator) ListPendingSuggestions() ([]*learning.Suggestion, error) {
	store := o.suggestionStore()
	if store == nil {
		return nil, fmt.Errorf("learning store unavailable")
	}
	return store.List()
}

func (o *Orchestrator) AcceptSuggestion(id string) error {
	store := o.suggestionStore()
	if store == nil {
		return fmt.Errorf("learning store unavailable")
	}
	return store.Accept(id)
}

func (o *Orchestrator) RejectSuggestion(id string) error {
	store := o.suggestionStore()
	if store == nil {
		return fmt.Errorf("learning store unavailable")
	}
	return store.Reject(id)
}

func (o *Orchestrator) suggestionStore() *learning.Store {
	if o == nil || o.cfg == nil {
		return nil
	}
	dir := o.cfg.Learning.OutputDir
	if dir == "" {
		dir = ".chronos-code/learned"
	}
	if !filepath.IsAbs(dir) && o.cfg.Workspace.Root != "" {
		dir = filepath.Join(o.cfg.Workspace.Root, dir)
	}
	return learning.NewStore(dir)
}

func (o *Orchestrator) snapshotWrite(input any) {
	path, abs, ok := o.writePath(input)
	if !ok {
		return
	}
	cp := fileCheckpoint{Path: path}
	data, err := os.ReadFile(abs)
	if err == nil {
		cp.Existed = true
		cp.Prev = data
	}
	o.editsMu.Lock()
	o.edits = append(o.edits, cp)
	o.editsMu.Unlock()
}

func (o *Orchestrator) commitWrite(input any) {
	// Snapshot is taken before the write. A failed write leaves a harmless
	// extra checkpoint that UndoLastEdit can skip if the file already matches.
	_, _, _ = o.writePath(input)
}

func (o *Orchestrator) UndoLastEdit() (string, error) {
	o.editsMu.Lock()
	defer o.editsMu.Unlock()
	if len(o.edits) == 0 {
		return "", fmt.Errorf("nothing to undo")
	}
	cp := o.edits[len(o.edits)-1]
	o.edits = o.edits[:len(o.edits)-1]
	abs := o.resolvePath(cp.Path)
	if !cp.Existed {
		if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
			return "", fmt.Errorf("undo create %s: %w", cp.Path, err)
		}
		return cp.Path, nil
	}
	if err := os.WriteFile(abs, cp.Prev, 0o644); err != nil {
		return "", fmt.Errorf("undo edit %s: %w", cp.Path, err)
	}
	return cp.Path, nil
}

func (o *Orchestrator) writePath(input any) (rel, abs string, ok bool) {
	args, _ := input.(map[string]any)
	path, _ := args["path"].(string)
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", false
	}
	return path, o.resolvePath(path), true
}

func (o *Orchestrator) resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	root := ""
	if o != nil && o.cfg != nil {
		root = o.cfg.Workspace.Root
	}
	if root == "" {
		return path
	}
	return filepath.Join(root, path)
}

func shortSessionID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
