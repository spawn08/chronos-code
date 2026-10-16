package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
)

const artifactPrefix = "harness/artifacts/"

func artifactPath(id string) (string, error) {
	if len(id) == 0 || len(id) > 128 || strings.ContainsAny(id, "/\\.\x00") {
		return "", fmt.Errorf("invalid artifact ID")
	}
	return artifactPrefix + id, nil
}

func saveArtifact(ctx context.Context, store storage.Storage, id string, value any) error {
	files, ok := store.(storage.SessionFileStore)
	if !ok || storage.SessionFromContext(ctx) == "" {
		return fmt.Errorf("session artifact storage unavailable")
	}
	path, err := artifactPath(id)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode artifact: %w", err)
	}
	return files.PutFile(ctx, storage.SessionFromContext(ctx), path, data)
}

// installArtifacts exposes the same bounded reader for tool results, shell
// logs and persisted history, independent of the location of the data home.
func (o *Orchestrator) installArtifacts(a *agent.Agent) {
	if a.Storage == nil || a.Tools == nil {
		return
	}
	read := &tool.Definition{
		Name: "read_artifact", Permission: tool.PermAllow, ParallelSafe: true, Effects: []tool.Effect{tool.EffectRead},
		Description: "Read saved tool output or a shell log in this session. Use artifact_id from the result; shell:<shell_id> reads the full durable log, even after exit or restart. offset is 1-based (negative reads from the end); limit defaults to 200 lines. Output is bounded; continue at next_offset when needed.",
		Parameters: map[string]any{"type": "object", "properties": map[string]any{
			"artifact_id": map[string]any{"type": "string"}, "offset": map[string]any{"type": "integer"}, "limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 400},
			"byte_offset": map[string]any{"type": "integer", "description": "Byte range instead of lines; negative offsets read from EOF. Use for very large logs or JSON lines."}, "max_bytes": map[string]any{"type": "integer", "minimum": 1, "maximum": 32768},
		}, "required": []string{"artifact_id"}},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			id, _ := args["artifact_id"].(string)
			var data []byte
			var err error
			byteOffset, byteMode := intValue(args["byte_offset"])
			maxBytes := 32768
			if n, ok := intValue(args["max_bytes"]); ok {
				maxBytes = n
			}
			if maxBytes < 1 || maxBytes > 32768 {
				return nil, fmt.Errorf("invalid max_bytes")
			}
			if byteMode && strings.HasPrefix(id, "shell:") && o.backgroundShells != nil {
				chunk, offset, err := o.backgroundShells.ReadLogChunk(ctx, strings.TrimPrefix(id, "shell:"), int64(byteOffset), maxBytes)
				if err != nil {
					return nil, err
				}
				return artifactByteResult(id, string(chunk), int(offset), len(chunk) == maxBytes), nil
			}
			if strings.HasPrefix(id, "shell:") && o.backgroundShells != nil {
				data, err = o.backgroundShells.ReadLog(ctx, strings.TrimPrefix(id, "shell:"))
			} else if files, ok := a.Storage.(storage.SessionFileStore); ok {
				path, pathErr := artifactPath(id)
				if pathErr != nil {
					return nil, pathErr
				}
				data, err = files.GetFile(ctx, storage.SessionFromContext(ctx), path)
			} else {
				return nil, fmt.Errorf("artifact storage unavailable")
			}
			if err != nil {
				return nil, fmt.Errorf("read_artifact: %w", err)
			}
			if byteMode {
				if byteOffset < 0 {
					byteOffset = max(0, len(data)+byteOffset)
				}
				if byteOffset < 0 || byteOffset > len(data) {
					return nil, fmt.Errorf("offset outside artifact")
				}
				if byteOffset < len(data) && !utf8.RuneStart(data[byteOffset]) {
					return nil, fmt.Errorf("byte_offset must be a UTF-8 character boundary")
				}
				end := min(len(data), byteOffset+maxBytes)
				for end > byteOffset && end < len(data) && !utf8.RuneStart(data[end]) {
					end--
				}
				if end == byteOffset && byteOffset < len(data) {
					return nil, fmt.Errorf("max_bytes must fit the next UTF-8 character")
				}
				return artifactByteResult(id, string(data[byteOffset:end]), byteOffset, end < len(data)), nil
			}
			result, err := artifactLines(string(data), args)
			if err != nil {
				return nil, err
			}
			result["artifact_id"] = id
			return result, nil
		},
	}
	a.Tools.Register(read)
	a.Tools.Register(&tool.Definition{
		Name: "list_artifacts", Permission: tool.PermAllow, ParallelSafe: true, Effects: []tool.Effect{tool.EffectRead},
		Description: "List the newest saved tool results for this session. Use read_artifact to recover findings after compaction. Shell logs are referenced by shell:<shell_id> from shell results.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Handler: func(ctx context.Context, _ map[string]any) (any, error) {
			files, ok := a.Storage.(storage.SessionFileStore)
			if !ok {
				return nil, fmt.Errorf("artifact storage unavailable")
			}
			infos, err := files.ListFiles(ctx, storage.SessionFromContext(ctx), artifactPrefix)
			if err != nil {
				return nil, err
			}
			for i := range infos {
				infos[i].Path = strings.TrimPrefix(infos[i].Path, artifactPrefix)
			}
			if len(infos) > 100 {
				infos = infos[len(infos)-100:]
			}
			return infos, nil
		},
	})
	a.Tools.Register(&tool.Definition{
		Name: "read_history", Permission: tool.PermAllow, ParallelSafe: true, Effects: []tool.Effect{tool.EffectRead},
		Description: "Recover persisted conversation and tool findings after compaction. Reads this session by default; an explicit prior session_id must belong to this project's storage and tenant. after_seq selects subsequent events; returns at most 20 messages and 32 KiB with next_seq.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"after_seq": map[string]any{"type": "integer", "minimum": 0}, "session_id": map[string]any{"type": "string"}, "query": map[string]any{"type": "string"}}},
		Handler: func(ctx context.Context, args map[string]any) (any, error) {
			sid, _ := args["session_id"].(string)
			if sid == "" {
				sid = storage.SessionFromContext(ctx)
			}
			if _, err := a.Storage.GetSession(ctx, sid); err != nil {
				return nil, fmt.Errorf("history session unavailable: %w", err)
			}
			after, _ := intValue(args["after_seq"])
			events, err := a.Storage.ListEvents(ctx, sid, int64(after))
			if err != nil {
				return nil, err
			}
			query, _ := args["query"].(string)
			var entries []map[string]any
			bytes := 0
			next := int64(after)
			for _, event := range events {
				if event.Type != "chat_message" {
					next = event.SeqNum
					continue
				}
				data, err := json.Marshal(event.Payload)
				if err != nil {
					return nil, err
				}
				if query != "" && !strings.Contains(strings.ToLower(string(data)), strings.ToLower(query)) {
					next = event.SeqNum
					continue
				}
				text := string(data)
				if len(text) > 16000 {
					text = validPrefix(text, 16000) + " [bounded; use referenced artifacts for full output]"
				}
				if len(entries) >= 20 || bytes+len(text) > 32<<10 {
					break
				}
				entries = append(entries, map[string]any{"seq": event.SeqNum, "message": text})
				bytes += len(text)
				next = event.SeqNum
			}
			return map[string]any{"session_id": sid, "messages": entries, "next_seq": next}, nil
		},
	})
	prev := a.ContextPinsFn
	a.ContextPinsFn = func(ctx context.Context) []model.Message {
		var pins []model.Message
		if prev != nil {
			pins = prev(ctx)
		}
		return append(pins, model.Message{Role: model.RoleSystem, Content: "Saved findings remain available after compaction: use read_history or list_artifacts/read_artifact before repeating an investigation. Shell logs use artifact_id shell:<shell_id>; an unchanged file still needs a source read if its contents are no longer in your context."})
	}
}

