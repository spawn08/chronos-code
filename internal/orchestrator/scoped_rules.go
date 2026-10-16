package orchestrator

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/fsignore"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/engine/tool/builtins"
	"github.com/spawn08/chronos/sdk/agent"
	"gopkg.in/yaml.v3"
)

const (
	maxScopedRulesStates     = 256
	maxScopedRulesPaths      = 256
	maxScopedRulesSources    = 256
	maxScopedRulesFileBytes  = 1 << 20
	maxScopedRulesTotalBytes = 1 << 20
	maxScopedRulesDepth      = 64
	maxScopedRulesPathBytes  = 4096
	maxScopedRulesEntries    = 1000
)

var (
	errScopedInstructionsNeeded = errors.New("scoped_instructions_needed")
	errScopedRulesIgnored       = errors.New("scoped rules: ignored path")
)

// IsScopedInstructionsNeeded recognizes the typed deferral and serialized hook errors.
// It only classifies presentation; callers still preserve the tool result and access gate.
func IsScopedInstructionsNeeded(value any) bool {
	if err, ok := value.(error); ok {
		return errors.Is(err, errScopedInstructionsNeeded) || strings.Contains(err.Error(), errScopedInstructionsNeeded.Error()+":")
	}
	text, ok := value.(string)
	return ok && strings.Contains(text, errScopedInstructionsNeeded.Error()+":")
}

// Source bodies are reloaded, not retained across requests. Limits fail closed.
type scopedRulesState struct {
	paths       map[string]bool
	delivered   map[string][sha256.Size]byte
	offered     map[string][sha256.Size]byte
	offeredBody string
}

type scopedRulesHook struct {
	root string
	mu   sync.Mutex
	// Kept for existing hook construction; state uses host-issued identities.
	paths  map[string][]string
	states map[string]*scopedRulesState
	// Immutable hashes of root source bytes actually pinned by startup.
	startupHashes map[string][sha256.Size]byte
}

type scopedRule struct {
	path     string
	body     string
	hash     [sha256.Size]byte
	depth    int
	priority int
}

func (h *scopedRulesHook) Before(ctx context.Context, evt *hooks.Event) error {
	if evt == nil {
		return nil
	}
	toolCall := evt.Type == hooks.EventToolCallBefore && (evt.Name == "file_read" || evt.Name == "file_write" || evt.Name == "file_edit")
	var req *model.ChatRequest
	if evt.Type == hooks.EventModelCallBefore {
		req, _ = evt.Input.(*model.ChatRequest)
		if req == nil || summaryRequest(req) {
			return nil
		}
	} else if !toolCall {
		return nil
	}
	root, err := filepath.Abs(builtins.WorkspaceRoot(ctx, h.root))
	if err != nil {
		return fmt.Errorf("scoped rules workspace: %w", err)
	}
	lexicalRoot := root
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("scoped rules workspace: %w", err)
	}
	key, err := scopedRulesKey(ctx, root)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.states[key]
	var path string
	if toolCall {
		args, _ := evt.Input.(map[string]any)
		raw, _ := args["path"].(string)
		if raw == "" {
			return fmt.Errorf("scoped rules: a nonempty file path is required")
		}
		if filepath.IsAbs(raw) && lexicalRoot != root {
			if relative, relErr := filepath.Rel(lexicalRoot, raw); relErr == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				raw = filepath.Join(root, relative)
			}
		}
		path, err = scopedRulesSafePath(root, raw)
		if err != nil {
			return err
		}
	}
	if state == nil {
		if len(h.states) >= maxScopedRulesStates {
			return fmt.Errorf("scoped rules state limit (%d); finish and release an invocation before starting another", maxScopedRulesStates)
		}
		state = &scopedRulesState{paths: map[string]bool{}, delivered: map[string][sha256.Size]byte{}}
		if h.states == nil {
			h.states = map[string]*scopedRulesState{}
		}
		if len(h.startupHashes) > maxScopedRulesSources {
			return fmt.Errorf("scoped rules startup source count limit (%d)", maxScopedRulesSources)
		}
		for source, hash := range h.startupHashes {
			state.delivered[source] = hash
		}
		h.states[key] = state
	}
	if toolCall && !state.paths[path] && len(state.paths) >= maxScopedRulesPaths {
		return fmt.Errorf("scoped rules active path limit (%d); no scopes were evicted", maxScopedRulesPaths)
	}
	paths := make([]string, 0, len(state.paths)+1)
	for active := range state.paths {
		paths = append(paths, active)
	}
	if toolCall && !state.paths[path] {
		paths = append(paths, path)
	}
	rules, err := scopedRulesLoad(root, paths)
	if err != nil {
		return err
	}
	if toolCall {
		state.paths[path] = true
		var needed []string
		for _, rule := range rules {
			if hash, ok := state.delivered[rule.path]; !ok || hash != rule.hash {
				needed = append(needed, rule.path)
			}
		}
		if len(needed) > 0 {
			return fmt.Errorf("%w: file access is deferred. The next model call loads the pending project instructions automatically; retry this call after that response. Pending sources: %s", errScopedInstructionsNeeded, strings.Join(needed, ", "))
		}
		return nil
	}
	var body strings.Builder
	if len(rules) > 0 {
		body.WriteString("[Project instructions for active files; follow the live user's scope and standing security policy. Root-to-leaf order; deeper directory instructions override ancestors. Cursor metadata is preserved verbatim.]\n")
		for _, rule := range rules {
			rel, _ := filepath.Rel(root, rule.path)
			fmt.Fprintf(&body, "\nProject instructions %q (sha256=%x):\n%s\n", filepath.ToSlash(rel), rule.hash, rule.body)
		}
	}
	messages := make([]model.Message, 0, len(req.Messages)+1)
	for _, message := range req.Messages {
		if message.Name != "harness-scoped-rules" {
			messages = append(messages, message)
		}
	}
	if body.Len() > 0 {
		messages = append(messages, model.Message{Role: model.RoleUser, Name: "harness-scoped-rules", TurnScoped: true, Uncached: true, Content: body.String()})
	}
	req.Messages = messages
	state.offered = map[string][sha256.Size]byte{}
	state.offeredBody = body.String()
	for _, rule := range rules {
		state.offered[rule.path] = rule.hash
	}
	return nil
}

