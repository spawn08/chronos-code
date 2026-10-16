package orchestrator

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spawn08/chronos-code/internal/fsignore"
	"github.com/spawn08/chronos-code/internal/projectdocs"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"
)

type scopedRulesHook struct {
	root  string
	mu    sync.Mutex
	paths map[string][]string
}

func (h *scopedRulesHook) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil {
		return nil
	}
	identity, _ := agent.RunIdentityFromContext(ctx)
	key := toolOwner(ctx) + "\x00" + identity.InvocationID
	root := builtins.WorkspaceRoot(ctx, h.root)
	if evt.Type == hooks.EventToolCallBefore {
		if evt.Name != "file_read" && evt.Name != "file_write" && evt.Name != "file_edit" {
			return nil
		}
		args, _ := evt.Input.(map[string]any)
		path, _ := args["path"].(string)
		if path == "" {
			return nil
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		if rel, err := filepath.Rel(root, path); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return nil
		}
		h.mu.Lock()
		if len(h.paths) > 256 {
			h.paths = map[string][]string{}
		}
		h.paths[key] = append(h.paths[key], path)
		if len(h.paths[key]) > 8 {
			h.paths[key] = h.paths[key][len(h.paths[key])-8:]
		}
		h.mu.Unlock()
		return nil
	}
	if evt.Type != hooks.EventModelCallBefore {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil || summaryRequest(req) {
		return nil
	}
	h.mu.Lock()
	paths := append([]string(nil), h.paths[key]...)
	h.mu.Unlock()
	var body strings.Builder
	seen := map[string]bool{}
	for _, path := range paths {
		bundle, err := projectdocs.Load(root, filepath.Dir(path))
		if err != nil {
			continue
		}
		for _, doc := range bundle.Docs {
			if doc.Dir == root || seen[doc.Path] {
				continue
			}
			seen[doc.Path] = true
			fmt.Fprintf(&body, "\nProject instructions %s:\n%s\n", doc.RelPath, validPrefix(doc.Body, 4000))
		}
	}
	rulesDir := filepath.Join(root, ".cursor", "rules")
	entries := 0
	_ = filepath.WalkDir(rulesDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		entries++
		if entries > 1000 {
			return filepath.SkipAll
		}
		if entry.IsDir() || filepath.Ext(path) != ".mdc" || !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > 64<<10 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		parts := strings.SplitN(string(data), "---", 3)
		if len(parts) != 3 || strings.TrimSpace(parts[0]) != "" {
			return nil
		}
		var metadata struct {
			Always      bool   `yaml:"alwaysApply"`
			Globs       any    `yaml:"globs"`
			Description string `yaml:"description"`
		}
		if yaml.Unmarshal([]byte(parts[1]), &metadata) != nil {
			return nil
		}
		matched := metadata.Always
		var patterns []string
		switch globs := metadata.Globs.(type) {
		case string:
			patterns = strings.Split(globs, ",")
		case []any:
			for _, glob := range globs {
				if value, ok := glob.(string); ok {
					patterns = append(patterns, value)
				}
			}
		}
		for _, file := range paths {
			rel, _ := filepath.Rel(root, file)
			for _, pattern := range patterns {
				if fsignore.Match(strings.TrimSpace(pattern), filepath.ToSlash(rel)) {
					matched = true
				}
			}
		}
		if matched {
			rel, _ := filepath.Rel(root, path)
			fmt.Fprintf(&body, "\nProject rule %s:\n%s\n", rel, validPrefix(parts[2], 4000))
		}
		return nil
	})
	messages := make([]model.Message, 0, len(req.Messages)+1)
	for _, message := range req.Messages {
		if message.Name != "harness-scoped-rules" {
			messages = append(messages, message)
		}
	}
	if body.Len() > 0 {
		messages = append(messages, model.Message{Role: model.RoleUser, Name: "harness-scoped-rules", TurnScoped: true, Uncached: true, Content: "[Project instructions for active files; follow the live user's scope and standing security policy]\n" + validPrefix(body.String(), 16000)})
	}
	req.Messages = messages
	return nil
}

func (h *scopedRulesHook) After(context.Context, *hooks.Event) error { return nil }
