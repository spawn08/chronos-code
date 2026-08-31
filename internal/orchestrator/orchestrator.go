package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"

	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/graph"
	"github.com/spawn08/chronos-code/internal/guardrail"
	"github.com/spawn08/chronos-code/internal/incctx"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos-code/internal/toolcompress"
	"github.com/spawn08/chronos-code/internal/workspace"
)

type Orchestrator struct {
	agents     map[string]*agent.Agent
	order      []string
	active     string
	store      storage.Storage
	cfg        *config.Config
	graphStore *graph.Store
	watcher    *graph.Watcher

	sessionMgr *session.Manager
	sessions   map[string]string // agentID -> current sessionID

	router    *router.Router
	budget    *budget.Tracker
	memory    *memory.Store
	workspace *workspace.Info
}

// OpenStorageForCLI opens the same storage.Storage backend New would (per
// cfg.Defaults.Storage), without building any agents. It lets lightweight CLI
// subcommands (e.g. `session list/delete/export`) operate on session/event/
// checkpoint data directly, without the cost (and API-key requirements) of
// spinning up a full Orchestrator.
func OpenStorageForCLI(cfg *config.Config) (storage.Storage, string, error) {
	return openStorage(cfg)
}

// New builds an Orchestrator: it loads/builds every configured agent, wires
// storage, the code graph, workspace/memory/guardrail/security/budget/router
// support, and establishes each agent's persistent session id. If
// resumeSessionID is non-empty, it is used verbatim as the session id for
// every agent (bypassing cfg.Session.AutoResume's "reuse the most recent
// session" heuristic) — this is how the CLI's `--resume <id>` flag resumes a
// specific prior session rather than just "the latest one."
func New(ctx context.Context, cfg *config.Config, resumeSessionID string) (*Orchestrator, error) {
	store, dsn, err := openStorage(cfg)
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}

	agents, err := agent.BuildAll(ctx, &cfg.FileConfig)
	if err != nil {
		return nil, fmt.Errorf("build agents: %w", err)
	}

	order := make([]string, 0, len(agents))
	for _, ac := range cfg.Agents {
		if _, ok := agents[ac.ID]; ok {
			order = append(order, ac.ID)
		}
	}
	sort.Strings(order)

	for _, a := range agents {
		if a.Storage == nil {
			a.Storage = store
		}
	}

	projectDir, _, _ := config.Discover()

	sessionMgr := session.NewManager(store, dsn)
	sessions := setupSessions(ctx, cfg, sessionMgr, agents, resumeSessionID)

	graphStore, watcher := setupGraph(ctx, cfg, agents)

	root := cfg.Workspace.Root
	if root == "" {
		root = config.WorkspaceRoot()
	}
	wsInfo := setupWorkspace(root, agents)

	memStore := setupMemory(cfg, agents)

	rt := setupRouter(cfg, projectDir)

	grCfg := setupGuardrails(cfg, projectDir, agents)

	setupSecurity(cfg, projectDir, root, store, agents)

	maxTokens, _ := grCfg.TokenBudget()
	tracker := budget.NewTracker(maxTokens, cfg.Tools.CompressionThresholdTokens)
	for _, a := range agents {
		a.Hooks = append(a.Hooks, tracker)
	}

	for _, a := range agents {
		agentID := a.ID
		toolcompress.WrapDynamic(a, func(ctx context.Context) int {
			return tracker.CompressionThreshold(sessionOrAgentKey(ctx, agentID))
		})
		if err := a.ConnectMCP(ctx); err != nil {
			fmt.Printf("warning: MCP connect for %s: %v\n", a.ID, err)
		}
		incctx.Wrap(a, root)
	}

	active := "coder"
	if _, ok := agents[active]; !ok && len(order) > 0 {
		active = order[0]
	}

	return &Orchestrator{
		agents:     agents,
		order:      order,
		active:     active,
		store:      store,
		cfg:        cfg,
		graphStore: graphStore,
		watcher:    watcher,
		sessionMgr: sessionMgr,
		sessions:   sessions,
		router:     rt,
		budget:     tracker,
		memory:     memStore,
		workspace:  wsInfo,
	}, nil
}