func artifactLines(text string, args map[string]any) (map[string]any, error) {
	lines := strings.Split(text, "\n")
	offset := 1
	if n, ok := intValue(args["offset"]); ok {
		offset = n
	}
	if offset < 0 {
		offset = max(1, len(lines)+offset+1)
	}
	if offset < 1 || offset > len(lines) {
		return nil, fmt.Errorf("offset outside artifact (%d lines)", len(lines))
	}
	limit := 200
	if n, ok := intValue(args["limit"]); ok {
		limit = n
	}
	if limit < 1 || limit > 400 {
		return nil, fmt.Errorf("limit must be between 1 and 400")
	}
	end := min(len(lines), offset-1+limit)
	content := strings.Join(lines[offset-1:end], "\n")
	for len(content) > 32<<10 && end > offset {
		end--
		content = strings.Join(lines[offset-1:end], "\n")
	}
	if len(content) > 32<<10 {
		return nil, fmt.Errorf("artifact line exceeds 32 KiB; use byte_offset and max_bytes to recover a bounded chunk")
	}
	return map[string]any{"content": content, "offset": offset, "next_offset": end + 1, "total_lines": len(lines), "truncated": end < len(lines)}, nil
}

func artifactByteResult(id, content string, offset int, truncated bool) map[string]any {
	originalSize := len(content)
	content = validPrefix(content, len(content))
	for {
		result := map[string]any{"artifact_id": id, "content": content, "byte_offset": offset, "next_byte_offset": offset + len(content), "truncated": truncated || len(content) < originalSize}
		encoded, err := json.Marshal(result)
		if err == nil && len(encoded) <= maxToolResultBytes {
			return result
		}
		content = validPrefix(content, len(content)/2)
	}
}

func validPrefix(s string, n int) string {
	n = min(n, len(s))
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func wrapArtifactResults(a *agent.Agent) {
	if _, ok := a.Storage.(storage.SessionFileStore); !ok {
		return
	}
	for _, def := range a.Tools.List() {
		if def.Handler == nil || strings.Contains(def.Name, "artifact") || def.Name == "read_history" || def.Name == "read_stored_result" || def.Name == "call_tool" {
			continue
		}
		wrapped := *def
		original := def.Handler
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			result, err := original(ctx, args)
			if err != nil || storage.SessionFromContext(ctx) == "" {
				return result, err
			}
			data, encodeErr := json.Marshal(result)
			if encodeErr != nil || len(data) < 4000 {
				return result, nil
			}
			id := "tool:" + rand.Text()
			if err := saveArtifact(ctx, a.Storage, id, result); err != nil {
				return result, fmt.Errorf("persist tool artifact: %w", err)
			}
			if values, ok := result.(map[string]any); ok {
				copy := make(map[string]any, len(values)+1)
				for key, value := range values {
					copy[key] = value
				}
				copy["artifact_id"] = id
				return copy, nil
			}
			return map[string]any{"content": result, "artifact_id": id}, nil
		}
		a.Tools.Register(&wrapped)
	}
}