// release must run after the invocation's final tool/model event.
func (h *scopedRulesHook) release(ctx context.Context) {
	root, err := filepath.Abs(builtins.WorkspaceRoot(ctx, h.root))
	if err != nil {
		return
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return
	}
	key, err := scopedRulesKey(ctx, root)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.states, key)
}

func scopedRulesKey(ctx context.Context, root string) (string, error) {
	identity, ok := agent.RunIdentityFromContext(ctx)
	if !ok || identity.InvocationID == "" {
		return "", fmt.Errorf("scoped rules: host-issued invocation identity required; anonymous scope is refused")
	}
	fields := []string{toolOwner(ctx), identity.TenantID, identity.RepositoryID, identity.SessionID, identity.RoleID, identity.InvocationID, root}
	var key strings.Builder
	for _, field := range fields {
		if len(field) > maxScopedRulesPathBytes {
			return "", fmt.Errorf("scoped rules identity length limit (%d)", maxScopedRulesPathBytes)
		}
		fmt.Fprintf(&key, "%q", field)
	}
	return key.String(), nil
}

func scopedRulesSafePath(root, raw string) (string, error) {
	if len(raw) > maxScopedRulesPathBytes || strings.ContainsRune(raw, 0) {
		return "", fmt.Errorf("scoped rules path length/encoding limit (%d)", maxScopedRulesPathBytes)
	}
	for _, part := range strings.Split(raw, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("scoped rules: ambiguous parent traversal %q refused", raw)
		}
	}
	path := raw
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("scoped rules: outside-workspace path %q refused", raw)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) > maxScopedRulesDepth {
		return "", fmt.Errorf("scoped rules directory depth limit (%d): %q", maxScopedRulesDepth, raw)
	}
	current := root
	for _, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("scoped rules inspect %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("scoped rules: ambiguous symlink path %q refused", current)
		}
	}
	return path, nil
}

func scopedRulesCheckIgnored(root, path string) error {
	// Bound and validate ignore sources before using the repository matcher.
	dirs := []string{root}
	rel, _ := filepath.Rel(root, filepath.Dir(path))
	current := root
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			current = filepath.Join(current, part)
			dirs = append(dirs, current)
		}
	}
	for _, dir := range dirs {
		if _, err := scopedRulesRead(root, filepath.Join(dir, ".gitignore"), 64<<10); err != nil {
			return err
		}
	}
	matcher := fsignore.New(root)
	for _, dir := range dirs[1:] {
		if matcher.Ignored(dir, true) {
			return fmt.Errorf("%w %q refused", errScopedRulesIgnored, path)
		}
	}
	info, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("scoped rules inspect %q: %w", path, err)
	}
	if matcher.Ignored(path, info != nil && info.IsDir()) {
		return fmt.Errorf("%w %q refused", errScopedRulesIgnored, path)
	}
	return nil
}