// sessionOrAgentKey resolves the same per-conversation cache/tracking key
// toolcompress and incctx already use internally: the in-context session ID
// if present, falling back to the agent ID.
func sessionOrAgentKey(ctx context.Context, agentID string) string {
	if id := storage.SessionFromContext(ctx); id != "" {
		return id
	}
	return agentID
}

// setupSessions establishes a persistent session id per agent (PRD P2-001).
// If resumeSessionID is non-empty, it is used verbatim for every agent.
// Otherwise, when cfg.Session.AutoResume is set, the agent's most recent
// session is reused (giving ChatWithSession a full event ledger to
// reconstruct from); if neither applies, a fresh session id is generated.
// Failures are logged as warnings and fall back to an in-memory
// (never-persisted-to-the-index) id so the agent still works without
// resumability.
func setupSessions(ctx context.Context, cfg *config.Config, mgr *session.Manager, agents map[string]*agent.Agent, resumeSessionID string) map[string]string {
	sessions := make(map[string]string, len(agents))
	for id, a := range agents {
		sid := resumeSessionID
		if sid == "" && cfg.Session.AutoResume {
			if latest, err := mgr.Latest(ctx, a.ID); err == nil && latest != nil {
				sid = latest.ID
			}
		}
		if sid == "" {
			sid = session.NewSessionID()
		}
		if err := mgr.Ensure(ctx, sid, a.ID); err != nil {
			fmt.Printf("warning: ensure session for %s: %v\n", a.ID, err)
		}
		sessions[id] = sid
	}
	return sessions
}

// setupWorkspace detects the project's language(s) and file index (PRD
// P2-005), registers the workspace_info T0 tool, and appends a short banner
// to every agent's system prompt. Failure is non-fatal (matches the graph
// setup's error-handling style): the harness still works, just without
// workspace context.
func setupWorkspace(root string, agents map[string]*agent.Agent) *workspace.Info {
	info, err := workspace.Detect(root)
	if err != nil {
		fmt.Printf("warning: detect workspace: %v\n", err)
		return nil
	}
	banner := info.Banner()
	for _, a := range agents {
		a.Tools.Register(workspace.Tool(info))
		a.SystemPrompt = strings.TrimSpace(a.SystemPrompt + "\n\n" + banner)
	}
	return info
}

// setupMemory wires chronos-code's YAML-backed memory store (PRD P2-002)
// into every agent via ContextPinsFn, which chronos's sdk/agent evaluates
// fresh on every turn and injects as pinned (never-summarized) context. A nil
// return (when memory is disabled in config) means callers must check before
// dereferencing.
func setupMemory(cfg *config.Config, agents map[string]*agent.Agent) *memory.Store {
	if !cfg.Memory.Enabled {
		return nil
	}
	dir := filepath.Join(config.ConfigDirName, "memory")
	store := memory.NewStore(dir)
	for _, a := range agents {
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			block, err := store.ContextBlock(5)
			if err != nil || block == "" {
				return nil
			}
			return []model.Message{{Role: model.RoleSystem, Content: block}}
		}
	}
	return store
}

