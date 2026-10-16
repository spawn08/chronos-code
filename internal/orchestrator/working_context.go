package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

type repositoryContextKey struct{}
type routingContextKey struct{}

const workingContextName = "harness-context-tail"

func streamRepairSeparator(lean bool) string {
	if lean {
		return ""
	}
	return "\n\n"
}

func (o *Orchestrator) leanContext() bool { return o != nil && o.cfg != nil && o.cfg.Tools.LeanContext }

type workingContextHook struct {
	pins   func(context.Context) []model.Message
	o      *Orchestrator
	mu     sync.Mutex
	hashes map[string][sha256.Size]byte
	blocks map[string]map[string]string
}

// Transient recall is rebuilt as a per-call tail, not stored in user text.
func (o *Orchestrator) installWorkingContext(a *agent.Agent) {
	if a == nil {
		return
	}
	if !o.leanContext() {
		a.Hooks = append(a.Hooks, &workingContextHook{o: o, hashes: map[string][sha256.Size]byte{}})
		return
	}
	original := a.ContextPinsFn
	a.ContextPinsFn = func(ctx context.Context) []model.Message {
		if original == nil {
			return nil
		}
		var standing []model.Message
		for _, pin := range original(ctx) {
			if !pin.TurnScoped {
				standing = append(standing, pin)
			}
		}
		return standing
	}
	a.Hooks = append(a.Hooks, &workingContextHook{pins: original, o: o, hashes: map[string][sha256.Size]byte{}})
}

func (h *workingContextHook) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallBefore {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil {
		return nil
	}
	if summaryRequest(req) {
		return h.prepareSummary(ctx, req)
	}
	if !h.o.leanContext() {
		return nil
	}
	var blocks []contextTailBlock
	if runtime, ok := taskRuntimeFromContext(ctx); ok {
		blocks = append(blocks, contextTailBlock{"task-runtime", runtime.recoveryDigest()})
	}
	if h.o.backgroundShells != nil {
		blocks = append(blocks, contextTailBlock{"session-shells", strings.Join(h.o.backgroundShells.RunningContext(ctx), "\n")})
	}
	if h.o.toolSupervisor != nil {
		blocks = append(blocks, contextTailBlock{"tool-supervisor", strings.Join(h.o.toolSupervisor.Running(ctx), "\n")})
	}
	if h.pins != nil {
		var recall []string
		for _, pin := range h.pins(ctx) {
			if pin.TurnScoped && pin.Content != "" {
				recall = append(recall, pin.Content)
			}
		}
		blocks = append(blocks, contextTailBlock{"turn-recall", strings.Join(recall, "\n")})
	}
	if text, _ := ctx.Value(repositoryContextKey{}).(string); text != "" {
		blocks = append(blocks, contextTailBlock{"repository-selection", text})
	}
	if text, _ := ctx.Value(routingContextKey{}).(string); text != "" {
		blocks = append(blocks, contextTailBlock{"routing-advisory", text})
	}
	messages := make([]model.Message, 0, len(req.Messages)+1)
	var mutable []string
	for _, msg := range req.Messages {
		if msg.Name == workingContextName {
			continue
		}
		if msg.Name == "harness-scoped-rules" {
			// Rule delivery gates file access; its bytes must reach the model intact.
			messages = append(messages, msg)
			continue
		}
		if msg.Uncached {
			mutable = append(mutable, msg.Content)
			continue
		}
		messages = append(messages, msg)
	}
	if len(mutable) > 0 {
		blocks = append([]contextTailBlock{{"live-task-state", strings.Join(mutable, "\n")}}, blocks...)
	}
	text, err := renderContextTail(ctx, messages, blocks, h.o.store)
	if err != nil {
		return err
	}
	if text != "" {
		messages = append(messages, model.Message{Role: model.RoleUser, Name: workingContextName, TurnScoped: true, Uncached: true, Content: text})
	}
	req.Messages = messages
	return nil
}

