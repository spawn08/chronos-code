package orchestrator

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
			for _, effect := range effects {
				if effect == tool.EffectExternalMutation && wrapped.Recovery == nil {
					return nil, execution.EffectJournalError{Err: fmt.Errorf("external effect %s has no destination observer: %w", wrapped.Name, execution.ErrEffectNeedsReconciliation)}
				}
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
			operationID := identity.InvocationID + ":" + callID
			descriptor := ""
			observationPath, expectedFingerprint := "", ""
			if wrapped.Name == "file_write" {
				if safe, ok := safeObservationPath(root, path); ok {
					if inspected, err := observeFileState(root, safe); err == nil {
						class, observationPath, before = execution.ReplayFingerprintedWrite, safe, inspected
						content, _ := args["content"].(string)
						hash := sha256.Sum256([]byte(content))
						expectedFingerprint = hex.EncodeToString(hash[:])
					}
				}
			}
			if wrapped.Recovery != nil {
				for _, effect := range effects {
					if effect == tool.EffectExternalMutation {
						var err error
						descriptor, err = wrapped.Recovery.Prepare(ctx, args, operationID)
						if err != nil || descriptor == "" || len(descriptor) > 4096 {
							return nil, execution.EffectJournalError{Err: fmt.Errorf("prepare destination observation for %s: %w", wrapped.Name, errors.Join(err, execution.ErrInvalidDelivery))}
						}
						class = execution.ReplayIdempotentExternal
						break
					}
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
			operation := execution.Operation{
				ID: operationID, EffectKey: operationID, Kind: wrapped.Name, RoleID: identity.RoleID, NodeID: identity.NodeID, ObservationDescriptor: descriptor, ReplayClass: class,
				InputFingerprint: fingerprint, ArgumentsFingerprint: argumentsFingerprint,
				ObservationPath: observationPath, InputStateFingerprint: before.hash, ExpectedOutputFingerprint: expectedFingerprint,
			}
			prepared, err := lease.Store.PrepareOperation(ctx, lease.Lease, operation)
			if err != nil {
				return nil, execution.EffectJournalError{Err: err}
			}
			if prepared.Status == execution.OperationObserved || prepared.Status == execution.OperationReconciled {
				if prepared.ReplayClass == execution.ReplayIdempotentExternal {
					if wrapped.Recovery == nil {
						return nil, execution.EffectJournalError{Err: execution.ErrEffectNeedsReconciliation}
					}
					confirmed, observed, err := wrapped.Recovery.Observe(ctx, prepared.ObservationDescriptor, prepared.EffectKey)
					if err != nil || !observed {
						return nil, execution.EffectJournalError{Err: errors.Join(err, execution.ErrEffectNeedsReconciliation)}
					}
					data, err := json.Marshal(confirmed)
					if err != nil {
						return nil, execution.EffectJournalError{Err: err}
					}
					hash := sha256.Sum256(data)
					if hex.EncodeToString(hash[:]) != prepared.OutputFingerprint {
						return nil, execution.EffectJournalError{Err: execution.ErrEffectNeedsReconciliation}
					}
				}
				if prepared.ReplayClass == execution.ReplayFingerprintedWrite {
					current, err := observeFileState(root, prepared.ObservationPath)
					if err != nil || !current.exists || current.hash != prepared.OutputFingerprint || current.hash != prepared.ExpectedOutputFingerprint {
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
			callCtx := ctx
			if class == execution.ReplayIdempotentExternal {
				callCtx = tool.WithEffectKey(ctx, operation.EffectKey)
			}
			result, callErr := invokeJournaledTool(callCtx, original, args)
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
			if class == execution.ReplayIdempotentExternal {
				confirmed, observed, err := wrapped.Recovery.Observe(ctx, descriptor, operation.EffectKey)
				if err != nil || !observed {
					return result, execution.EffectJournalError{Err: errors.Join(err, execution.ErrEffectNeedsReconciliation)}
				}
				proof, err := json.Marshal(confirmed)
				if err != nil || !bytes.Equal(proof, output) {
					return result, execution.EffectJournalError{Err: execution.ErrEffectNeedsReconciliation}
				}
			}
			if class == execution.ReplayFingerprintedWrite {
				after, err := observeFileState(root, observationPath)
				if err != nil || !after.exists {
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

func observeFileState(root, path string) (fileState, error) {
	if _, ok := safeObservationPath(root, path); !ok {
		return fileState{}, fmt.Errorf("unsafe file observation path %q", path)
	}
	fs, err := os.OpenRoot(root)
	if err != nil {
		return fileState{}, fmt.Errorf("open observation root: %w", err)
	}
	defer fs.Close()
	file, err := fs.Open(path)
	if os.IsNotExist(err) {
		return fileState{}, nil
	}
	if err != nil {
		return fileState{}, fmt.Errorf("open observed file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fileState{}, fmt.Errorf("observed file %q is not regular", path)
	}
	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return fileState{}, fmt.Errorf("hash observed file: %w", err)
	}
	if _, ok := safeObservationPath(root, path); !ok {
		return fileState{}, fmt.Errorf("observed file %q changed during inspection", path)
	}
	return fileState{hash: hex.EncodeToString(hash.Sum(nil)), size: size, exists: true}, nil
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