// setupRouter loads routing.yaml (project override at
// <projectDir>/routing.yaml, falling back to the embedded default) and builds
// the deterministic T0 intent classifier (PRD P2-006). A nil return means
// routing is disabled or the config failed to load/parse; callers must
// treat that as "route everything to whatever agent is already active."
//
// It also attaches a T1 fallback classifier (PRD P2-006 gap / G-012): when
// the router's model is configured in routing.yaml, messages that match no
// T0 pattern are classified by that cheap model instead of always defaulting
// to the "code" intent. Failure to build the T1 provider (e.g. missing API
// key) is non-fatal — the router still works with T0-only matching.
func setupRouter(cfg *config.Config, projectDir string) *router.Router {
	if !cfg.Router.Enabled {
		return nil
	}
	data, err := readOverridableFile(projectDir, "routing.yaml", "routing.yaml")
	if err != nil {
		fmt.Printf("warning: load routing.yaml: %v\n", err)
		return nil
	}
	rcfg, err := router.Parse(data)
	if err != nil {
		fmt.Printf("warning: parse routing.yaml: %v\n", err)
		return nil
	}
	rt, err := router.New(rcfg, "coder")
	if err != nil {
		fmt.Printf("warning: build router: %v\n", err)
		return nil
	}
	if rcfg.Router.Model.Provider != "" && rcfg.Router.Model.Model != "" {
		provider, err := agent.BuildProvider(agent.ModelConfig{
			Provider: rcfg.Router.Model.Provider,
			Model:    rcfg.Router.Model.Model,
		})
		if err != nil {
			fmt.Printf("warning: build T1 router classifier: %v\n", err)
		} else if t1 := router.NewT1Classifier(provider, rcfg); t1 != nil {
			rt.SetT1(t1)
		}
	}
	return rt
}

// setupGuardrails loads the guardrail YAML config (project override at
// <projectDir>/guardrails/default.yaml, falling back to the embedded
// default), converts it into chronos guardrails.Rule values, and registers
// them on every agent's Guardrails engine (PRD P2-003) — which chronos's
// sdk/agent already invokes automatically via CheckInput/CheckOutput on
// every Chat/ChatWithSession call. Returns the parsed config (never nil; a
// zero-value *guardrail.Config on failure) so callers can still read its
// TokenBudget for the budget tracker.
func setupGuardrails(cfg *config.Config, projectDir string, agents map[string]*agent.Agent) *guardrail.Config {
	data, err := readOverridableFile(projectDir, "guardrails/default.yaml", "guardrails/default.yaml")
	if err != nil {
		fmt.Printf("warning: load guardrails config: %v\n", err)
		return &guardrail.Config{}
	}
	grCfg, err := guardrail.ParseConfig(data)
	if err != nil {
		fmt.Printf("warning: parse guardrails config: %v\n", err)
		return &guardrail.Config{}
	}

	secretsData, err := readOverridableFile(projectDir, "security.yaml", "security.yaml")
	var extraSecretPatterns []string
	if err == nil {
		if policy, perr := security.LoadPolicy(secretsData); perr == nil {
			extraSecretPatterns = policy.SecretPatterns
		}
	}

	rules, err := guardrail.BuildRules(grCfg, extraSecretPatterns)
	if err != nil {
		fmt.Printf("warning: build guardrail rules: %v\n", err)
		return grCfg
	}
	for _, a := range agents {
		if a.Guardrails == nil {
			a.Guardrails = guardrails.NewEngine()
		}
		for _, r := range rules {
			a.Guardrails.AddRule(r)
		}
	}
	return grCfg
}

// setupSecurity loads security.yaml (project override at
// <projectDir>/security.yaml, falling back to the embedded default) and
// attaches a security.Guard to every agent's hook chain (PRD P2-004), which
// blocks disallowed file paths and shell commands before they execute (see
// security.Guard.Before, wired via chronos's hooks.EventToolCallBefore).
func setupSecurity(cfg *config.Config, projectDir, root string, store storage.Storage, agents map[string]*agent.Agent) *security.Policy {
	data, err := readOverridableFile(projectDir, "security.yaml", "security.yaml")
	if err != nil {
		fmt.Printf("warning: load security.yaml: %v\n", err)
		return &security.Policy{}
	}
	policy, err := security.LoadPolicy(data)
	if err != nil {
		fmt.Printf("warning: parse security.yaml: %v\n", err)
		return &security.Policy{}
	}
	guard := security.NewGuard(policy, root, store)
	for _, a := range agents {
		a.Hooks = append(a.Hooks, guard)
	}
	return policy
}

