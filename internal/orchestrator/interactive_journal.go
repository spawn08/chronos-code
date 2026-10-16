package orchestrator

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

type journalCall struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	Arguments    string          `json:"arguments"`
	State        string          `json:"state"`
	ExpectedHash string          `json:"expected_hash,omitempty"`
	ExpectedPath string          `json:"expected_path,omitempty"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        string          `json:"error,omitempty"`
	Dispatch     *journalCall    `json:"dispatch,omitempty"`
}

type journalRound struct {
	Invocation    string        `json:"invocation"`
	Model         string        `json:"model,omitempty"`
	History       []byte        `json:"history"`
	Calls         []journalCall `json:"calls"`
	Uncertain     []journalCall `json:"uncertain,omitempty"`
	Recovered     bool          `json:"recovered,omitempty"`
	NeedsRecovery bool          `json:"needs_recovery,omitempty"`
}

// interactiveJournal records provider tool intent before the SDK executes any
// handler. Each receipt is durable independently of the SDK's completed round.
// It does not replace the delivery worker's stronger destination journal.
type interactiveJournal struct {
	a    *agent.Agent
	root string
	mu   sync.Mutex
}

func (j *interactiveJournal) key(ctx context.Context) string {
	key := "harness/journal/" + j.a.ID
	if identity, ok := agent.RunIdentityFromContext(ctx); ok && identity.ParentInvocationID != "" {
		key += "/" + identity.InvocationID
	}
	return key
}

func (j *interactiveJournal) load(ctx context.Context) (journalRound, error) {
	var round journalRound
	files, ok := j.a.Storage.(storage.SessionFileStore)
	if !ok {
		return round, fmt.Errorf("interactive journal requires session file storage")
	}
	if storage.SessionFromContext(ctx) == "" {
		return round, nil
	}
	data, err := files.GetFile(ctx, storage.SessionFromContext(ctx), j.key(ctx))
	if errors.Is(err, storage.ErrFileNotFound) {
		return round, nil
	}
	if err != nil {
		return round, err
	}
	if err := json.Unmarshal(data, &round); err != nil {
		return round, fmt.Errorf("decode interactive journal: %w", err)
	}
	return round, nil
}

func (j *interactiveJournal) save(ctx context.Context, round journalRound) error {
	files, ok := j.a.Storage.(storage.SessionFileStore)
	if !ok {
		return fmt.Errorf("interactive journal requires session file storage")
	}
	if storage.SessionFromContext(ctx) == "" {
		return nil
	}
	data, err := json.Marshal(round)
	if err != nil {
		return err
	}
	return files.PutFile(ctx, storage.SessionFromContext(ctx), j.key(ctx), data)
}

func (j *interactiveJournal) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || storage.SessionFromContext(ctx) == "" {
		return nil
	}
	if evt.Type == hooks.EventModelCallBefore {
		if req, ok := evt.Input.(*model.ChatRequest); ok && summaryRequest(req) {
			return nil
		}
	}
	if _, durable := execution.OperationLeaseFromContext(ctx); durable {
		return nil
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	round, err := j.load(ctx)
	if err != nil {
		return fmt.Errorf("read interactive journal: %w", err)
	}
	identity, _ := agent.RunIdentityFromContext(ctx)
	switch evt.Type {
	case hooks.EventModelCallBefore:
		req, ok := evt.Input.(*model.ChatRequest)
		if !ok || req == nil || round.Recovered || len(round.Calls) == 0 || round.Invocation == identity.InvocationID {
			return nil
		}
		incomplete := round.NeedsRecovery
		for _, call := range round.Calls {
			incomplete = incomplete || call.State == "started" || call.State == "planned" || call.State == "running"
		}
		if !incomplete {
			return nil
		}
		history, err := agent.DecodeToolRound(round.History)
		if err != nil {
			return err
		}
		for i := range round.Calls {
			call := &round.Calls[i]
			if err := j.refreshSupervisedReceipt(ctx, call); err != nil {
				return err
			}
			if call.Dispatch != nil {
				if call.Dispatch.State == "completed" {
					call.State, call.Result, call.Error = "completed", call.Dispatch.Result, call.Dispatch.Error
				} else if j.reconcileWrite(ctx, call.Dispatch) {
					call.Dispatch.State = "reconciled"
					call.State, call.Result = "reconciled", json.RawMessage(`{"recovered":true,"status":"written"}`)
				}
			}
			if call.State == "started" && j.reconcileWrite(ctx, call) {
				call.State = "reconciled"
				call.Result = json.RawMessage(`{"recovered":true,"status":"written"}`)
			}
			content := string(call.Result)
			if call.Error != "" {
				content = "Error: " + call.Error
			}
			if content == "" {
				if call.State == "planned" || call.State == "not_started" {
					content = "This call was planned but did not reach execution before interruption; it may be requested again."
					call.State = "not_started"
				} else {
					content = "Interrupted tool has an unknown outcome. Inspect the destination or saved artifacts; do not repeat its effects."
					call.State = "uncertain"
					if call.Dispatch != nil {
						call.Dispatch.State = "uncertain"
					}
				}
				call.Result, err = json.Marshal(content)
				if err != nil {
					return err
				}
			}
			history = append(history, model.Message{Role: model.RoleTool, Name: call.Name, ToolCallID: call.ID, Content: content})
		}
		var recoveryObservations []model.Message
		for i := range round.Uncertain {
			call := &round.Uncertain[i]
			if err := j.refreshSupervisedReceipt(ctx, call); err != nil {
				return err
			}
			observation, err := json.Marshal(map[string]any{"call_id": call.ID, "tool": call.Name, "state": call.State, "receipt": call.Result})
			if err != nil {
				return err
			}
			recoveryObservations = append(recoveryObservations, model.Message{Role: model.RoleUser, Name: agent.HarnessNoteName, TurnScoped: true, Content: "Original tool recovery observation (do not duplicate unresolved effects): " + string(observation)})
		}
		if req.Model != "" && round.Model != "" && req.Model != round.Model {
			history = model.StripAnthropicThinking(history)
		}
		// Preserve the live request and its system pins, but restore the original
		// native assistant/tool group before it so the model can continue safely.
		var pins, tail []model.Message
		for _, msg := range req.Messages {
			if msg.Role == model.RoleSystem {
				pins = append(pins, msg)
			} else if msg.TurnScoped || msg.Name == workingContextName || msg.Name == agent.HarnessNoteName {
				tail = append(tail, msg)
			}
		}
		for len(history) > 0 && history[0].Role == model.RoleSystem {
			history = history[1:]
		}
		freshHistory := make([]model.Message, 0, len(history))
		for _, msg := range history {
			if !msg.TurnScoped && msg.Name != workingContextName && msg.Name != agent.HarnessNoteName {
				freshHistory = append(freshHistory, msg)
			}
		}
		history = freshHistory
		var live model.Message
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == model.RoleUser && !req.Messages[i].TurnScoped && req.Messages[i].Name != workingContextName && req.Messages[i].Name != agent.HarnessNoteName {
				live = req.Messages[i]
				break
			}
		}
		req.Messages = append(pins, history...)
		if live.Content != "" {
			req.Messages = append(req.Messages, live)
		}
		req.Messages = append(req.Messages, recoveryObservations...)
		req.Messages = append(req.Messages, tail...)
		round.NeedsRecovery = true
		return j.save(ctx, round)
	case hooks.EventToolCallBefore:
		args, _ := evt.Input.(map[string]any)
		encoded, err := json.Marshal(args)
		if err != nil {
			return err
		}
		id, _ := agent.ToolCallIDFromContext(ctx)
		for _, old := range append(append([]journalCall(nil), round.Calls...), round.Uncertain...) {
			if old.ID == id && old.Name != evt.Name && old.Dispatch == nil {
				continue
			}
			if old.Dispatch != nil {
				old = *old.Dispatch
			}
			name, arguments := journalEffect(evt.Name, string(encoded))
			oldName, oldArguments := journalEffect(old.Name, old.Arguments)
			if (old.State == "uncertain" || old.State == "started" || old.State == "running" || old.State == "reconciled") && oldName == name && canonicalArgs(oldArguments) == canonicalArgs(arguments) {
				if j.reconcileWrite(ctx, &old) {
					return fmt.Errorf("previous %s effect is already present at its destination; do not repeat it", name)
				}
				return fmt.Errorf("previous %s outcome is unresolved; inspect the destination before repeating this action", name)
			}
		}
		if id == "" {
			id = "direct-" + rand.Text()
		}
		if evt.Metadata == nil {
			evt.Metadata = map[string]any{}
		}
		evt.Metadata["interactive_call_id"] = id
		found := false
		for i := range round.Calls {
			if round.Calls[i].ID == id {
				call := &round.Calls[i]
				if call.Name != evt.Name {
					call.Dispatch = &journalCall{ID: id, Name: evt.Name, Arguments: string(encoded)}
					call = call.Dispatch
				}
				call.State = "started"
				if evt.Name == "file_write" || evt.Name == "file_edit" {
					call.ExpectedHash = expectedWriteHash(ctx, j.root, args)
					call.ExpectedPath = journalWritePath(ctx, j.root, args)
				}
				found = true
			}
		}
		if !found {
			call := journalCall{ID: id, Name: evt.Name, Arguments: string(encoded), State: "started"}
			if evt.Name == "file_write" || evt.Name == "file_edit" {
				call.ExpectedHash = expectedWriteHash(ctx, j.root, args)
				call.ExpectedPath = journalWritePath(ctx, j.root, args)
			}
			if len(round.History) == 0 {
				round.Invocation = identity.InvocationID
				round.History, err = agent.EncodeToolRound([]model.Message{{Role: model.RoleUser, Content: "Resume an interrupted direct tool operation."}, {Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: id, Name: evt.Name, Arguments: string(encoded)}}}})
				if err != nil {
					return err
				}
			}
			round.Calls = append(round.Calls, call)
		}
		return j.save(ctx, round)
	}
	return nil
}

func (j *interactiveJournal) After(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || storage.SessionFromContext(ctx) == "" {
		return nil
	}
	if _, durable := execution.OperationLeaseFromContext(ctx); durable {
		return nil
	}
	ctx, cancel := durableObservationContext(ctx)
	defer cancel()
	j.mu.Lock()
	defer j.mu.Unlock()
	switch evt.Type {
	case hooks.EventModelCallAfter:
		resp, ok := evt.Output.(*model.ChatResponse)
		req, requestOK := evt.Input.(*model.ChatRequest)
		if evt.Error != nil || !ok || !requestOK || resp == nil || req == nil || summaryRequest(req) {
			return nil
		}
		if len(resp.ToolCalls) == 0 {
			prior, err := j.load(ctx)
			if err != nil {
				return err
			}
			prior.Recovered = true
			for _, call := range append(append([]journalCall(nil), prior.Calls...), prior.Uncertain...) {
				if call.State == "running" || call.Dispatch != nil && call.Dispatch.State == "running" {
					prior.Recovered, prior.NeedsRecovery = false, true
				}
			}
			return j.save(ctx, prior)
		}
		history := append(append([]model.Message(nil), req.Messages...), model.Message{Role: model.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls, ProviderState: resp.ProviderState})
		data, err := agent.EncodeToolRound(history)
		if err != nil {
			return err
		}
		identity, _ := agent.RunIdentityFromContext(ctx)
		round := journalRound{Invocation: identity.InvocationID, Model: req.Model, History: data, NeedsRecovery: true}
		// Uncertain effects survive later successful rounds, rather than losing
		// their replay protection when the model proceeds with another tool.
		prior, err := j.load(ctx)
		if err != nil {
			return err
		}
		for _, call := range prior.Uncertain {
			if call.State != "completed" && call.State != "reconciled" {
				round.Uncertain = append(round.Uncertain, call)
			}
		}
		for _, call := range prior.Calls {
			if call.State == "uncertain" || call.State == "started" || call.State == "running" {
				if call.State == "started" {
					call.State = "uncertain"
				}
				round.Uncertain = append(round.Uncertain, call)
			}
		}
		for _, call := range resp.ToolCalls {
			round.Calls = append(round.Calls, journalCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments, State: "planned"})
		}
		return j.save(ctx, round)
	case hooks.EventToolCallAfter:
		round, err := j.load(ctx)
		if err != nil {
			return err
		}
		id, _ := agent.ToolCallIDFromContext(ctx)
		if id == "" && evt.Metadata != nil {
			id, _ = evt.Metadata["interactive_call_id"].(string)
		}
		matched := false
		for i := range round.Calls {
			if round.Calls[i].ID != id {
				continue
			}
			matched = true
			call := &round.Calls[i]
			if call.Name != evt.Name {
				if call.Dispatch == nil || call.Dispatch.Name != evt.Name {
					return fmt.Errorf("tool receipt does not match journaled intent")
				}
				call = call.Dispatch
			}
			call.State, call.Error, call.Result = "completed", "", nil
			if evt.Error != nil {
				call.Error = evt.Error.Error()
			} else {
				data, err := json.Marshal(evt.Output)
				if err != nil {
					return err
				}
				call.Result = data
				if pendingSupervisedReceipt(data) {
					call.State = "running"
					round.NeedsRecovery = true
				}
			}
		}
		if !matched {
			return fmt.Errorf("tool receipt has no journaled call ID %q", id)
		}
		return j.save(ctx, round)
	}
	return nil
}

func expectedWriteHash(ctx context.Context, root string, args map[string]any) string {
	path, _ := args["path"].(string)
	if path == "" {
		return ""
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(builtins.WorkspaceRoot(ctx, root), path)
	}
	content, whole := args["content"].(string)
	if builtins.IsFileEdit(args) {
		data, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		old, _ := args["old_content"].(string)
		next, ok := args["new_content"].(string)
		if old == "" || !ok {
			return ""
		}
		n := strings.Count(string(data), old)
		all, _ := args["replace_all"].(bool)
		if n == 0 || n > 1 && !all {
			return ""
		}
		content = strings.Replace(string(data), old, next, 1)
		if all {
			content = strings.ReplaceAll(string(data), old, next)
		}
	} else if !whole {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(content)))
}

func (j *interactiveJournal) reconcileWrite(ctx context.Context, call *journalCall) bool {
	if call.ExpectedHash == "" {
		return false
	}
	state := readFileState(builtins.WorkspaceRoot(ctx, j.root), call.ExpectedPath)
	return state.exists && state.hash == call.ExpectedHash
}

// Bound journal writes after cancellation so a cancelled request can still
// preserve its last result without extending the lifetime of active work.
func durableObservationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func journalEffect(name, arguments string) (string, string) {
	if name == "call_tool" {
		var dispatch struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal([]byte(arguments), &dispatch) == nil && dispatch.Name != "" {
			return dispatch.Name, string(dispatch.Arguments)
		}
	}
	return name, arguments
}

func journalWritePath(ctx context.Context, root string, args map[string]any) string {
	path, _ := args["path"].(string)
	if path != "" && !filepath.IsAbs(path) {
		path = filepath.Join(builtins.WorkspaceRoot(ctx, root), path)
	}
	return path
}

func pendingSupervisedReceipt(data []byte) bool {
	var receipt struct {
		ID     string `json:"tool_run_id"`
		Status string `json:"status"`
	}
	return json.Unmarshal(data, &receipt) == nil && receipt.ID != "" && receipt.Status != "completed"
}

func (j *interactiveJournal) refreshSupervisedReceipt(ctx context.Context, call *journalCall) error {
	if call.Dispatch != nil {
		if err := j.refreshSupervisedReceipt(ctx, call.Dispatch); err != nil {
			return err
		}
		if call.Dispatch.State == "running" {
			call.State, call.Result = "running", call.Dispatch.Result
		}
	}
	if call.State != "running" {
		return nil
	}
	var receipt map[string]any
	if err := json.Unmarshal(call.Result, &receipt); err != nil {
		return fmt.Errorf("decode pending tool receipt: %w", err)
	}
	id, _ := receipt["tool_run_id"].(string)
	if _, err := artifactPath(id); err != nil {
		return fmt.Errorf("invalid pending tool run: %w", err)
	}
	files := j.a.Storage.(storage.SessionFileStore)
	data, err := files.GetFile(ctx, storage.SessionFromContext(ctx), "harness/runs/"+id)
	if err != nil && !errors.Is(err, storage.ErrFileNotFound) {
		return fmt.Errorf("read pending tool receipt: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &receipt); err != nil {
			return fmt.Errorf("decode saved tool receipt: %w", err)
		}
		if receipt["status"] == "completed" {
			call.State, call.Result = "completed", data
			return nil
		}
	}
	receipt["hint"] = "The original tool run has no durable terminal receipt. Use await_tool to recover its live status or reconcile its destination; do not repeat its effects."
	call.Result, err = json.Marshal(receipt)
	return err
}