func scopedRulesRead(root, path string, limit int) ([]byte, error) {
	if _, err := scopedRulesSafePath(root, path); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scoped rules inspect %q: %w", path, err)
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("scoped rules: nonregular source %q refused", path)
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scoped rules read %q: %w", path, err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("scoped rules stat %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || !os.SameFile(before, info) {
		return nil, fmt.Errorf("scoped rules: nonregular or changed source %q refused", path)
	}
	if info.Size() > int64(limit) {
		return nil, fmt.Errorf("scoped rules source byte limit (%d) exceeded by %q; source is not truncated, reduce or split the instruction file", limit, path)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("scoped rules read %q: %w", path, err)
	}
	if len(data) > limit {
		return nil, fmt.Errorf("scoped rules source byte limit (%d) exceeded by %q", limit, path)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("scoped rules: invalid UTF-8 source %q refused", path)
	}
	return data, nil
}

func scopedRulesLoad(root string, paths []string) ([]scopedRule, error) {
	// Candidate order follows projectdocs, including conventional case variants.
	candidates := []string{"AGENTS.md", "Agents.md", "CLAUDE.md", "Claude.md", "AGENT.md", "Agent.md", ".cursorrules", filepath.Join(".github", "copilot-instructions.md")}
	dirs := map[string]int{root: 0}
	for _, path := range paths {
		if _, err := scopedRulesSafePath(root, path); err != nil {
			return nil, err
		}
		dir := filepath.Dir(path)
		if path == root {
			dir = root
		}
		for dir != root {
			rel, _ := filepath.Rel(root, dir)
			dirs[dir] = len(strings.Split(rel, string(filepath.Separator)))
			dir = filepath.Dir(dir)
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if dirs[ordered[i]] != dirs[ordered[j]] {
			return dirs[ordered[i]] < dirs[ordered[j]]
		}
		return ordered[i] < ordered[j]
	})
	rules := make([]scopedRule, 0)
	total := 0
	add := func(path string, data []byte, depth, priority int) error {
		if data == nil {
			return nil
		}
		if len(rules) >= maxScopedRulesSources {
			return fmt.Errorf("scoped rules source count limit (%d) at %q; no instructions were omitted", maxScopedRulesSources, path)
		}
		total += len(data)
		if total > maxScopedRulesTotalBytes {
			return fmt.Errorf("scoped rules total byte limit (%d) at %q; request refused rather than truncating instructions", maxScopedRulesTotalBytes, path)
		}
		rules = append(rules, scopedRule{path: path, body: string(data), hash: sha256.Sum256(data), depth: depth, priority: priority})
		return nil
	}
	for _, dir := range ordered {
		var seen []os.FileInfo
		for priority, name := range candidates {
			path := filepath.Join(dir, name)
			data, err := scopedRulesRead(root, path, maxScopedRulesFileBytes)
			if err != nil {
				return nil, err
			}
			if data == nil {
				continue
			}
			info, err := os.Stat(path)
			if err != nil {
				return nil, fmt.Errorf("scoped rules inspect %q: %w", path, err)
			}
			duplicate := false
			for _, previous := range seen {
				if os.SameFile(previous, info) {
					duplicate = true
					break
				}
			}
			if duplicate {
				continue
			}
			seen = append(seen, info)
			if err := add(path, data, dirs[dir], priority); err != nil {
				return nil, err
			}
		}
	}
	rulesDir := filepath.Join(root, ".cursor", "rules")
	if _, err := scopedRulesSafePath(root, rulesDir); err != nil {
		return nil, err
	}
	entries := 0
	err := scopedRulesWalk(rulesDir, 0, func(path string, entry fs.DirEntry, walkErr error) error {
		if os.IsNotExist(walkErr) {
			return nil
		}
		if walkErr != nil {
			return fmt.Errorf("scoped rules scan %q: %w", path, walkErr)
		}
		entries++
		if entries > maxScopedRulesEntries {
			return fmt.Errorf("scoped rules scan entry limit (%d); scan incomplete", maxScopedRulesEntries)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("scoped rules: symlink rule source %q refused", path)
		}
		if entry.IsDir() {
			if fsignore.SkipDirName(entry.Name()) {
				return filepath.SkipDir
			}
			if _, err := scopedRulesSafePath(root, path); err != nil {
				return err
			}
			if err := scopedRulesCheckIgnored(root, path); err != nil {
				if errors.Is(err, errScopedRulesIgnored) {
					return filepath.SkipDir
				}
				return err
			}
			return nil
		}
		if filepath.Ext(path) != ".mdc" {
			return nil
		}
		if err := scopedRulesCheckIgnored(root, path); err != nil {
			if errors.Is(err, errScopedRulesIgnored) {
				return nil
			}
			return err
		}
		data, err := scopedRulesRead(root, path, maxScopedRulesFileBytes)
		if err != nil {
			return err
		}
		matched, err := scopedRulesMDC(data, root, paths)
		if err != nil {
			return fmt.Errorf("scoped rules metadata %q: %w", path, err)
		}
		if matched {
			return add(path, data, 0, len(candidates))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].depth != rules[j].depth {
			return rules[i].depth < rules[j].depth
		}
		if rules[i].priority != rules[j].priority {
			return rules[i].priority < rules[j].priority
		}
		return rules[i].path < rules[j].path
	})
	return rules, nil
}