// readOverridableFile prefers <projectDir>/<overridePath> when projectDir is
// non-empty and the file exists, falling back to the embedded default at
// embeddedName (via internal/defaults.ReadFile).
func readOverridableFile(projectDir, overridePath, embeddedName string) ([]byte, error) {
	if projectDir != "" {
		candidate := filepath.Join(projectDir, overridePath)
		if data, err := os.ReadFile(candidate); err == nil {
			return data, nil
		}
	}
	return defaults.ReadFile(embeddedName)
}

// setupGraph opens the code graph store, indexes the workspace on start
// (PRD P1-007), registers the T0 graph tools on every agent, and starts a
// background watcher to keep the graph fresh. Any failure (e.g. no Go module
// at the workspace root) is logged as a warning and treated as non-fatal —
// the harness still works without the graph, just without the T0 navigation
// tools.
func setupGraph(ctx context.Context, cfg *config.Config, agents map[string]*agent.Agent) (*graph.Store, *graph.Watcher) {
	root := cfg.Workspace.Root
	if root == "" {
		root = config.WorkspaceRoot()
	}

	dbPath := cfg.Workspace.GraphDB
	if dbPath == "" {
		dbPath = ".chronos-code/graph.db"
	}
	if dir := filepath.Dir(dbPath); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fmt.Printf("warning: create graph db dir %s: %v\n", dir, err)
			return nil, nil
		}
	}

	graphStore, err := graph.OpenStore(dbPath)
	if err != nil {
		fmt.Printf("warning: open graph store: %v\n", err)
		return nil, nil
	}

	ix := graph.NewIndexer(graphStore, root)
	indexOnStart := cfg.Workspace.IndexOnStart == nil || *cfg.Workspace.IndexOnStart
	if indexOnStart {
		stats, err := ix.IndexAll(ctx)
		if err != nil {
			fmt.Printf("warning: index code graph: %v\n", err)
		} else {
			fmt.Printf("code graph: indexed %d files, %d symbols, %d edges in %s\n",
				stats.Files, stats.Symbols, stats.Edges, stats.Elapsed.Round(1e6))
		}
	}

	for _, a := range agents {
		for _, def := range graph.Tools(graphStore, root) {
			a.Tools.Register(def)
		}
		for _, def := range graph.ImpactTools(graphStore, root) {
			a.Tools.Register(def)
		}
	}

	var watcher *graph.Watcher
	if indexOnStart {
		watcher, err = graph.Watch(ctx, ix)
		if err != nil {
			fmt.Printf("warning: start graph watcher: %v\n", err)
			watcher = nil
		}
	}

	return graphStore, watcher
}

func (o *Orchestrator) Chat(ctx context.Context, message string) (*model.ChatResponse, error) {
	a := o.ActiveAgent()
	if a == nil {
		return nil, fmt.Errorf("no active agent")
	}
	sid := o.sessions[o.active]
	if sid == "" || a.Storage == nil {
		return a.Chat(ctx, message)
	}
	return a.ChatWithSession(ctx, sid, message)
}

// ChatStream streams a response from the active agent. Note: chronos's
// ChatStream does not append turns to the durable event ledger the way
// ChatWithSession (used by Chat) does — this is a limitation of the
// underlying chronos SDK, not something chronos-code layers on top of it.
// The session id is still threaded through the context so session-scoped
// features that key off it (tool-result compression, the security audit
// log, budget tracking, incremental file-read caching) behave consistently
// whether the caller streams or not.
func (o *Orchestrator) ChatStream(ctx context.Context, message string) (<-chan *model.ChatResponse, error) {
	a := o.ActiveAgent()
	if a == nil {
		return nil, fmt.Errorf("no active agent")
	}
	if sid := o.sessions[o.active]; sid != "" {
		ctx = storage.WithSession(ctx, sid)
	}
	return a.ChatStream(ctx, message)
}

