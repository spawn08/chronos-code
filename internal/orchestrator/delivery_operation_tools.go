package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/execution"
)

// wrapDeliveryOperations journals effectful tool calls only when a durable
// worker lease is present. It wraps both built-ins and late MCP tools through
// the same installation path; bounded execution-v1 remains unchanged.
func wrapDeliveryOperations(a *agent.Agent) {
	if a == nil || a.Tools == nil {
		return
	}
	for _, definition := range a.Tools.List() {
		if definition == nil || definition.Handler == nil {
			continue
		}
		wrapped := *definition
		original := definition.Handler
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			lease, ok := execution.OperationLeaseFromContext(ctx)
			if !ok {
				return original(ctx, args)
			}
			effects := wrapped.Effects
			if wrapped.ResolveEffects != nil {
				var err error
				effects, err = wrapped.ResolveEffects(ctx, args)
				if err != nil {
					return nil, execution.EffectJournalError{Err: fmt.Errorf("resolve %s effects: %w", wrapped.Name, err)}
				}
			}
			if !hasDeliveryEffect(effects) {
				return original(ctx, args)
			}
			callID, ok := agent.ToolCallIDFromContext(ctx)
			identity, identityOK := agent.RunIdentityFromContext(ctx)
			if !ok || !identityOK || identity.InvocationID == "" {
				return nil, execution.EffectJournalError{Err: fmt.Errorf("effectful tool %s has no host-bound call identity", wrapped.Name)}
			}
			root := builtins.WorkspaceRoot(ctx, "")
			before := fileState{}
			path, _ := args["path"].(string)
			class := execution.ReplayUnknown
			observationPath, expectedFingerprint := "", ""
			if wrapped.Name == "file_write" {
				if safe, ok := safeObservationPath(root, path); ok {
					class, observationPath = execution.ReplayFingerprintedWrite, safe
					before = readFileState(root, safe)
					content, _ := args["content"].(string)
					hash := sha256.Sum256([]byte(content))
					expectedFingerprint = hex.EncodeToString(hash[:])
				}
			}
			fingerprint, err := operationFingerprint(wrapped.Name, args, before.hash)
			if err != nil {
				return nil, execution.EffectJournalError{Err: err}
			}
			argumentsFingerprint, err := operationFingerprint(wrapped.Name, args, "")
			if err != nil {
				return nil, execution.EffectJournalError{Err: err}
			}
			operationID := identity.InvocationID + ":" + callID
			operation := execution.Operation{
				ID: operationID, EffectKey: operationID, Kind: wrapped.Name, ReplayClass: class,
				InputFingerprint: fingerprint, ArgumentsFingerprint: argumentsFingerprint,
				ObservationPath: observationPath, InputStateFingerprint: before.hash, ExpectedOutputFingerprint: expectedFingerprint,
			}
			prepared, err := lease.Store.PrepareOperation(ctx, lease.Lease, operation)
			if err != nil {
				return nil, execution.EffectJournalError{Err: err}
			}
			if prepared.Status == execution.OperationObserved || prepared.Status == execution.OperationReconciled {
				if prepared.ReplayClass == execution.ReplayFingerprintedWrite {
					current := readFileState(root, prepared.ObservationPath)
					if prepared.ObservationPath == "" || !current.exists || current.hash != prepared.OutputFingerprint || current.hash != prepared.ExpectedOutputFingerprint {
						return nil, execution.EffectJournalError{Err: execution.ErrEffectNeedsReconciliation}
					}
				}
				if len(prepared.Result) == 0 {
					return nil, execution.EffectJournalError{Err: execution.ErrEffectNeedsReconciliation}
				}
				var observed any
				if err := json.Unmarshal(prepared.Result, &observed); err != nil {
					return nil, execution.EffectJournalError{Err: fmt.Errorf("decode observed effect receipt: %w", err)}
				}
				return observed, nil
			}
			if prepared.Status != execution.OperationPrepared {
				return nil, execution.EffectJournalError{Err: execution.ErrEffectNeedsReconciliation}
			}
			if _, err := lease.Store.BeginOperation(ctx, lease.Lease, operationID); err != nil {
				return nil, execution.EffectJournalError{Err: err}
			}
			result, callErr := invokeJournaledTool(ctx, original, args)
			if callErr != nil {
				return result, execution.EffectJournalError{Err: errors.Join(callErr, execution.ErrEffectNeedsReconciliation)}
			}
			output, err := json.Marshal(result)
			if err != nil {
				return result, execution.EffectJournalError{Err: fmt.Errorf("encode tool result: %w", err)}
			}
			if len(output) > 1<<20 {
				return nil, execution.EffectJournalError{Err: fmt.Errorf("tool result exceeds durable receipt limit: %w", execution.ErrEffectNeedsReconciliation)}
			}
			outputHash := sha256.Sum256(output)
			fingerprint = hex.EncodeToString(outputHash[:])
			if class == execution.ReplayFingerprintedWrite {
				after := readFileState(root, observationPath)
				if !after.exists {
					return result, execution.EffectJournalError{Err: execution.ErrEffectNeedsReconciliation}
				}
				if after.hash != expectedFingerprint {
					return result, execution.EffectJournalError{Err: execution.ErrEffectNeedsReconciliation}
				}
				fingerprint = after.hash
			}
			if _, err := lease.Store.ObserveOperation(ctx, lease.Lease, operationID, output, fingerprint, ""); err != nil {
				return result, execution.EffectJournalError{Err: err}
			}
			return result, nil
		}
		a.Tools.Register(&wrapped)
	}
}

// safeObservationPath accepts only regular workspace-relative paths with no
// symlink traversal. An unsupported path can still be executed by the tool's
// own policy, but its effect remains unknown on recovery.
func safeObservationPath(root, path string) (string, bool) {
	if root == "" || path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", false
	}
	current := root
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return "", false
		}
	}
	return path, true
}

func invokeJournaledTool(ctx context.Context, handler tool.Handler, args map[string]any) (result any, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("effectful tool panicked: %v", recovered)
		}
	}()
	return handler(ctx, args)
}

func hasDeliveryEffect(effects []tool.Effect) bool {
	for _, effect := range effects {
		if effect != tool.EffectRead {
			return true
		}
	}
	return false
}

func operationFingerprint(name string, args map[string]any, fileHash string) (string, error) {
	data, err := json.Marshal(struct {
		Name     string         `json:"name"`
		Args     map[string]any `json:"args"`
		FileHash string         `json:"file_hash"`
	}{name, args, fileHash})
	if err != nil {
		return "", fmt.Errorf("encode operation fingerprint: %w", err)
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}