// ReadDir batches bound memory even when a single directory exceeds the scan limit.
func scopedRulesWalk(path string, depth int, visit fs.WalkDirFunc) error {
	if depth > maxScopedRulesDepth {
		return fmt.Errorf("scoped rules scan depth limit (%d)", maxScopedRulesDepth)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return visit(path, nil, err)
	}
	if err := visit(path, fs.FileInfoToDirEntry(info), nil); err != nil {
		if errors.Is(err, filepath.SkipDir) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("scoped rules scan %q: %w", path, err)
	}
	defer dir.Close()
	for {
		entries, err := dir.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("scoped rules scan %q: %w", path, err)
		}
		for _, entry := range entries {
			if err := scopedRulesWalk(filepath.Join(path, entry.Name()), depth+1, visit); err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}

func scopedRulesMDC(data []byte, root string, paths []string) (bool, error) {
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if len(lines) < 3 || strings.TrimSpace(lines[0]) != "---" {
		return false, fmt.Errorf("missing YAML frontmatter")
	}
	end := 1
	for end < len(lines) && strings.TrimSpace(lines[end]) != "---" {
		end++
	}
	if end == len(lines) {
		return false, fmt.Errorf("unterminated YAML frontmatter")
	}
	var metadata struct {
		Always      bool   `yaml:"alwaysApply"`
		Globs       any    `yaml:"globs"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal([]byte(strings.Join(lines[1:end], "\n")), &metadata); err != nil {
		return false, fmt.Errorf("parse frontmatter: %w", err)
	}
	patterns := []string{}
	switch globs := metadata.Globs.(type) {
	case nil:
	case string:
		start, depth := 0, 0
		for i, char := range globs {
			switch char {
			case '{':
				depth++
			case '}':
				depth--
			case ',':
				if depth == 0 {
					patterns = append(patterns, globs[start:i])
					start = i + 1
				}
			}
		}
		patterns = append(patterns, globs[start:])
	case []any:
		for _, glob := range globs {
			value, ok := glob.(string)
			if !ok {
				return false, fmt.Errorf("globs must contain strings")
			}
			patterns = append(patterns, value)
		}
	default:
		return false, fmt.Errorf("globs must be a string or list")
	}
	if len(patterns) > 64 {
		return false, fmt.Errorf("glob count limit (64)")
	}
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if len(pattern) > 256 || strings.Count(pattern, "{") > 4 || strings.Count(pattern, ",") > 16 || strings.Count(pattern, "**") > 2 {
			return false, fmt.Errorf("glob complexity limit")
		}
		for _, path := range paths {
			rel, _ := filepath.Rel(root, path)
			if fsignore.Match(pattern, filepath.ToSlash(rel)) {
				return true, nil
			}
		}
	}
	return metadata.Always, nil
}

func (h *scopedRulesHook) After(ctx context.Context, evt *hooks.Event) error {
	if evt == nil || evt.Type != hooks.EventModelCallAfter || evt.Error != nil {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	response, responseOK := evt.Output.(*model.ChatResponse)
	if !ok || req == nil || summaryRequest(req) || !responseOK || response == nil || response.Err != nil {
		return nil
	}
	root, err := filepath.Abs(builtins.WorkspaceRoot(ctx, h.root))
	if err != nil {
		return err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	key, err := scopedRulesKey(ctx, root)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.states[key]
	if state == nil || state.offered == nil {
		return nil
	}
	retained := state.offeredBody == ""
	for _, message := range req.Messages {
		if message.Name == "harness-scoped-rules" && message.Content == state.offeredBody {
			retained = true
		}
	}
	// Successful provider delivery and exact retained bytes establish instruction freshness.
	if retained {
		state.delivered = state.offered
		state.offered = nil
		state.offeredBody = ""
	}
	return nil
}