// Route classifies message via the T0 intent router, falling back to the T1
// cheap-model classifier when configured (PRD P2-006), and returns the agent
// id it should be sent to. If routing is disabled or the message doesn't
// match any T0 pattern or T1 classification, it returns the currently active
// agent id and matched=false, so callers can always safely call Route
// without special-casing "router present or not."
func (o *Orchestrator) Route(ctx context.Context, message string) (agentID string, matched bool) {
	if o.router == nil {
		return o.active, false
	}
	_, agentID, matched = o.router.ClassifyWithFallback(ctx, message)
	if _, ok := o.agents[agentID]; !ok {
		return o.active, false
	}
	return agentID, matched
}

func (o *Orchestrator) SwitchAgent(id string) error {
	if _, ok := o.agents[id]; !ok {
		return fmt.Errorf("agent %q not found (available: %v)", id, o.order)
	}
	o.active = id
	return nil
}

func (o *Orchestrator) ActiveAgent() *agent.Agent {
	return o.agents[o.active]
}

func (o *Orchestrator) ActiveID() string {
	return o.active
}

func (o *Orchestrator) ListAgents() []string {
	return o.order
}

func (o *Orchestrator) GetAgent(id string) (*agent.Agent, bool) {
	a, ok := o.agents[id]
	return a, ok
}

func (o *Orchestrator) Store() storage.Storage {
	return o.store
}

// GraphStore returns the code graph store, or nil if indexing failed or was
// disabled at startup.
func (o *Orchestrator) GraphStore() *graph.Store {
	return o.graphStore
}

// SessionManager returns the session lifecycle manager (PRD P2-001), used by
// the CLI's `session` subcommands.
func (o *Orchestrator) SessionManager() *session.Manager {
	return o.sessionMgr
}

// CurrentSessionID returns the session id currently bound to the active
// agent.
func (o *Orchestrator) CurrentSessionID() string {
	return o.sessions[o.active]
}

// MemoryStore returns the YAML-backed memory store (PRD P2-002), or nil if
// memory is disabled in config.
func (o *Orchestrator) MemoryStore() *memory.Store {
	return o.memory
}

// BudgetStatusLine returns a human-readable token-budget status line for the
// active agent's current session (PRD P2-009), suitable for a TUI status
// bar.
func (o *Orchestrator) BudgetStatusLine() string {
	if o.budget == nil {
		return ""
	}
	return o.budget.StatusLine(o.CurrentSessionID())
}

// Workspace returns the detected workspace info (PRD P2-005), or nil if
// detection failed.
func (o *Orchestrator) Workspace() *workspace.Info {
	return o.workspace
}

func (o *Orchestrator) Close() error {
	for _, a := range o.agents {
		a.CloseMCP()
	}
	if o.watcher != nil {
		o.watcher.Close()
	}
	if o.graphStore != nil {
		o.graphStore.Close()
	}
	if o.store != nil {
		return o.store.Close()
	}
	return nil
}

func openStorage(cfg *config.Config) (storage.Storage, string, error) {
	backend := "sqlite"
	dsn := ".chronos-code/sessions.db"
	if cfg.Defaults != nil {
		if cfg.Defaults.Storage.Backend != "" {
			backend = cfg.Defaults.Storage.Backend
		}
		if cfg.Defaults.Storage.DSN != "" {
			dsn = cfg.Defaults.Storage.DSN
		}
	}
	switch backend {
	case "sqlite":
		if dir := filepath.Dir(dsn); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, "", fmt.Errorf("create storage dir %s: %w", dir, err)
			}
		}
		store, err := sqlite.New(dsn)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: %w", err)
		}
		if err := store.Migrate(context.Background()); err != nil {
			return nil, "", fmt.Errorf("sqlite migrate: %w", err)
		}
		return store, dsn, nil
	default:
		return nil, "", fmt.Errorf("unsupported storage backend: %s", backend)
	}
}