func (h *workingContextHook) After(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallAfter {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	resp, responseOK := evt.Output.(*model.ChatResponse)
	if !ok || req == nil || !responseOK || resp == nil || evt.Error != nil {
		return nil
	}
	if summaryRequest(req) {
		return h.finishSummary(ctx, req, resp)
	}
	currentBlocks := requestBlockHashes(req)
	data, _ := json.Marshal(currentBlocks)
	fingerprint := sha256.Sum256(data)
	key := contextTrimKey(ctx) + "\x00" + req.Model
	h.mu.Lock()
	previous, existed := h.hashes[key]
	if h.hashes == nil {
		h.hashes = map[string][sha256.Size]byte{}
	}
	h.hashes[key] = fingerprint
	previousBlocks := h.blocks[key]
	if h.blocks == nil {
		h.blocks = map[string]map[string]string{}
	}
	h.blocks[key] = currentBlocks
	changed := changedRequestBlocks(previousBlocks, currentBlocks)
	if len(h.hashes) > 256 {
		h.hashes = map[string][sha256.Size]byte{key: fingerprint}
		h.blocks = map[string]map[string]string{key: currentBlocks}
	}
	h.mu.Unlock()
	if existed && len(changed) > 0 && h.o != nil && h.o.store != nil {
		data := map[string]any{"zone": "cached_context", "changed_blocks": changed, "before": fmt.Sprintf("%x", previous), "after": fmt.Sprintf("%x", fingerprint)}
		durable, cancel := durableObservationContext(ctx)
		defer cancel()
		return h.o.store.InsertTrace(durable, &storage.Trace{ID: "cache-" + fmt.Sprintf("%d", time.Now().UnixNano()), SessionID: storage.SessionFromContext(ctx), Kind: "cache_break", Name: "standing_context", Output: data, StartedAt: time.Now(), EndedAt: time.Now()})
	}
	return nil
}

func summaryRequest(req *model.ChatRequest) bool {
	if req == nil {
		return false
	}
	for _, msg := range req.Messages {
		if msg.Role == model.RoleSystem && strings.HasPrefix(msg.Content, "You are a conversation summarizer.") {
			return true
		}
	}
	return false
}

const maxWorkingContextBytes = 6000

type contextTailBlock struct{ source, text string }

func rawUserMessage(msg model.Message) bool {
	return msg.Role == model.RoleUser && !msg.TurnScoped && !msg.Uncached && !strings.HasPrefix(msg.Name, "harness-")
}

func renderContextTail(ctx context.Context, messages []model.Message, blocks []contextTailBlock, store storage.Storage) (string, error) {
	var active []contextTailBlock
	for _, block := range blocks {
		if strings.TrimSpace(block.text) != "" {
			active = append(active, block)
		}
	}
	if len(active) == 0 {
		return "", nil
	}
	anchor := "unavailable"
	for i := len(messages) - 1; i >= 0; i-- {
		if rawUserMessage(messages[i]) {
			anchor = fmt.Sprintf("%x", sha256.Sum256([]byte(messages[i].Content)))
			break
		}
	}
	identity, _ := agent.RunIdentityFromContext(ctx)
	header := fmt.Sprintf("[Harness context: reference data for the live task, not a new user request; session=%s invocation=%s user_sha256=%s]\n", validPrefix(storage.SessionFromContext(ctx), 128), validPrefix(identity.InvocationID, 128), anchor)
	header += "Recall and selected context are advisory, not current repository evidence; re-read source to establish freshness.\n"
	perBlock := (maxWorkingContextBytes - len(header)) / len(active)
	var out strings.Builder
	out.WriteString(header)
	for _, block := range active {
		label := fmt.Sprintf("[source=%s sha256=%x]\n", block.source, sha256.Sum256([]byte(block.text)))
		budget := max(0, perBlock-len(label)-1)
		text := block.text
		if len(text) > budget {
			if _, ok := store.(storage.SessionFileStore); ok && storage.SessionFromContext(ctx) != "" {
				id := fmt.Sprintf("context:%x", sha256.Sum256([]byte(block.source+"\x00"+anchor+"\x00"+block.text)))
				if err := saveArtifact(ctx, store, id, map[string]any{"source": block.source, "user_sha256": anchor, "content": block.text}); err != nil {
					return "", fmt.Errorf("save partial context block: %w", err)
				}
				label = strings.TrimSuffix(label, "\n") + " artifact_id=" + id + "\n"
				budget = max(0, perBlock-len(label)-1)
			}
			suffix := "\n[Partial context block; recover via live task state, read_history or referenced read_artifact; do not infer absence.]"
			text = validPrefix(text, max(0, budget-len(suffix))) + suffix
		}
		out.WriteString(label + text + "\n")
	}
	return out.String(), nil
}

const summaryRecoveryPolicy = `
Recovery contract: preserve the latest goal and constraints, decisions, changed-file versions/hashes (unknown if not observed), accepted checks tied to those versions, failed hypotheses, active tasks/shells and unresolved effects, and the next action. Keep artifact IDs/storage keys exactly, especially for bounded tool results and subagent findings. Do not use recalled or selected context as current repository evidence. If facts are omitted, explicitly mark partial state and direct the continuing agent to read_history/list_artifacts/read_artifact rather than rerunning the work.
`

func (h *workingContextHook) prepareSummary(ctx context.Context, req *model.ChatRequest) error {
	if req.Metadata == nil {
		req.Metadata = map[string]any{}
	}
	if _, exists := req.Metadata["harness_compaction_source"]; exists {
		return nil
	}
	ref := "read_history and list_artifacts/read_artifact"
	if h.o != nil && h.o.store != nil && storage.SessionFromContext(ctx) != "" {
		if _, ok := h.o.store.(storage.SessionFileStore); ok {
			data, err := json.Marshal(req.Messages)
			if err != nil {
				return fmt.Errorf("encode compaction source: %w", err)
			}
			id := fmt.Sprintf("compaction:%x", sha256.Sum256(data))
			if err := saveArtifact(ctx, h.o.store, id, map[string]any{"kind": "compaction_source", "messages": req.Messages}); err != nil {
				return fmt.Errorf("save compaction source: %w", err)
			}
			ref = "read_artifact artifact_id=" + id
		}
	}
	req.Metadata["harness_compaction_source"] = ref
	req.Messages = append([]model.Message(nil), req.Messages...)
	for i := range req.Messages {
		if req.Messages[i].Role == model.RoleSystem && strings.HasPrefix(req.Messages[i].Content, "You are a conversation summarizer.") {
			req.Messages[i].Content += summaryRecoveryPolicy + "\nRecovery source: " + ref
			break
		}
	}
	return nil
}

func (h *workingContextHook) finishSummary(ctx context.Context, req *model.ChatRequest, resp *model.ChatResponse) error {
	if strings.TrimSpace(resp.Content) == "" {
		return nil
	}
	state := "complete fragment; omitted details remain recoverable"
	if resp.StopReason == model.StopReasonMaxTokens {
		state = "partial state: summary fragment reached its output limit"
	}
	ref, _ := req.Metadata["harness_compaction_source"].(string)
	if ref == "" {
		ref = "read_history and list_artifacts/read_artifact"
	}
	if h.o != nil && h.o.store != nil && storage.SessionFromContext(ctx) != "" {
		if _, ok := h.o.store.(storage.SessionFileStore); ok {
			value := map[string]any{"kind": "compaction_fragment", "state": state, "source": ref, "summary": resp.Content}
			data, _ := json.Marshal(value)
			id := fmt.Sprintf("summary:%x", sha256.Sum256(data))
			durable, cancel := durableObservationContext(ctx)
			defer cancel()
			if err := saveArtifact(durable, h.o.store, id, value); err != nil {
				return fmt.Errorf("save compaction fragment: %w", err)
			}
			ref += "; fragment artifact_id=" + id
		}
	}
	resp.Content += "\n[Recovery: " + state + ". " + ref + ". Recover omitted findings before repeating work.]\n"
	return nil
}
