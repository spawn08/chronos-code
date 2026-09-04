package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	guardrails "github.com/spawn08/chronos/engine/guardrails"
	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/mcp"
	"github.com/spawn08/chronos/engine/model"
	chronosstream "github.com/spawn08/chronos/engine/stream"
	"github.com/spawn08/chronos/engine/tool"
	chronostrace "github.com/spawn08/chronos/os/trace"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"

	"github.com/spawn08/chronos-code/internal/activation"
	"github.com/spawn08/chronos-code/internal/apierror"
	"github.com/spawn08/chronos-code/internal/attention"
	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/graph"
	"github.com/spawn08/chronos-code/internal/guardrail"
	"github.com/spawn08/chronos-code/internal/incctx"
	"github.com/spawn08/chronos-code/internal/learning"
	"github.com/spawn08/chronos-code/internal/lsp"
	"github.com/spawn08/chronos-code/internal/mcpdiscover"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/modelinfo"
	"github.com/spawn08/chronos-code/internal/projectdocs"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos-code/internal/skills"
	"github.com/spawn08/chronos-code/internal/teambuilder"
	"github.com/spawn08/chronos-code/internal/toolcompress"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos-code/internal/workspace"

	"github.com/spawn08/chronos/sdk/team"
)

type Orchestrator struct {
	agents     map[string]*agent.Agent
	order      []string
	active     string
	primary    string
	store      storage.Storage
	cfg        *config.Config
	graphStore *graph.Store
	watcher    *graph.Watcher

	sessionMgr *session.Manager
	sessions   map[string]string // agentID -> current sessionID
	sessionMu  sync.RWMutex

	router             *router.Router
	routingConfig      *router.Config
	routingMu          sync.Mutex
	routingState       map[string]router.Classification
	modelOverrides     map[string]bool
	buildProvider      func(agent.ModelConfig) (model.Provider, error)
	budget             *budget.Tracker
	budgetMu           sync.RWMutex
	usdBudget          *budget.Tracker
	memory             *memory.Store
	workspace          *workspace.Info
	actBuf             *activation.Buffer
	attBudget          *attention.Budgeter
	teams              map[string]*team.Team
	projectDocsWatcher *projectdocs.Watcher
	skillCatalog       []*skills.Skill
	permissionChecker  *security.PermissionChecker
	permissionYolo     atomic.Bool
	hookRunner         *security.HookRunner
	learningStore      *learning.SQLStore
	planMode           atomic.Bool
	editsMu            sync.Mutex
	edits              []fileCheckpoint
	lastExecMu         sync.Mutex
	lastExecRoute      router.Classification
	lastExecAgent      string
	lspManager         interface{ Close() error }
	broker             *chronosstream.Broker
	policy             *security.Policy
	mcpFactory         mcpdiscover.ClientFactory
	mcpTimeout         time.Duration
	mcpRuntimes        []*mcpdiscover.Runtime
	mcpMu              sync.Mutex
	mcpClosed          bool
	closeOnce          sync.Once
	closeErr           error
}

type SkillInfo struct {
	Name        string
	Description string
	Source      string
}

type ExecutionMode uint8

const (
	ExecutionBlocking ExecutionMode = iota
	ExecutionStreaming
)

// ExecutionRequest describes one orchestrated user task. RequestedAgent and
// SessionID preserve explicit caller selections; otherwise the executor
// chooses the route and that agent's current session.
type ExecutionRequest struct {
	Message        string
	Mode           ExecutionMode
	RequestedAgent string
	SessionID      string
	TaskID         string
	// PPD supplies optional complexity and recursion metadata. Task kind and
	// classifier-derived risk are always calculated from Message.
	PPD           *router.PPDRequest
	PolicyContext map[string]any
	// VerificationMode applies policy to the supplied or runtime-derived
	// obligations and events; execution does not collect evidence automatically.
	VerificationMode        verification.Mode
	VerificationObligations []verification.Obligation
	VerificationEvents      []execution.Event
}

// ExecutionResult carries the common identity and either a blocking response
// or a streaming response channel, according to the request mode.
type ExecutionResult struct {
	AgentID       string
	SessionID     string
	TaskID        string
	PPDDecision   *router.PPDDecision
	MemoryIntent  *memory.IntentResult
	ContextReport ContextReport
	Response      *model.ChatResponse
	Stream        <-chan *model.ChatResponse
}

type taskIDKey struct{}
type executionPolicyContextKey struct{}

// TaskIDFromContext returns the task identity assigned by Execute.
func TaskIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(taskIDKey{}).(string)
	return id
}

// ExecutionPolicyContext returns the policy data assigned by Execute.
func ExecutionPolicyContext(ctx context.Context) map[string]any {
	policy, _ := ctx.Value(executionPolicyContextKey{}).(map[string]any)
	return policy
}

const (
	userHookPromptContextTokens = 1000
	DefaultPrimaryAgentID       = "chronos-code"
)

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
func New(ctx context.Context, cfg *config.Config, resumeSessionID string) (_ *Orchestrator, err error) {
	store, dsn, err := openStorage(cfg)
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	var learningStore *learning.SQLStore
	var languageServerManager *lsp.Manager
	var mcpRuntimes []*mcpdiscover.Runtime
	defer func() {
		if err == nil {
			return
		}
		var cleanupErrs []error
		if learningStore != nil {
			if closeErr := learningStore.Close(context.Background()); closeErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("close learning telemetry after startup failure: %w", closeErr))
			}
		}
		if languageServerManager != nil {
			if closeErr := languageServerManager.Close(); closeErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("close LSP manager after startup failure: %w", closeErr))
			}
		}
		for _, runtime := range mcpRuntimes {
			if closeErr := runtime.Close(); closeErr != nil {
				cleanupErrs = append(cleanupErrs, fmt.Errorf("close MCP runtime after startup failure: %w", closeErr))
			}
		}
		if closeErr := store.Close(); closeErr != nil {
			cleanupErrs = append(cleanupErrs, fmt.Errorf("close storage after startup failure: %w", closeErr))
		}
		err = errors.Join(append([]error{err}, cleanupErrs...)...)
	}()

	applyStoredCredentials(ctx, cfg)

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
	setupTracing(store, agents)

	projectDir, userDir, discoverErr := config.Discover()
	if discoverErr != nil {
		fmt.Printf("warning: discover project config dir: %v (falling back to embedded defaults)\n", discoverErr)
	}

	sessionMgr := session.NewManager(store, dsn)
	sessions := setupSessions(ctx, cfg, sessionMgr, agents, resumeSessionID)

	graphStore, watcher := setupGraph(ctx, cfg, agents)

	root := cfg.Workspace.Root
	if root == "" {
		root = config.WorkspaceRoot()
	}
	learningStore, err = setupLearningTelemetry(ctx, cfg, root, agents)
	if err != nil {
		return nil, fmt.Errorf("configure learning telemetry: %w", err)
	}
	wsInfo := setupWorkspace(root, agents)

	if cfg.Session.RecallPriorSummariesEnabled() {
		setupSessionSummaries(sessionMgr, agents)
	}
	memStore := setupMemory(cfg, agents)
	if cfg.Learning.PatternInjectionEnabled() {
		setupLearnedPatterns(ctx, cfg, root, agents)
	}

	pdWatcher := setupProjectDocs(ctx, cfg, root, agents)

	skillCatalog := setupSkills(cfg, root, agents)
	languageServerManager = setupLSP(root, wsInfo, agents)

	rt, routingConfig := setupRouter(cfg, projectDir, selectPrimaryAgent(agents, order))

	policy, err := setupSecurity(projectDir, userDir, root, store, agents)
	if err != nil {
		return nil, fmt.Errorf("configure security: %w", err)
	}
	grCfg := setupGuardrails(cfg, projectDir, agents, policy.SecretPatterns)

	maxTokens, _ := grCfg.TokenBudget()
	tracker := budget.NewTracker(maxTokens, cfg.Tools.CompressionThresholdTokens)
	attBudget := attention.NewBudgeter(100)
	for _, a := range agents {
		a.Hooks = append(a.Hooks, attBudget)
	}

	actBuf := activation.NewBuffer(50)
	broker := chronosstream.NewBroker(chronosstream.WithBufferSize(256))
	for _, a := range agents {
		a.Broker = broker
	}
	if err := setupSubAgents(agents); err != nil {
		return nil, fmt.Errorf("configure subagent delegation: %w", err)
	}
	var hookRunner *security.HookRunner
	if len(cfg.Hooks.PreToolCall)+len(cfg.Hooks.PostToolCall)+len(cfg.Hooks.UserPromptSubmit) > 0 {
		hookRunner, err = security.NewHookRunner(root)
		if err != nil {
			return nil, fmt.Errorf("configure user hooks: %w", err)
		}
	}
	var discoveredServers []mcp.ServerConfig
	if cfg.MCP.DiscoveryEnabled() {
		discovered := mcpdiscover.Load(root)
		if discovered.Err != nil {
			fmt.Printf("warning: MCP discovery failed; configured servers remain available\n")
		}
		discoveredServers = discovered.Servers
	}
	mcpRuntimes = setupMCPRuntimes(ctx, agents, discoveredServers, policy, mcpdiscover.DefaultConnectTimeout, mcpdiscover.NewClient)
	for _, a := range agents {
		// Wrap tool handlers only after
		// MCP registration so server tools are covered too, not just the
		// built-in/YAML-declared ones registered before this point.
		wrapUserToolHooks(a, cfg.Hooks, hookRunner)
		agentID := a.ID
		toolcompress.WrapDynamic(a, func(ctx context.Context) int {
			base := tracker.CompressionThreshold(sessionOrAgentKey(ctx, agentID))
			w := attBudget.CurrentWeight(sessionOrAgentKey(ctx, agentID))
			return attention.AdjustThreshold(base, w)
		})
		wrapToolResultCap(a)
		incctx.Wrap(a, root)
		incctx.WrapGrep(a, root)
		if graphStore != nil {
			activation.Wrap(a, graphStore, actBuf)
		}
	}

	teams := setupTeams(cfg, agents)

	normalizeToolPermissions(agents)

	active := selectPrimaryAgent(agents, order)

	orch := &Orchestrator{
		agents:             agents,
		order:              order,
		active:             active,
		primary:            active,
		store:              store,
		cfg:                cfg,
		graphStore:         graphStore,
		watcher:            watcher,
		sessionMgr:         sessionMgr,
		sessions:           sessions,
		router:             rt,
		routingConfig:      routingConfig,
		routingState:       make(map[string]router.Classification),
		modelOverrides:     make(map[string]bool),
		budget:             tracker,
		usdBudget:          budget.NewTrackerWithUSDCap(0, 0, 0),
		memory:             memStore,
		workspace:          wsInfo,
		actBuf:             actBuf,
		attBudget:          attBudget,
		teams:              teams,
		projectDocsWatcher: pdWatcher,
		skillCatalog:       skillCatalog,
		permissionChecker:  security.NewPermissionChecker(policy, root),
		hookRunner:         hookRunner,
		learningStore:      learningStore,
		lspManager:         languageServerManager,
		broker:             broker,
		policy:             policy,
		mcpFactory:         mcpdiscover.NewClient,
		mcpTimeout:         mcpdiscover.DefaultConnectTimeout,
		mcpRuntimes:        mcpRuntimes,
	}
	orch.SetApprovalHandler(nil)
	for _, a := range agents {
		// Context guard runs before budget: it trims messages that would exceed
		// the model's context window, preventing 413/400 token-limit errors that
		// the SDK's tool-calling loop doesn't guard against.
		a.Hooks = append(a.Hooks, sessionUXHook{orchestrator: orch})
		a.Hooks = append(a.Hooks, newContextGuardHook(a.Model.Model(), len(a.Tools.List())))
		a.Hooks = append(a.Hooks, modelEscalationHook{orchestrator: orch, agentID: a.ID})
		// Keep the budget hook last: if it reserves, no later Before hook can
		// abort the call and strand the reservation.
		a.Hooks = append(a.Hooks, budgetHook{tracker: tracker, orchestrator: orch, agentID: a.ID})
	}
	return orch, nil
}

// applyStoredCredentials fills in ModelConfig.APIKey from each provider's
// full authentication precedence chain (ROADMAP.md §5.3: gateway/API-key env
// vars > chronos-code's own OAuth login > reuse of an existing Claude Code /
// Codex CLI login > chronos-code's own stored API key) for any agent (or the
// shared Defaults block) whose provider has no api_key already set in YAML —
// without this, `chronos-code auth login <provider> ...` stores a credential
// that nothing ever reads back, silently having no effect on actual model
// calls.
//
// CAVEAT: chronos's provider constructors send ModelConfig.APIKey as
// whatever bearer/key format that provider's SDK expects for a plain API
// key (e.g. Anthropic's x-api-key header). An OAuth access token resolved
// here (chronos-code's own OAuth login, or a reused Claude Code/Codex
// token) is passed through the same field; whether the provider's HTTP
// layer accepts it as-is depends on that provider's API — this is the same
// simplification called out in the prior version of this function, now
// widened to cover the full chain rather than just the keychain API-key
// case.
func applyStoredCredentials(ctx context.Context, cfg *config.Config) {
	store := auth.NewStore()
	resolved := make(map[string]string) // provider -> effective token (may be "")

	resolve := func(provider string) string {
		key, cached := resolved[provider]
		if cached {
			return key
		}
		key = auth.Resolve(ctx, store, provider).Token
		resolved[provider] = key
		return key
	}

	apply := func(mc *agent.ModelConfig) {
		if mc.Provider == "" {
			return
		}
		if mc.APIKey == "" {
			if key := resolve(mc.Provider); key != "" {
				mc.APIKey = key
			}
		}
		if mc.BaseURL == "" {
			if override, ok := cfg.Providers[mc.Provider]; ok && override.BaseURL != "" {
				mc.BaseURL = override.BaseURL
			}
		}
	}

	if cfg.Defaults != nil {
		apply(&cfg.Defaults.Model)
	}
	for i := range cfg.Agents {
		apply(&cfg.Agents[i].Model)
	}
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

func wrapUserToolHooks(a *agent.Agent, configured config.HooksConfig, runner *security.HookRunner) {
	if runner == nil {
		return
	}
	for _, def := range a.Tools.List() {
		if def.Handler == nil {
			continue
		}
		wrapped := *def
		original := def.Handler
		toolName := def.Name
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			vars := map[string]any{
				"tool_name":  toolName,
				"tool_args":  args,
				"session_id": storage.SessionFromContext(ctx),
				"agent_id":   a.ID,
			}
			for _, hook := range configured.PreToolCall {
				if _, err := runner.Run(ctx, hook, vars); err != nil {
					return nil, fmt.Errorf("pre-tool hook %q: %w", hook.Name, err)
				}
			}

			result, handlerErr := original(ctx, args)
			vars["tool_output"] = result
			for _, hook := range configured.PostToolCall {
				_, _ = runner.Run(ctx, hook, vars)
			}
			return result, handlerErr
		}
		a.Tools.Register(&wrapped)
	}
}

// budgetHook adapts a single shared *budget.Tracker into a per-agent
// hooks.Hook: it forces agentID onto ctx as the session id (via
// storage.WithSession) whenever ctx doesn't already carry one, before
// delegating to the tracker. This keeps the tracker's fallback-key behavior
// consistent with sessionOrAgentKey above (agent ID, not the model name
// budget.Tracker would otherwise fall back to when it can't see which agent
// a *hooks.Event came from).
type budgetHook struct {
	tracker      *budget.Tracker
	orchestrator *Orchestrator
	agentID      string
}

const budgetReservationMetadataKey = "chronos_code_budget_reservation"

type budgetReservation struct {
	tracker *budget.Tracker
	id      budget.ReservationID
}

func (h budgetHook) withFallbackSession(ctx context.Context) context.Context {
	if storage.SessionFromContext(ctx) == "" {
		return storage.WithSession(ctx, h.agentID)
	}
	return ctx
}

func (h budgetHook) Before(ctx context.Context, evt *hooks.Event) error {
	ctx = h.withFallbackSession(ctx)
	if evt.Type == hooks.EventModelCallBefore {
		if err := claimTurnModelCall(ctx); err != nil {
			return err
		}
	}
	if err := h.tracker.Before(ctx, evt); err != nil {
		return err
	}
	if evt.Type != hooks.EventModelCallBefore || h.orchestrator == nil {
		return nil
	}
	req, ok := evt.Input.(*model.ChatRequest)
	if !ok || req == nil {
		return fmt.Errorf("reserve model cost: missing chat request")
	}
	modelID := req.Model
	if evt.Metadata != nil {
		if provider, ok := evt.Metadata["provider"].(model.Provider); modelID == "" && ok {
			modelID = provider.Model()
		}
	}
	tracker := h.orchestrator.currentUSDBudget()
	id, err := tracker.Reserve(storage.SessionFromContext(ctx), modelID,
		model.NewTokenCounter(modelID).CountTokens(req.Messages), req.MaxTokens)
	if err != nil {
		if errors.Is(err, budget.ErrUnknownModel) && !tracker.HasUSDCap() {
			return nil
		}
		return fmt.Errorf("reserve model cost: %w", err)
	}
	if evt.Metadata == nil {
		evt.Metadata = make(map[string]any)
	}
	evt.Metadata[budgetReservationMetadataKey] = budgetReservation{tracker: tracker, id: id}
	return nil
}

func (h budgetHook) After(ctx context.Context, evt *hooks.Event) error {
	ctx = h.withFallbackSession(ctx)
	if err := h.tracker.After(ctx, evt); err != nil {
		return err
	}
	if evt.Type != hooks.EventModelCallAfter {
		return nil
	}
	reservation, ok := evt.Metadata[budgetReservationMetadataKey].(budgetReservation)
	if !ok {
		return nil
	}
	delete(evt.Metadata, budgetReservationMetadataKey)
	var usage model.Usage
	if evt.Error == nil {
		if resp, ok := evt.Output.(*model.ChatResponse); ok && resp != nil {
			usage = resp.Usage
		}
	}
	return reservation.tracker.ReconcileUsage(reservation.id, usage)
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

func setupLearningTelemetry(ctx context.Context, cfg *config.Config, root string, agents map[string]*agent.Agent) (*learning.SQLStore, error) {
	if !cfg.Learning.Enabled {
		return nil, nil
	}
	dir := filepath.Join(root, config.ConfigDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create learning telemetry directory %s: %w", dir, err)
	}
	store, err := learning.OpenSQLStore(ctx, filepath.Join(dir, "memory.db"))
	if err != nil {
		return nil, fmt.Errorf("open learning telemetry: %w", err)
	}
	for _, a := range agents {
		a.Hooks = append(a.Hooks, learning.NewTelemetryRecorder(store, root, a.ID))
	}
	return store, nil
}

// setupTracing wires chronos's span-based execution tracer into every agent
// (PRD P3-001), persisting model_call/tool_call/approval spans via the same
// Storage backend sessions/memory/audit already use. Without this, every
// agent's Tracer field stays nil and chronos never writes to the traces
// table — internal/learning.Analyze (and hence `chronos-code learn
// suggest`, PRD P3-002) would have nothing to read. A single Collector is
// shared across all agents; it carries no per-agent state of its own.
func setupTracing(store storage.Storage, agents map[string]*agent.Agent) {
	tracer := chronostrace.NewCollector(store)
	for _, a := range agents {
		if a.Tracer == nil {
			a.Tracer = tracer
		}
	}
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

const maxPinnedLSPDiagnostics = 5

const (
	maxPriorSessionSummaries    = 3
	maxPriorSessionSummaryBytes = 2000
)

func setupSessionSummaries(manager *session.Manager, agents map[string]*agent.Agent) {
	if manager == nil {
		return
	}
	for _, a := range agents {
		prev := a.ContextPinsFn
		agentID := a.ID
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var messages []model.Message
			if prev != nil {
				messages = append(messages, prev(ctx)...)
			}
			query, _ := ctx.Value(messageKey{}).(string)
			summaries, err := manager.RecallSummaries(ctx, agentID, storage.SessionFromContext(ctx), query, maxPriorSessionSummaries, maxPriorSessionSummaryBytes)
			if err != nil {
				contextSourceOmitted(ctx, ContextSourceSessionSummaries, ContextOmittedSourceError)
				return messages
			}
			if len(summaries) == 0 {
				contextSourceOmitted(ctx, ContextSourceSessionSummaries, ContextOmittedNotSelected)
				return messages
			}
			var b strings.Builder
			truncated := false
			b.WriteString("Relevant context from prior sessions:")
			for _, summary := range summaries {
				fmt.Fprintf(&b, "\n- [session=%s source=%s updated=%s] %s", summary.SessionID, summary.Source, summary.UpdatedAt.UTC().Format(time.RFC3339), summary.Text)
				truncated = truncated || summary.Truncated
			}
			content := b.String()
			contextSourceSelected(ctx, ContextSourceSessionSummaries, len(summaries), len(content), truncated)
			return append(messages, model.Message{Role: model.RoleSystem, Content: content})
		}
	}
}

// setupLSP registers the build-tag-dependent tools before runtime wrappers are
// installed. The manager remains lazy, so missing server executables cannot
// prevent startup.
func setupLSP(root string, info *workspace.Info, agents map[string]*agent.Agent) *lsp.Manager {
	manager := lsp.NewManager(root)
	var files []string
	if info != nil {
		files = info.Files
	}
	installLSPTools(root, files, agents, lsp.Tools(manager, root))
	return manager
}

func installLSPTools(root string, files []string, agents map[string]*agent.Agent, definitions []*tool.Definition) {
	var diagnostics *tool.Definition
	for _, definition := range definitions {
		if definition.Name == "lsp_diagnostics" {
			diagnostics = definition
		}
		for _, a := range agents {
			a.Tools.Register(definition)
		}
	}
	if diagnostics == nil {
		return
	}

	for _, a := range agents {
		prev := a.ContextPinsFn
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var messages []model.Message
			if prev != nil {
				messages = append(messages, prev(ctx)...)
			}
			message, _ := ctx.Value(messageKey{}).(string)
			pin, count, reason, truncated := lspDiagnosticPin(ctx, root, files, message, diagnostics)
			if pin != "" {
				contextSourceSelected(ctx, ContextSourceDiagnostics, count, len(pin), truncated)
				messages = append(messages, model.Message{Role: model.RoleSystem, Content: pin})
			} else {
				contextSourceOmitted(ctx, ContextSourceDiagnostics, reason)
			}
			return messages
		}
	}
}

func lspDiagnosticPin(ctx context.Context, root string, files []string, message string, diagnostics *tool.Definition) (string, int, string, bool) {
	if message == "" || diagnostics == nil || diagnostics.Handler == nil {
		return "", 0, ContextOmittedNotSelected, false
	}

	type pinnedDiagnostic struct {
		file, severity, message string
		line, column            int
	}
	var pinned []pinnedDiagnostic
	var errorsCount, warningsCount, total int
	hadError := false
	for _, file := range referencedWorkspaceFiles(root, files, message) {
		result, err := diagnostics.Handler(ctx, map[string]any{"file": file})
		if err != nil {
			hadError = true
			continue
		}
		output, ok := result.(map[string]any)
		if !ok {
			continue
		}
		items, ok := output["diagnostics"].([]map[string]any)
		if !ok {
			continue
		}
		for _, item := range items {
			severity, _ := item["severity"].(string)
			switch severity {
			case "error":
				errorsCount++
			case "warning":
				warningsCount++
			}
			total++
			if len(pinned) == maxPinnedLSPDiagnostics {
				continue
			}
			text, _ := item["message"].(string)
			pinned = append(pinned, pinnedDiagnostic{
				file: file, severity: severity, message: text,
				line: numericValue(item["line"]), column: numericValue(item["col"]),
			})
		}
	}
	if total == 0 {
		if hadError {
			return "", 0, ContextOmittedSourceError, false
		}
		return "", 0, ContextOmittedNotSelected, false
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Fresh LSP diagnostics for referenced files (errors: %d, warnings: %d; showing %d of %d):", errorsCount, warningsCount, len(pinned), total)
	for _, diagnostic := range pinned {
		fmt.Fprintf(&b, "\n- %s:%d:%d [%s] %s", diagnostic.file, diagnostic.line, diagnostic.column, diagnostic.severity, diagnostic.message)
	}
	return b.String(), len(pinned), "", total > len(pinned)
}

func referencedWorkspaceFiles(root string, files []string, message string) []string {
	mentioned := make(map[string]bool)
	for _, field := range strings.Fields(message) {
		field = strings.Trim(field, "`'\"()[]{}<>,:;!?")
		field = filepath.ToSlash(filepath.Clean(field))
		mentioned[field] = true
	}

	baseCounts := make(map[string]int, len(files))
	for _, file := range files {
		baseCounts[filepath.Base(file)]++
	}
	var referenced []string
	for _, file := range files {
		rel := filepath.ToSlash(filepath.Clean(file))
		absolute := filepath.ToSlash(filepath.Join(root, file))
		base := filepath.Base(file)
		if mentioned[rel] || mentioned["./"+rel] || mentioned[absolute] || baseCounts[base] == 1 && mentioned[base] {
			referenced = append(referenced, file)
		}
	}
	return referenced
}

func numericValue(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case int64:
		return int(value)
	case float64:
		return int(value)
	default:
		return 0
	}
}

// messageKey is a context key for the current user message, set by Chat so
// ContextPinsFn can use it for relevance-ranked memory recall (P3-009).
type messageKey struct{}
type explicitSkillKey struct{}

const maxRecentSkillTools = 5

type skillToolHistory struct {
	mu      sync.Mutex
	agentID string
	tools   map[string][]string
}

func newSkillToolHistory(agentID string) *skillToolHistory {
	return &skillToolHistory{agentID: agentID, tools: make(map[string][]string)}
}

func (h *skillToolHistory) Before(context.Context, *hooks.Event) error { return nil }

func (h *skillToolHistory) After(ctx context.Context, evt *hooks.Event) error {
	if evt.Type != hooks.EventToolCallAfter || evt.Name == "" {
		return nil
	}
	key := sessionOrAgentKey(ctx, h.agentID)
	h.mu.Lock()
	defer h.mu.Unlock()
	tools := append(h.tools[key], evt.Name)
	if len(tools) > maxRecentSkillTools {
		tools = tools[len(tools)-maxRecentSkillTools:]
	}
	h.tools[key] = tools
	return nil
}

func (h *skillToolHistory) query(ctx context.Context, message string) string {
	key := sessionOrAgentKey(ctx, h.agentID)
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.tools[key]) == 0 {
		return message
	}
	return message + "\n" + strings.Join(h.tools[key], " ")
}

// setupMemory wires chronos-code's YAML-backed memory store (PRD P2-002)
// into every agent via ContextPinsFn, which chronos's sdk/agent evaluates
// fresh on every turn and injects as pinned (never-summarized) context.
// When the context carries a user message (set by Chat), memories are
// ranked by semantic relevance (PRD P3-009) instead of recency. A nil
// return (when memory is disabled in config) means callers must check before
// dereferencing.
func setupMemory(cfg *config.Config, agents map[string]*agent.Agent) *memory.Store {
	if !cfg.Memory.Enabled {
		return nil
	}
	dir := filepath.Join(config.ConfigDirName, "memory")
	store := memory.NewStore(dir)
	for _, a := range agents {
		prev := a.ContextPinsFn
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var messages []model.Message
			if prev != nil {
				messages = append(messages, prev(ctx)...)
			}
			tenantStore := store.ForContext(ctx)
			if msg, ok := ctx.Value(messageKey{}).(string); ok && msg != "" {
				scored, err := tenantStore.Recall(msg, 5)
				if err != nil {
					contextSourceOmitted(ctx, ContextSourceMemory, ContextOmittedSourceError)
					return messages
				}
				if len(scored) > 0 {
					content := formatScoredMemories(scored)
					truncated := false
					for _, item := range scored {
						truncated = truncated || len(item.Record.Content) > 120
					}
					contextSourceSelected(ctx, ContextSourceMemory, len(scored), len(content), truncated)
					return append(messages, model.Message{Role: model.RoleSystem, Content: content})
				}
			}
			block, err := tenantStore.ContextBlock(5)
			if err != nil {
				contextSourceOmitted(ctx, ContextSourceMemory, ContextOmittedSourceError)
				return messages
			}
			if block == "" {
				contextSourceOmitted(ctx, ContextSourceMemory, ContextOmittedNotSelected)
				return messages
			}
			contextSourceSelected(ctx, ContextSourceMemory, strings.Count(block, "\n- "), len(block), len(block) >= 800)
			return append(messages, model.Message{Role: model.RoleSystem, Content: block})
		}
	}
	return store
}

func formatScoredMemories(scored []memory.ScoredRecord) string {
	var b strings.Builder
	b.WriteString("Known project/user/feedback notes (relevance-ranked):")
	for _, sr := range scored {
		content := sr.Record.Content
		if len(content) > 120 {
			content = content[:120] + "..."
		}
		fmt.Fprintf(&b, "\n- [%s] %s", sr.Record.Category, content)
	}
	return b.String()
}

func setupLearnedPatterns(ctx context.Context, cfg *config.Config, root string, agents map[string]*agent.Agent) {
	if !cfg.Learning.Enabled {
		return
	}
	repoPath, sourceRevision, err := learning.RepositoryIdentity(ctx, root)
	if err != nil {
		return
	}
	outputDir := cfg.Learning.OutputDir
	if outputDir == "" {
		outputDir = filepath.Join(config.ConfigDirName, "learned")
	}
	if !filepath.IsAbs(outputDir) {
		outputDir = filepath.Join(repoPath, outputDir)
	}
	setupLearnedPatternPins(learning.NewStore(outputDir), repoPath, sourceRevision, agents)
}

func setupLearnedPatternPins(store *learning.Store, repoPath, sourceRevision string, agents map[string]*agent.Agent) {
	for _, a := range agents {
		prev := a.ContextPinsFn
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var messages []model.Message
			if prev != nil {
				messages = append(messages, prev(ctx)...)
			}
			trigger, _ := ctx.Value(messageKey{}).(string)
			pattern, err := store.SelectPattern(repoPath, trigger, sourceRevision)
			if err != nil {
				contextSourceOmitted(ctx, ContextSourceLearnedPattern, ContextOmittedSourceError)
				return messages
			}
			if pattern == nil {
				contextSourceOmitted(ctx, ContextSourceLearnedPattern, ContextOmittedNotSelected)
				return messages
			}
			content := learning.RenderPattern(pattern)
			contextSourceSelected(ctx, ContextSourceLearnedPattern, 1, len(content), len(content) >= 1000)
			return append(messages, model.Message{Role: model.RoleSystem, Content: content})
		}
	}
}

// setupProjectDocs discovers AGENTS.md/CLAUDE.md/AGENT.md/.cursorrules/
// .github/copilot-instructions.md from root down to the current directory
// (ROADMAP.md §5.4), renders them (summarizing or truncating if they exceed
// projectdocs.TokenBudget), and injects the result into every agent via
// ContextPinsFn — chained after any pin function setupMemory already
// installed, so neither clobbers the other. A background watcher
// re-renders on any candidate file change without requiring an agent
// rebuild, since the injected pins read a mutex-guarded pointer rather than
// a value baked in at startup. Returns nil if there are no instructions
// files to watch, or if the workspace root can't be established.
func setupProjectDocs(ctx context.Context, cfg *config.Config, root string, agents map[string]*agent.Agent) *projectdocs.Watcher {
	cwd, err := os.Getwd()
	if err != nil {
		cwd = root
	}
	bundle, err := projectdocs.Load(root, cwd)
	if err != nil {
		fmt.Printf("warning: load project instructions: %v\n", err)
		return nil
	}
	if bundle.Empty() {
		return nil
	}

	modelID := ""
	if cfg.Defaults != nil {
		modelID = cfg.Defaults.Model.Model
	}
	cachePath := filepath.Join(root, config.ConfigDirName, "projectdocs-cache.json")
	summarize := projectDocsSummarizer(cfg)

	var mu sync.RWMutex
	render := func(b *projectdocs.Bundle) string {
		out, err := projectdocs.Render(ctx, b, modelID, cachePath, summarize)
		if err != nil {
			fmt.Printf("warning: render project instructions: %v\n", err)
			return ""
		}
		return out
	}

	rendered := render(bundle)
	get := func() string {
		mu.RLock()
		defer mu.RUnlock()
		return rendered
	}
	for _, a := range agents {
		prev := a.ContextPinsFn
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var msgs []model.Message
			if prev != nil {
				msgs = append(msgs, prev(ctx)...)
			}
			if text := get(); text != "" {
				contextSourceSelected(ctx, ContextSourceProjectDocs, 1, len(text), strings.Contains(text, "[project instructions truncated:"))
				msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: text})
			} else {
				contextSourceOmitted(ctx, ContextSourceProjectDocs, ContextOmittedNotSelected)
			}
			return msgs
		}
	}

	dirs, err := projectdocs.WatchDirs(root, cwd)
	if err != nil {
		return nil
	}
	watcher, err := projectdocs.Watch(ctx, dirs, func() {
		b, err := projectdocs.Load(root, cwd)
		if err != nil {
			return
		}
		out := render(b)
		mu.Lock()
		rendered = out
		mu.Unlock()
	})
	if err != nil {
		fmt.Printf("warning: watch project instructions: %v\n", err)
		return nil
	}
	return watcher
}

// projectDocsSummarizer builds a projectdocs.Summarizer from cfg.Router's
// model (the same cheap/fast model config setupRouter uses for T1
// classification) so an over-budget instructions bundle gets condensed
// instead of hard-truncated. Returns nil (meaning "hard-truncate instead")
// if no router model is configured or it fails to build.
func projectDocsSummarizer(cfg *config.Config) projectdocs.Summarizer {
	if cfg.Router.Model.Provider == "" {
		return nil
	}
	provider, err := agent.BuildProvider(cfg.Router.Model)
	if err != nil {
		return nil
	}
	modelID := cfg.Router.Model.Model
	return func(ctx context.Context, text string) (string, error) {
		resp, err := provider.Chat(ctx, &model.ChatRequest{
			Model:     modelID,
			MaxTokens: 4000,
			Messages: []model.Message{
				{Role: model.RoleSystem, Content: "Condense the following project instructions to fit a much smaller token budget, preserving every concrete rule, convention, and constraint. Drop prose and examples, not obligations."},
				{Role: model.RoleUser, Content: text},
			},
		})
		if err != nil {
			return "", fmt.Errorf("summarize project instructions: %w", err)
		}
		return resp.Content, nil
	}
}

// setupSkills discovers the merged native/provider skill catalog and, for
// every agent, chains in a
// ContextPinsFn that BM25-selects the top-K most relevant skills for the
// current user message and injects only those — never the whole catalog,
// which is the "load everything into context" anti-pattern §5.1 explicitly
// rejects. It reads the same messageKey{} context value setupMemory's
// message-aware branch already relies on, so it must run after (or in any
// order relative to) setupMemory — both only read that key at call time,
// never at setup time. A missing/unreadable bundled catalog is logged and
// skipped, not fatal: an agent with no skills selected behaves exactly as
// it did before this feature existed.
func setupSkills(cfg *config.Config, root string, agents map[string]*agent.Agent) []*skills.Skill {
	bundledData, err := defaults.ReadFile("skills/default-skills.yaml")
	if err != nil {
		fmt.Printf("warning: read bundled skill catalog: %v\n", err)
		return nil
	}
	bundled, err := skills.LoadBundledYAML(bundledData)
	if err != nil {
		fmt.Printf("warning: parse bundled skill catalog: %v\n", err)
		return nil
	}
	catalog, err := skills.Discover(root, bundled)
	if err != nil {
		fmt.Printf("warning: discover skills: %v\n", err)
		return nil
	}
	if len(catalog) == 0 {
		return nil
	}

	modelID := ""
	if cfg.Defaults != nil {
		modelID = cfg.Defaults.Model.Model
	}

	for _, a := range agents {
		prev := a.ContextPinsFn
		history := newSkillToolHistory(a.ID)
		a.Hooks = append(a.Hooks, history)
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var msgs []model.Message
			if prev != nil {
				msgs = append(msgs, prev(ctx)...)
			}
			msg, _ := ctx.Value(messageKey{}).(string)
			if msg == "" {
				contextSourceOmitted(ctx, ContextSourceSkills, ContextOmittedNotSelected)
				return msgs
			}
			if selected, _ := ctx.Value(explicitSkillKey{}).(*skills.Skill); selected != nil {
				content := skills.Render([]*skills.Skill{selected})
				contextSourceSelected(ctx, ContextSourceSkills, 1, len(content), false)
				msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: content})
				return msgs
			}
			query := history.query(ctx, msg)
			selected := skills.Select(query, catalog, skills.DefaultTopK, modelID)
			if rendered := skills.Render(selected); rendered != "" {
				contextSourceSelected(ctx, ContextSourceSkills, len(selected), len(rendered), false)
				msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: rendered})
			} else {
				contextSourceOmitted(ctx, ContextSourceSkills, ContextOmittedNotSelected)
			}
			return msgs
		}
	}
	return catalog
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
func setupRouter(cfg *config.Config, projectDir string, defaultAgent string) (*router.Router, *router.Config) {
	if !cfg.Router.Enabled {
		return nil, nil
	}
	data, err := readOverridableFile(projectDir, "routing.yaml", "routing.yaml")
	if err != nil {
		fmt.Printf("warning: load routing.yaml: %v\n", err)
		return nil, nil
	}
	rcfg, err := router.Parse(data)
	if err != nil {
		fmt.Printf("warning: parse routing.yaml: %v\n", err)
		return nil, nil
	}
	if defaultAgent == "" {
		defaultAgent = DefaultPrimaryAgentID
	}
	rt, err := router.New(rcfg, defaultAgent)
	if err != nil {
		fmt.Printf("warning: build router: %v\n", err)
		return nil, nil
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
	return rt, rcfg
}

// setupGuardrails loads the guardrail YAML config (project override at
// <projectDir>/guardrails/default.yaml, falling back to the embedded
// default), converts it into chronos guardrails.Rule values, and registers
// them on every agent's Guardrails engine (PRD P2-003) — which chronos's
// sdk/agent already invokes automatically via CheckInput/CheckOutput on
// every Chat/ChatWithSession call. Returns the parsed config (never nil; a
// zero-value *guardrail.Config on failure) so callers can still read its
// TokenBudget for the budget tracker.
func setupGuardrails(cfg *config.Config, projectDir string, agents map[string]*agent.Agent, secretPatterns []string) *guardrail.Config {
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

	rules, err := guardrail.BuildRules(grCfg, secretPatterns)
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

// setupSecurity resolves the embedded security floor with optional user and
// project overlays, then attaches the effective guard to every agent. Invalid
// or weakening overlays fail startup rather than dropping to an empty policy.
func setupSecurity(projectDir, userDir, root string, store storage.Storage, agents map[string]*agent.Agent) (*security.Policy, error) {
	floor, err := defaults.ReadFile("security.yaml")
	if err != nil {
		return nil, fmt.Errorf("read embedded security floor: %w", err)
	}
	overlays := make([]security.Overlay, 0, 2)
	for _, candidate := range []struct {
		source string
		dir    string
	}{
		{source: "user", dir: userDir},
		{source: "project", dir: projectDir},
	} {
		if candidate.dir == "" {
			continue
		}
		data, readErr := os.ReadFile(filepath.Join(candidate.dir, "security.yaml"))
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			return nil, fmt.Errorf("read %s security overlay: %w", candidate.source, readErr)
		}
		overlays = append(overlays, security.Overlay{Source: candidate.source, Data: data})
	}
	policy, err := security.ResolvePolicy(floor, overlays...)
	if err != nil {
		return nil, err
	}
	guard := security.NewGuard(policy, root, store)
	for _, a := range agents {
		a.Hooks = append(a.Hooks, guard)
	}
	return policy, nil
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

// Execute prepares and runs one task through the selected agent. It is the
// shared path for blocking and streaming execution.
func (o *Orchestrator) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	intent, hasIntent, err := memory.ParseIntent(request.Message)
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("parse memory intent: %w", err)
	}
	if request.VerificationMode == "" {
		request.VerificationMode = o.VerificationMode()
	}
	agentID := request.RequestedAgent
	var ppdDecision *router.PPDDecision
	var routeIntent, routeSpecialist string
	var routeMatched bool
	var classification router.Classification
	if agentID == "" {
		agentID = o.active
		classification = router.ClassifyTask(request.Message)
		o.recordRoute(classification, agentID)
		if o.router != nil {
			routeIntent, routeSpecialist, routeMatched = o.router.ClassifyWithFallback(ctx, request.Message)
			if _, ok := o.agents[routeSpecialist]; !ok {
				routeMatched = false
				routeSpecialist = ""
			}
			o.applyResolvedModel(ctx, agentID, request.Message)
		}
		if o.routingConfig != nil {
			ppdRequest := router.PPDRequest{}
			if request.PPD != nil {
				ppdRequest = *request.PPD
			}
			ppdRequest.Kind = classification.Kind
			ppdRequest.HighRisk = ppdRequest.HighRisk || classification.Complexity == router.ComplexityHigh
			decision := router.NewPPDPolicy(o.routingConfig.PPD, nil).Decide(ppdRequest)
			ppdDecision = &decision
			if decision.Action == router.PPDActionDelegate {
				agentID = decision.Specialist
			}
		}
	}
	a, ok := o.agents[agentID]
	if !ok || a == nil {
		if ppdDecision != nil && ppdDecision.Action == router.PPDActionDelegate {
			return ExecutionResult{AgentID: agentID, PPDDecision: ppdDecision}, fmt.Errorf("PPD specialist %q not found", agentID)
		}
		return ExecutionResult{}, fmt.Errorf("agent %q not found", agentID)
	}
	sessionID := request.SessionID
	if sessionID == "" {
		sessionID = o.sessionID(agentID)
	}
	taskID := request.TaskID
	if taskID == "" {
		taskID = session.NewSessionID()
	}
	ctx = context.WithValue(ctx, taskIDKey{}, taskID)
	if request.PolicyContext != nil {
		ctx = context.WithValue(ctx, executionPolicyContextKey{}, request.PolicyContext)
	}
	result := ExecutionResult{AgentID: agentID, SessionID: sessionID, TaskID: taskID, PPDDecision: ppdDecision}
	collector := newContextReportCollector()
	ctx = withContextReportCollector(ctx, collector)
	result.ContextReport = o.contextReport(collector)
	if hasIntent {
		result.MemoryIntent, err = o.applyMemoryIntent(ctx, intent)
		if err != nil {
			contextSourceOmitted(ctx, ContextSourceMemory, ContextOmittedSourceError)
			result.ContextReport = o.contextReport(collector)
			return result, err
		}
		if result.MemoryIntent.Applied {
			contextSourceSelected(ctx, ContextSourceMemory, 1, 0, false)
		} else {
			contextSourceOmitted(ctx, ContextSourceMemory, result.MemoryIntent.Reason)
		}
	}
	ctx, message, err := o.preparePrompt(ctx, request.Message, agentID, sessionID)
	if err != nil {
		result.ContextReport = o.contextReport(collector)
		return result, err
	}
	if !hasIntent {
		if hint := formatRoutingHint(agentID, routeIntent, routeSpecialist, routeMatched, classification, o.implementationPath(classification.Complexity), ppdDecision); hint != "" {
			message = hint + "\n\n" + message
		}
	}
	if hasIntent && intent.Action == memory.IntentRecallPast && result.MemoryIntent.Applied {
		ctx = context.WithValue(ctx, messageKey{}, intent.Payload)
	}
	// Predictive context is part of preparation, not a blocking-only feature.
	if o.graphStore != nil && o.actBuf != nil {
		if preloaded := activation.PredictiveContext(ctx, o.graphStore, o.actBuf, message); preloaded != "" {
			contextSourceSelected(ctx, ContextSourceGraphPrediction, strings.Count(preloaded, "\n"), len(preloaded), false)
			message += "\n\n" + preloaded
		} else {
			contextSourceOmitted(ctx, ContextSourceGraphPrediction, ContextOmittedNotSelected)
		}
	}
	if request.Mode == ExecutionStreaming {
		result.Stream, err = o.executeStreamWithRecovery(ctx, a, sessionID, agentID, message)
		if err == nil {
			result.Stream = assessStream(ctx, result.Stream, request)
		}
		result.ContextReport = o.contextReport(collector)
		return result, err
	}
	result.Response, err = o.executeBlockingWithRecovery(ctx, a, sessionID, agentID, message)
	result.ContextReport = o.contextReport(collector)
	if err != nil {
		return result, err
	}
	decision := verification.Assess(request.VerificationMode, true, request.VerificationObligations, request.VerificationEvents)
	if !decision.Allowed {
		return result, fmt.Errorf("verification does not support successful completion")
	}
	return result, nil
}

func (o *Orchestrator) contextReport(collector *contextReportCollector) ContextReport {
	if o.cfg != nil && !o.cfg.Session.ContextReportEnabled() {
		return ContextReport{}
	}
	return collector.report()
}

const (
	maxAPIRetries     = 2
	maxCompactRetries = 1
)

func (o *Orchestrator) executeStreamWithRecovery(ctx context.Context, a *agent.Agent, sessionID, agentID, message string) (<-chan *model.ChatResponse, error) {
	chatStream := func() (<-chan *model.ChatResponse, error) {
		if sessionID != "" && a.Storage != nil {
			return a.ChatStreamWithSession(ctx, sessionID, message)
		}
		return a.ChatStream(ctx, message)
	}

	stream, err := chatStream()
	if err == nil {
		return stream, nil
	}

	classified := apierror.Classify(err)

	if apierror.IsCompactable(classified) {
		o.publishRetryEvent(ctx, agentID, classified.Message)
		if compactErr := o.CompactActiveSession(ctx); compactErr == nil {
			stream, err = chatStream()
			if err == nil {
				return stream, nil
			}
			classified = apierror.Classify(err)
		}
	}

	if classified.Retryable {
		for attempt := 1; attempt <= maxAPIRetries; attempt++ {
			delay := classified.RetryAfter
			if delay <= 0 {
				delay = time.Duration(attempt) * 5 * time.Second
			}
			o.publishRetryEvent(ctx, agentID, fmt.Sprintf("%s (retry %d/%d in %s)", classified.Message, attempt, maxAPIRetries, delay.Round(time.Second)))
			if sleepErr := sleepContext(ctx, delay); sleepErr != nil {
				return nil, err
			}
			stream, err = chatStream()
			if err == nil {
				return stream, nil
			}
			classified = apierror.Classify(err)
			if !classified.Retryable {
				break
			}
		}
	}

	return nil, classified
}

func (o *Orchestrator) executeBlockingWithRecovery(ctx context.Context, a *agent.Agent, sessionID, agentID, message string) (*model.ChatResponse, error) {
	chat := func() (*model.ChatResponse, error) {
		if sessionID != "" && a.Storage != nil {
			return a.ChatWithSession(ctx, sessionID, message)
		}
		return a.Chat(ctx, message)
	}

	resp, err := chat()
	if err == nil {
		return resp, nil
	}

	classified := apierror.Classify(err)

	if apierror.IsCompactable(classified) {
		o.publishRetryEvent(ctx, agentID, classified.Message)
		if compactErr := o.CompactActiveSession(ctx); compactErr == nil {
			resp, err = chat()
			if err == nil {
				return resp, nil
			}
			classified = apierror.Classify(err)
		}
	}

	if classified.Retryable {
		for attempt := 1; attempt <= maxAPIRetries; attempt++ {
			delay := classified.RetryAfter
			if delay <= 0 {
				delay = time.Duration(attempt) * 5 * time.Second
			}
			o.publishRetryEvent(ctx, agentID, fmt.Sprintf("%s (retry %d/%d in %s)", classified.Message, attempt, maxAPIRetries, delay.Round(time.Second)))
			if sleepErr := sleepContext(ctx, delay); sleepErr != nil {
				return nil, err
			}
			resp, err = chat()
			if err == nil {
				return resp, nil
			}
			classified = apierror.Classify(err)
			if !classified.Retryable {
				break
			}
		}
	}

	return nil, classified
}

func (o *Orchestrator) publishRetryEvent(ctx context.Context, agentID, message string) {
	if o.broker == nil {
		return
	}
	o.broker.PublishTopic(o.CurrentSessionID(), chronosstream.Event{
		Type: chronosstream.EventCustom,
		Data: map[string]any{
			"agent":   agentID,
			"type":    "api_retry",
			"message": message,
		},
	})
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (o *Orchestrator) applyMemoryIntent(ctx context.Context, intent memory.Intent) (*memory.IntentResult, error) {
	result := &memory.IntentResult{Action: intent.Action, Category: intent.Category, RecordID: intent.RecordID}
	if o.cfg == nil || !o.cfg.Memory.AutoExtract {
		result.Reason = "auto_extract_disabled"
		return result, nil
	}
	if o.memory == nil {
		result.Reason = "memory_disabled"
		return result, nil
	}

	tenantStore := o.memory.ForContext(ctx)
	switch intent.Action {
	case memory.IntentRemember:
		record, err := tenantStore.Add(intent.Category, intent.Payload)
		if err != nil {
			return result, fmt.Errorf("apply remember intent: %w", err)
		}
		result.RecordID = record.ID
	case memory.IntentForget:
		if err := tenantStore.Forget(intent.RecordID); err != nil {
			return result, fmt.Errorf("apply forget intent for %q: %w", intent.RecordID, err)
		}
	case memory.IntentRecallPast:
	default:
		return result, fmt.Errorf("apply memory intent: unsupported action %q", intent.Action)
	}
	result.Applied = true
	return result, nil
}

// VerificationMode returns the configured verification completion policy.
func (o *Orchestrator) VerificationMode() verification.Mode {
	if o.cfg == nil || o.cfg.Verification.Mode == "" {
		return verification.ModeReport
	}
	return o.cfg.Verification.Mode
}

// assessStream preserves the streaming adapter while withholding successful
// completion when its supplied verification evidence is insufficient.
func assessStream(ctx context.Context, stream <-chan *model.ChatResponse, request ExecutionRequest) <-chan *model.ChatResponse {
	assessed := make(chan *model.ChatResponse)
	go func() {
		defer close(assessed)
		for {
			select {
			case <-ctx.Done():
				return
			case response, ok := <-stream:
				if !ok {
					decision := verification.Assess(request.VerificationMode, true, request.VerificationObligations, request.VerificationEvents)
					if !decision.Allowed {
						select {
						case assessed <- &model.ChatResponse{Err: fmt.Errorf("verification does not support successful completion")}:
						case <-ctx.Done():
						}
					}
					return
				}
				select {
				case assessed <- response:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return assessed
}

// Chat preserves the blocking public API while delegating to Execute.
func (o *Orchestrator) Chat(ctx context.Context, message string) (*model.ChatResponse, error) {
	result, err := o.Execute(ctx, ExecutionRequest{Message: message})
	if err != nil {
		return nil, err
	}
	return result.Response, nil
}

// ChatStream preserves the streaming public API while delegating to Execute.
func (o *Orchestrator) ChatStream(ctx context.Context, message string) (<-chan *model.ChatResponse, error) {
	result, err := o.Execute(ctx, ExecutionRequest{Message: message, Mode: ExecutionStreaming})
	if err != nil {
		return nil, err
	}
	return result.Stream, nil
}

func (o *Orchestrator) preparePrompt(ctx context.Context, message, agentID, sessionID string) (context.Context, string, error) {
	ctx = o.turnContextFor(ctx, message, agentID, sessionID)
	if o.PlanMode() {
		message = planModePrompt + "\n\n" + message
	}
	if o.hookRunner == nil || o.cfg == nil || len(o.cfg.Hooks.UserPromptSubmit) == 0 {
		return ctx, message, nil
	}

	var output []string
	vars := map[string]any{
		"user_message": message,
		"session_id":   storage.SessionFromContext(ctx),
		"agent_id":     agentID,
		"task_id":      TaskIDFromContext(ctx),
	}
	for _, hook := range o.cfg.Hooks.UserPromptSubmit {
		result, err := o.hookRunner.Run(ctx, hook, vars)
		if err != nil {
			contextSourceOmitted(ctx, ContextSourceUserHook, ContextOmittedSourceError)
			return ctx, message, fmt.Errorf("user-prompt hook %q: %w", hook.Name, err)
		}
		if text := strings.Join(result.Stdout.Lines, "\n"); text != "" {
			output = append(output, hook.Name+":\n"+text)
		}
	}
	contextBlock := security.Summarize(security.CapturedOutput{Lines: output}, userHookPromptContextTokens)
	if contextBlock != "" {
		contextSourceSelected(ctx, ContextSourceUserHook, len(output), len(contextBlock), len(strings.Join(output, "\n")) > len(contextBlock))
		message += "\n\n<user_hook_context>\n" + contextBlock + "\n</user_hook_context>"
	} else {
		contextSourceOmitted(ctx, ContextSourceUserHook, ContextOmittedNotSelected)
	}
	return ctx, message, nil
}

// turnContext gives every execution path the same per-turn inputs for dynamic
// context pins and session-scoped runtime features.
func (o *Orchestrator) turnContext(ctx context.Context, message string) context.Context {
	return o.turnContextFor(ctx, message, o.active, o.CurrentSessionID())
}

func (o *Orchestrator) turnContextFor(ctx context.Context, message, agentID, sessionID string) context.Context {
	ctx = context.WithValue(ctx, messageKey{}, message)
	maxModelCalls := 0
	if o.cfg != nil {
		maxModelCalls = o.cfg.Session.MaxModelCallsPerTurn
	}
	ctx = withSubagentTurnState(ctx, maxModelCalls, agentID)
	if sessionID != "" {
		ctx = storage.WithSession(ctx, sessionID)
	}
	return ctx
}

func selectPrimaryAgent(agents map[string]*agent.Agent, order []string) string {
	for _, id := range []string{DefaultPrimaryAgentID, "coder"} {
		if _, ok := agents[id]; ok {
			return id
		}
	}
	if len(order) > 0 {
		return order[0]
	}
	return ""
}

func (o *Orchestrator) implementationPath(complexity router.Complexity) router.ImplementationPath {
	if o.routingConfig != nil {
		return o.routingConfig.PathFor(complexity)
	}
	return router.DefaultPath(complexity)
}

func formatRoutingHint(agentID, intent, specialist string, matched bool, class router.Classification, path router.ImplementationPath, ppd *router.PPDDecision) string {
	if ppd != nil && ppd.Action == router.PPDActionDelegate {
		return ""
	}
	if path.Hint == "" {
		path = router.DefaultPath(class.Complexity)
	}
	parts := []string{fmt.Sprintf("Path: complexity=%s kind=%s graph=%s plan=%s max_tools=%d. %s. Graph before files before shell. Recall-past and learned patterns before repeating a search.", class.Complexity, class.Kind, path.Graph, path.Plan, path.MaxToolCalls, path.Hint)}
	if class.Complexity == "" {
		parts = parts[:0]
	}
	if matched && specialist != "" && specialist != agentID {
		parts = append(parts, fmt.Sprintf("Routing hint: intent=%s specialist=%s. You remain %s; spawn_subagent %s if that specialist loop is needed.", intent, specialist, agentID, specialist))
	}
	if ppd != nil && ppd.Action == router.PPDActionShadow && ppd.Specialist != "" && ppd.Specialist != agentID {
		parts = append(parts, "PPD: this looks like multi-step or cross-package work. spawn_subagent "+ppd.Specialist+" before implementing.")
	}
	return strings.Join(parts, "\n")
}

// Route classifies message via the T0 intent router, falling back to the T1
// cheap-model classifier when configured (PRD P2-006), and returns the
// specialist the classifier would pick. Execute keeps the active Chronos
// Code conversation unless the caller requested an agent or PPD delegates.
// Model routing is applied to the classified agent for Route callers and
// tests; Execute applies it to the agent that actually runs.
func (o *Orchestrator) Route(ctx context.Context, message string) (agentID string, matched bool) {
	if o.router == nil {
		return o.active, false
	}
	_, agentID, matched = o.router.ClassifyWithFallback(ctx, message)
	if _, ok := o.agents[agentID]; !ok {
		return o.active, false
	}
	if !matched {
		agentID = o.active
	}
	o.applyResolvedModel(ctx, agentID, message)
	return agentID, matched
}

func (o *Orchestrator) applyResolvedModel(ctx context.Context, agentID, message string) {
	classification := router.ClassifyTask(message)
	o.routingMu.Lock()
	defer o.routingMu.Unlock()
	if o.routingConfig == nil || o.modelOverrides[agentID] {
		return
	}
	delete(o.routingState, agentID)
	spec, ok := o.routingConfig.ResolveModel(classification.Complexity, classification.Kind)
	if !ok {
		return
	}
	if err := o.switchModel(ctx, agentID, spec.Provider, spec.Model); err == nil {
		o.routingState[agentID] = classification
	}
}

type modelEscalationHook struct {
	orchestrator *Orchestrator
	agentID      string
}

func (h modelEscalationHook) Before(context.Context, *hooks.Event) error { return nil }

func (h modelEscalationHook) After(ctx context.Context, evt *hooks.Event) error {
	if evt.Type == hooks.EventToolCallAfter && evt.Error != nil {
		_ = h.orchestrator.escalateModel(ctx, h.agentID)
	}
	return nil
}

func (o *Orchestrator) escalateModel(ctx context.Context, agentID string) error {
	o.routingMu.Lock()
	defer o.routingMu.Unlock()
	if o.routingConfig == nil || o.modelOverrides[agentID] {
		return nil
	}
	classification, ok := o.routingState[agentID]
	if !ok || classification.Complexity == router.ComplexityHigh {
		return nil
	}
	next := router.ComplexityMedium
	if classification.Complexity == router.ComplexityMedium {
		next = router.ComplexityHigh
	}
	spec, ok := o.routingConfig.ResolveModel(next, classification.Kind)
	if !ok {
		return nil
	}
	if err := o.switchModel(ctx, agentID, spec.Provider, spec.Model); err != nil {
		return err
	}
	classification.Complexity = next
	o.routingState[agentID] = classification
	return nil
}

// SetApprovalHandler installs handler behind the policy checker on every agent
// registry. Replacing the human handler never replaces policy enforcement.
func (o *Orchestrator) SetApprovalHandler(handler tool.ApprovalFunc) {
	for _, a := range o.agents {
		a.Tools.SetApprovalHandler(func(ctx context.Context, toolName string, args map[string]any) (bool, error) {
			switch o.permissionChecker.Check(toolName, args, o.permissionYolo.Load()) {
			case security.Auto:
				return true, nil
			case security.Deny:
				o.auditPermissionDenial(ctx, toolName, args)
				return false, nil
			case security.Confirm:
				if handler == nil {
					return false, nil
				}
				return handler(ctx, toolName, args)
			default:
				return false, nil
			}
		})
	}
}

// normalizeToolPermissions ensures every executable tool call reaches the
// policy-aware approval handler. It runs once at startup after registration.
func normalizeToolPermissions(agents map[string]*agent.Agent) {
	for _, a := range agents {
		for _, def := range a.Tools.List() {
			if def.Permission == tool.PermDeny {
				continue
			}
			normalized := *def
			normalized.Permission = tool.PermRequireApproval
			a.Tools.Register(&normalized)
		}
	}
}

func (o *Orchestrator) auditPermissionDenial(ctx context.Context, toolName string, args map[string]any) {
	if o.store == nil {
		return
	}
	_ = o.store.AppendAuditLog(ctx, &storage.AuditLog{
		ID:        session.NewSessionID(),
		SessionID: storage.SessionFromContext(ctx),
		Actor:     "permission-policy",
		Action:    "block",
		Resource:  toolName,
		Detail:    map[string]any{"args": args},
		CreatedAt: time.Now(),
	})
}

// SetPermissionMode applies mode to every agent registry. Auto-approve is
// implemented by the policy checker while registries remain in prompt mode,
// so hard denials and explicit confirmations cannot bypass the checker.
func (o *Orchestrator) SetPermissionMode(mode string) error {
	if mode == "" {
		return nil
	}
	parsed, err := tool.ParsePermissionMode(mode)
	if err != nil {
		return err
	}
	registryMode := parsed
	yolo := false
	if parsed == tool.PermissionModeAutoApprove {
		registryMode = tool.PermissionModePrompt
		yolo = true
	}
	o.permissionYolo.Store(yolo)
	for _, a := range o.agents {
		if err := a.Tools.SetPermissionMode(registryMode); err != nil {
			return fmt.Errorf("set permission mode for agent %q: %w", a.ID, err)
		}
	}
	return nil
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

// ActiveModelInfo returns the active agent's current provider name and
// model ID (model.Provider.Name()/Model()), for TUI display (e.g. the
// header bar and /model command) — it reflects whatever SwitchModel last
// set, not just the YAML the agent was originally built from.
func (o *Orchestrator) ActiveModelInfo() (provider, modelID string) {
	a := o.ActiveAgent()
	if a == nil || a.Model == nil {
		return "", ""
	}
	return a.Model.Name(), a.Model.Model()
}

// SwitchModel rebuilds the active agent's model provider against
// (provider, modelID), resolving credentials through auth.Resolve's full
// precedence chain (ROADMAP.md §5.3) exactly as applyStoredCredentials does
// at startup — so switching models in a running TUI session picks up
// env vars, chronos-code's own login, or a reused Claude Code/Codex
// credential the same way the initial build did. It takes effect
// immediately and is treated as an explicit per-agent override of automatic
// model routing; there is no need to restart or rebuild the Orchestrator.
func (o *Orchestrator) SwitchModel(ctx context.Context, provider, modelID string) error {
	o.routingMu.Lock()
	defer o.routingMu.Unlock()
	if err := o.switchModel(ctx, o.active, provider, modelID); err != nil {
		return err
	}
	if o.modelOverrides == nil {
		o.modelOverrides = make(map[string]bool)
	}
	o.modelOverrides[o.active] = true
	return nil
}

func (o *Orchestrator) switchModel(ctx context.Context, agentID, provider, modelID string) error {
	a := o.agents[agentID]
	if a == nil {
		return fmt.Errorf("no active agent")
	}
	mc := agent.ModelConfig{Provider: provider, Model: modelID}
	if key := auth.Resolve(ctx, auth.NewStore(), provider).Token; key != "" {
		mc.APIKey = key
	}
	if o.cfg != nil {
		if override, ok := o.cfg.Providers[provider]; ok && override.BaseURL != "" {
			mc.BaseURL = override.BaseURL
		}
	}
	buildProvider := o.buildProvider
	if buildProvider == nil {
		buildProvider = agent.BuildProvider
	}
	p, err := buildProvider(mc)
	if err != nil {
		return fmt.Errorf("build provider for %s/%s: %w", provider, modelID, err)
	}
	a.Model = p
	return nil
}

func thinkingBudgetForEffort(effort string) int {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return 1024
	case "high":
		return 10000
	default:
		return 4096
	}
}

func nativeThinkingUnsupported(a *agent.Agent) bool {
	if a == nil || a.Model == nil {
		return false
	}
	switch strings.ToLower(a.Model.Name()) {
	case "gemini", "google":
		return a.Tools != nil && len(a.Tools.List()) > 0
	default:
		return false
	}
}

// ThinkingLevel reports the active agent's native thinking effort
// ("off", "low", "medium", or "high").
func (o *Orchestrator) ThinkingLevel() string {
	o.routingMu.Lock()
	defer o.routingMu.Unlock()
	a := o.ActiveAgent()
	if a == nil || !a.ReasoningConfig.Enabled {
		return "off"
	}
	effort := strings.ToLower(strings.TrimSpace(a.ReasoningConfig.Effort))
	switch effort {
	case "low", "medium", "high":
		return effort
	default:
		return "medium"
	}
}

// SetThinking enables or disables provider-native thinking on every loaded
// agent. level is off, low, medium, or high. Gemini agents that have tools
// are skipped because Chronos cannot yet preserve their signed thought
// blocks across tool rounds.
func (o *Orchestrator) SetThinking(level string) error {
	o.routingMu.Lock()
	defer o.routingMu.Unlock()
	level = strings.ToLower(strings.TrimSpace(level))
	var cfg model.ReasoningConfig
	switch level {
	case "off", "none", "false", "0":
	case "low", "medium", "high":
		cfg = model.ReasoningConfig{
			Enabled:      true,
			Effort:       level,
			BudgetTokens: thinkingBudgetForEffort(level),
			Summary:      true,
		}
	default:
		return fmt.Errorf("thinking level %q is invalid (want off, low, medium, or high)", level)
	}

	skipped := 0
	for _, a := range o.agents {
		if a == nil {
			continue
		}
		if cfg.Enabled && nativeThinkingUnsupported(a) {
			skipped++
			continue
		}
		a.ReasoningConfig = cfg
	}
	if cfg.Enabled && skipped > 0 && skipped == len(o.agents) {
		return fmt.Errorf("native thinking with tools is not supported for the loaded gemini agents")
	}
	return nil
}

// Login stores an API-key credential for provider (ROADMAP.md §5.3's
// simplest, always-available auth path) and, if provider matches the
// active agent's current provider, immediately rebuilds its model provider
// so the new credential takes effect without restarting the session.
func (o *Orchestrator) Login(ctx context.Context, provider, apiKey string) error {
	if err := auth.LoginAPIKey(auth.NewStore(), provider, apiKey); err != nil {
		return err
	}
	if activeProvider, modelID := o.ActiveModelInfo(); activeProvider == provider && modelID != "" {
		o.routingMu.Lock()
		defer o.routingMu.Unlock()
		return o.switchModel(ctx, o.active, provider, modelID)
	}
	return nil
}

// LoginOAuth runs a bring-your-own-IdP OAuth Authorization Code + PKCE flow
// (internal/auth.LoginPKCE). chronos-code has no built-in "Sign in with
// Anthropic/OpenAI" client of its own — every endpoint and client_id must
// be supplied by the caller, so this only works for an enterprise's own IdP
// app registration or one the user has already set up. onPromptURL is
// called with the authorization URL to show the user (e.g. print it in the
// TUI); LoginOAuth also always attempts to open it in the system browser.
func (o *Orchestrator) LoginOAuth(ctx context.Context, cfg auth.ProviderOAuthConfig, onPromptURL func(string)) error {
	return auth.LoginPKCE(ctx, auth.NewStore(), cfg, onPromptURL)
}

// Logout removes provider's stored chronos-code credential. It has no
// effect on a credential reused from ~/.claude or ~/.codex (those belong to
// the other CLI, not chronos-code) or on env-var-based auth.
func (o *Orchestrator) Logout(provider string) error {
	return auth.Logout(auth.NewStore(), provider)
}

// ExternalLogin identifies a provider whose credential chronos-code is
// reusing from an already-installed Claude Code / Codex CLI login, rather
// than one it obtained itself.
type ExternalLogin struct {
	Provider string
	Label    string
}

// DetectedExternalLogins reports which providers have a reusable existing
// Claude Code / Codex CLI login on this machine (the "~/.claude" /
// "~/.codex" reuse link of the ROADMAP.md §5.3 precedence chain) — e.g. so
// /login's interactive picker only offers "use my existing login" when
// there's actually one to use, and never a brand-new sign-in flow this
// tool has no OAuth client of its own to perform.
func (o *Orchestrator) DetectedExternalLogins() []ExternalLogin {
	var found []ExternalLogin
	if _, err := auth.LoadClaudeCodeCredential(); err == nil {
		found = append(found, ExternalLogin{Provider: "anthropic", Label: "Claude Code / Claude Enterprise (existing login)"})
	}
	if _, err := auth.LoadCodexCredential(); err == nil {
		found = append(found, ExternalLogin{Provider: "openai", Label: "Codex CLI (existing login)"})
	}
	return found
}

// AuthorizedProviders returns the subset of candidates that currently
// resolve to a non-empty credential (env var, chronos-code's own login, or
// an external CLI reuse) via auth.Resolve. It makes no network calls.
// Keychain lookups are memoized by auth.Store for the process lifetime
// (invalidated on login/logout), so repeating this for the TUI status bar
// or /model completions stays cheap after the first scan.
func (o *Orchestrator) AuthorizedProviders(ctx context.Context, candidates []string) []string {
	store := auth.NewStore()
	var out []string
	for _, p := range candidates {
		if auth.Resolve(ctx, store, p).Token != "" {
			out = append(out, p)
		}
	}
	return out
}

// ListActiveProviderModels attempts to fetch a live model list for the
// active agent's provider using its resolved credential
// (modelinfo.FetchLive) — real model IDs from the vendor's own API, not a
// hardcoded list. It reports ok=false (never an error the caller must
// handle) when the provider has no supported live-listing endpoint, no
// credential is currently resolvable, or the request fails for any reason
// (network, timeout, auth); callers should fall back to modelinfo.All() in
// that case, which is the pre-existing, always-available static registry.
func (o *Orchestrator) ListActiveProviderModels(ctx context.Context) (models []modelinfo.Info, ok bool) {
	provider, _ := o.ActiveModelInfo()
	if provider == "" {
		return nil, false
	}
	key := auth.Resolve(ctx, auth.NewStore(), provider).Token
	if key == "" {
		return nil, false
	}
	list, err := modelinfo.FetchLive(ctx, provider, key)
	if err != nil {
		return nil, false
	}
	return list, true
}

// AuthStatusLine reports which link of provider's authentication
// precedence chain (auth.Resolve, ROADMAP.md §5.3) is currently effective,
// for TUI display (e.g. /whoami and the header bar) — never the token
// itself.
func (o *Orchestrator) AuthStatusLine(ctx context.Context, provider string) string {
	rc := auth.Resolve(ctx, auth.NewStore(), provider)
	if rc.Source == auth.SourceNone {
		return fmt.Sprintf("%s: not authenticated", provider)
	}
	return fmt.Sprintf("%s: %s (%s)", provider, rc.Source, rc.Method)
}

func (o *Orchestrator) ActiveID() string {
	return o.active
}

func (o *Orchestrator) PrimaryID() string {
	return o.primary
}

func (o *Orchestrator) ListAgents() []string {
	return o.order
}

func (o *Orchestrator) ListSubagents() []string {
	result := make([]string, 0, len(o.order))
	for _, id := range o.order {
		if id != o.active {
			result = append(result, id)
		}
	}
	return result
}

func (o *Orchestrator) MCPStatuses() []mcpdiscover.ServerStatus {
	o.mcpMu.Lock()
	defer o.mcpMu.Unlock()
	byName := make(map[string]mcpdiscover.ServerStatus)
	for _, runtime := range o.mcpRuntimes {
		for _, status := range runtime.Statuses() {
			byName[status.Name] = status
		}
	}
	if o.workspace != nil && o.cfg != nil && o.cfg.MCP.DiscoveryEnabled() {
		for _, cfg := range mcpdiscover.Load(o.workspace.Root).Servers {
			if _, ok := byName[cfg.Name]; ok {
				continue
			}
			byName[cfg.Name] = mcpdiscover.ServerStatus{Name: cfg.Name, State: mcpdiscover.StateApprovalRequired}
		}
	}
	names := make([]string, 0, len(byName))
	for name := range byName {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]mcpdiscover.ServerStatus, 0, len(names))
	for _, name := range names {
		out = append(out, byName[name])
	}
	return out
}

func (o *Orchestrator) ConnectMCP(ctx context.Context, name string) (mcpdiscover.ServerStatus, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return mcpdiscover.ServerStatus{}, fmt.Errorf("MCP server name is required")
	}
	o.mcpMu.Lock()
	defer o.mcpMu.Unlock()
	if o.mcpClosed {
		return mcpdiscover.ServerStatus{}, fmt.Errorf("orchestrator is closed")
	}
	cfg, ok := o.lookupMCPLocked(name)
	if !ok {
		return mcpdiscover.ServerStatus{}, fmt.Errorf("MCP server %q is not configured", name)
	}
	if o.policy == nil {
		return mcpdiscover.ServerStatus{}, fmt.Errorf("security policy is not configured")
	}
	if err := o.policy.AllowMCPServerSession(name); err != nil {
		return mcpdiscover.ServerStatus{}, fmt.Errorf("connect MCP server %q: %w", name, err)
	}
	factory := o.mcpFactory
	if factory == nil {
		factory = mcpdiscover.NewClient
	}
	timeout := o.mcpTimeout
	if timeout <= 0 {
		timeout = mcpdiscover.DefaultConnectTimeout
	}

	agentIDs := make([]string, 0, len(o.agents))
	for id := range o.agents {
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)
	var last mcpdiscover.ServerStatus
	for i, id := range agentIDs {
		a := o.agents[id]
		if a == nil {
			continue
		}
		if i >= len(o.mcpRuntimes) || o.mcpRuntimes[i] == nil {
			return mcpdiscover.ServerStatus{}, fmt.Errorf("MCP runtime for agent %q is not available", id)
		}
		before := registeredToolNameSet(a)
		o.mcpRuntimes[i].RememberServer(cfg)
		last = o.mcpRuntimes[i].ConnectServer(ctx, cfg, a.Tools, o.policy, timeout, factory)
		wrapLateTools(a, before, o)
	}
	if last.Name == "" {
		return mcpdiscover.ServerStatus{}, fmt.Errorf("MCP server %q is not configured", name)
	}
	if last.State != mcpdiscover.StateConnected {
		return last, fmt.Errorf("MCP server %q: %s", name, last.State)
	}
	return last, nil
}

func (o *Orchestrator) lookupMCPLocked(name string) (mcp.ServerConfig, bool) {
	for _, runtime := range o.mcpRuntimes {
		if cfg, ok := runtime.Server(name); ok {
			return cfg, true
		}
	}
	if o.workspace == nil {
		return mcp.ServerConfig{}, false
	}
	for _, cfg := range mcpdiscover.Load(o.workspace.Root).Servers {
		if cfg.Name == name {
			return cfg, true
		}
	}
	return mcp.ServerConfig{}, false
}

func registeredToolNameSet(a *agent.Agent) map[string]struct{} {
	names := make(map[string]struct{})
	if a == nil || a.Tools == nil {
		return names
	}
	for _, def := range a.Tools.List() {
		names[def.Name] = struct{}{}
	}
	return names
}

func wrapLateTools(a *agent.Agent, before map[string]struct{}, o *Orchestrator) {
	if a == nil || a.Tools == nil {
		return
	}
	var pending []*tool.Definition
	for _, def := range a.Tools.List() {
		if _, existed := before[def.Name]; existed || def.Handler == nil {
			continue
		}
		copied := *def
		pending = append(pending, &copied)
	}
	for _, def := range pending {
		orig := def.Handler
		if o != nil && o.hookRunner != nil && o.cfg != nil {
			toolName := def.Name
			configured := o.cfg.Hooks
			runner := o.hookRunner
			hooked := orig
			orig = func(ctx context.Context, args map[string]any) (any, error) {
				vars := map[string]any{
					"tool_name":  toolName,
					"tool_args":  args,
					"session_id": storage.SessionFromContext(ctx),
					"agent_id":   a.ID,
				}
				for _, hook := range configured.PreToolCall {
					if _, err := runner.Run(ctx, hook, vars); err != nil {
						return nil, fmt.Errorf("pre-tool hook %q: %w", hook.Name, err)
					}
				}
				result, handlerErr := hooked(ctx, args)
				vars["tool_output"] = result
				for _, hook := range configured.PostToolCall {
					_, _ = runner.Run(ctx, hook, vars)
				}
				return result, handlerErr
			}
		}
		wrapped := *def
		if wrapped.Permission != tool.PermDeny {
			wrapped.Permission = tool.PermRequireApproval
		}
		wrapped.Handler = func(ctx context.Context, args map[string]any) (any, error) {
			result, err := orig(ctx, args)
			if err != nil || result == nil {
				return result, err
			}
			return capResult(result), nil
		}
		a.Tools.Register(&wrapped)
	}
}

func (o *Orchestrator) ListSkills() []SkillInfo {
	result := make([]SkillInfo, len(o.skillCatalog))
	for i, skill := range o.skillCatalog {
		result[i] = SkillInfo{Name: skill.Name, Description: skill.Description, Source: skill.Source}
	}
	sort.Slice(result, func(i, j int) bool {
		return strings.ToLower(result[i].Name) < strings.ToLower(result[j].Name)
	})
	return result
}

// WithSkill pins one named skill for the next turn, bypassing automatic
// relevance selection while retaining the same isolated system context.
func (o *Orchestrator) WithSkill(ctx context.Context, name string) (context.Context, error) {
	for _, skill := range o.skillCatalog {
		if strings.EqualFold(skill.Name, name) {
			return context.WithValue(ctx, explicitSkillKey{}, skill), nil
		}
	}
	return ctx, fmt.Errorf("skill %q not found", name)
}

// RunSubagent invokes the active agent's spawn_subagent tool directly. The
// supplied arguments use the tool's public schema: task plus either agent, or
// system_prompt and an optional tools list for a dynamic subagent.
func (o *Orchestrator) RunSubagent(ctx context.Context, args map[string]any) (string, error) {
	active := o.ActiveAgent()
	if active == nil {
		return "", fmt.Errorf("no active agent")
	}
	task, _ := args["task"].(string)
	ctx = o.turnContext(ctx, task)
	result, err := active.Tools.Execute(ctx, harness.SpawnToolName, args)
	if err != nil {
		return "", err
	}
	resultMap, ok := result.(map[string]any)
	if !ok {
		return "", fmt.Errorf("spawn_subagent returned %T, want result object", result)
	}
	text, ok := resultMap["result"].(string)
	if !ok {
		return "", fmt.Errorf("spawn_subagent result is not text")
	}
	return text, nil
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
	return o.sessionID(o.active)
}

func (o *Orchestrator) sessionID(agentID string) string {
	o.sessionMu.RLock()
	defer o.sessionMu.RUnlock()
	return o.sessions[agentID]
}

// ResetSession starts a fresh conversation context for the active agent. The
// previous session remains persisted and available through session history.
func (o *Orchestrator) ResetSession(ctx context.Context) (string, error) {
	agentID := o.active
	if _, ok := o.agents[agentID]; !ok {
		return "", fmt.Errorf("no active agent")
	}
	sessionID := session.NewSessionID()
	if err := o.sessionMgr.Ensure(ctx, sessionID, agentID); err != nil {
		return "", fmt.Errorf("create replacement session: %w", err)
	}
	o.sessionMu.Lock()
	o.sessions[agentID] = sessionID
	o.sessionMu.Unlock()
	return sessionID, nil
}

// CompactActiveSession recovers from a cumulative token-budget cap (PRD
// P2-009) without discarding the active agent's conversation: it forces a
// summarization pass over the current session's history (agent.Agent's
// CompactSession, which — unlike ChatWithSession's inline compaction —
// summarizes unconditionally rather than waiting for the conversation to
// near the model's context window, since a budget cap is a cost concern
// wholly unrelated to context size) and then clears the session's
// accumulated usage in the budget tracker so it can keep making model calls
// under the same session ID and history. Callers should fall back to
// ResetSession if this returns an error.
func (o *Orchestrator) CompactActiveSession(ctx context.Context) error {
	agentID := o.active
	a, ok := o.agents[agentID]
	if !ok {
		return fmt.Errorf("no active agent")
	}
	sessionID := o.CurrentSessionID()
	if sessionID == "" {
		return fmt.Errorf("no active session")
	}
	if err := a.CompactSession(ctx, sessionID); err != nil {
		return fmt.Errorf("compact session: %w", err)
	}
	if o.budget != nil {
		o.budget.ResetSession(sessionID)
	}
	return nil
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

// SetUSDCap configures the per-session model cost cap in microdollars. A cap
// less than or equal to zero is unlimited. Configure it before issuing calls.
func (o *Orchestrator) SetUSDCap(cap budget.Microdollars) {
	o.budgetMu.Lock()
	o.usdBudget = budget.NewTrackerWithUSDCap(0, 0, cap)
	o.budgetMu.Unlock()
}

// SessionCost returns model usage and USD cost for the active session.
func (o *Orchestrator) SessionCost() budget.SessionCost {
	sessionID := o.CurrentSessionID()
	if sessionID == "" {
		sessionID = o.active
	}
	return o.currentUSDBudget().Cost(sessionID)
}

// SubscribeActivity returns execution events for the active session and a
// cleanup function. Delegated agents share this broker and session.
func (o *Orchestrator) SubscribeActivity() (<-chan chronosstream.Event, func(), error) {
	if o.broker == nil {
		return nil, func() {}, fmt.Errorf("execution activity is unavailable")
	}
	sub, err := o.broker.SubscribeTopic(o.CurrentSessionID())
	if err != nil {
		return nil, func() {}, err
	}
	return sub.C, func() { o.broker.Unsubscribe(sub.ID) }, nil
}

func (o *Orchestrator) currentUSDBudget() *budget.Tracker {
	o.budgetMu.RLock()
	tracker := o.usdBudget
	o.budgetMu.RUnlock()
	if tracker != nil {
		return tracker
	}
	// Supports narrowly constructed Orchestrators in tests and embedders.
	o.budgetMu.Lock()
	defer o.budgetMu.Unlock()
	if o.usdBudget == nil {
		o.usdBudget = budget.NewTrackerWithUSDCap(0, 0, 0)
	}
	return o.usdBudget
}

// Workspace returns the detected workspace info (PRD P2-005), or nil if
// detection failed.
func (o *Orchestrator) Workspace() *workspace.Info {
	return o.workspace
}

// ActivationBuffer returns the spreading activation buffer (PRD P3-007).
func (o *Orchestrator) ActivationBuffer() *activation.Buffer {
	return o.actBuf
}

// AttentionBudgeter returns the attention budgeter (PRD P3-008).
func (o *Orchestrator) AttentionBudgeter() *attention.Budgeter {
	return o.attBudget
}

// Teams returns all built teams (PRD P4-001).
func (o *Orchestrator) Teams() map[string]*team.Team {
	return o.teams
}

// GetTeam returns a team by ID.
func (o *Orchestrator) GetTeam(id string) (*team.Team, bool) {
	t, ok := o.teams[id]
	return t, ok
}

// ListTeams returns all team IDs.
func (o *Orchestrator) ListTeams() []string {
	ids := make([]string, 0, len(o.teams))
	for id := range o.teams {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// RunTeam executes a team with a user message (PRD P4-001).
func (o *Orchestrator) RunTeam(ctx context.Context, teamID, message string) (string, error) {
	t, ok := o.teams[teamID]
	if !ok {
		return "", fmt.Errorf("team %q not found (available: %v)", teamID, o.ListTeams())
	}
	return teambuilder.Run(ctx, t, message)
}

// setupTeams builds Team instances from YAML config (PRD P4-001). Teams
// defined in .chronos-code/teams/ or the embedded defaults are assembled
// from the pre-built agents map. Failures are non-fatal: a misconfigured
// team doesn't prevent the harness from starting.
func setupTeams(cfg *config.Config, agents map[string]*agent.Agent) map[string]*team.Team {
	if len(cfg.Teams) == 0 {
		return nil
	}
	teams, err := teambuilder.BuildAll(cfg.Teams, agents)
	if err != nil {
		fmt.Printf("warning: build teams: %v\n", err)
		return nil
	}
	if len(teams) > 0 {
		ids := make([]string, 0, len(teams))
		for id := range teams {
			ids = append(ids, id)
		}
		fmt.Printf("teams: built %d (%v)\n", len(teams), ids)
	}
	return teams
}

func setupMCPRuntimes(ctx context.Context, agents map[string]*agent.Agent, discovered []mcp.ServerConfig, policy *security.Policy, timeout time.Duration, factory mcpdiscover.ClientFactory) []*mcpdiscover.Runtime {
	agentIDs := make([]string, 0, len(agents))
	for id := range agents {
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)
	runtimes := make([]*mcpdiscover.Runtime, 0, len(agentIDs))
	for _, id := range agentIDs {
		a := agents[id]
		configured := make([]mcp.ServerConfig, 0, len(a.MCPClients))
		for _, client := range a.MCPClients {
			configured = append(configured, client.Config())
			_ = client.Close()
		}
		a.MCPClients = nil
		runtime := mcpdiscover.Start(ctx, configured, discovered, a.Tools, policy, timeout, factory)
		runtimes = append(runtimes, runtime)
		for _, status := range runtime.Statuses() {
			if status.State != mcpdiscover.StateConnected {
				fmt.Printf("warning: MCP server for %s: %s\n", a.ID, status.State)
			}
		}
	}
	return runtimes
}

func (o *Orchestrator) Close() error {
	o.closeOnce.Do(func() {
		o.mcpMu.Lock()
		o.mcpClosed = true
		runtimes := append([]*mcpdiscover.Runtime(nil), o.mcpRuntimes...)
		o.mcpMu.Unlock()
		var errs []error
		for _, runtime := range runtimes {
			if err := runtime.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close MCP runtime: %w", err))
			}
		}
		if o.watcher != nil {
			if err := o.watcher.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close graph watcher: %w", err))
			}
		}
		if o.projectDocsWatcher != nil {
			if err := o.projectDocsWatcher.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close project docs watcher: %w", err))
			}
		}
		if o.graphStore != nil {
			if err := o.graphStore.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close graph store: %w", err))
			}
		}
		if o.learningStore != nil {
			if err := o.learningStore.Close(context.Background()); err != nil {
				errs = append(errs, fmt.Errorf("close learning telemetry: %w", err))
			}
		}
		if o.lspManager != nil {
			if err := o.lspManager.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close LSP manager: %w", err))
			}
		}
		if o.broker != nil {
			if err := o.broker.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close activity broker: %w", err))
			}
		}
		if o.store != nil {
			if err := o.store.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close storage: %w", err))
			}
		}
		o.closeErr = errors.Join(errs...)
	})
	return o.closeErr
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
	case "postgres":
		if postgresOpener == nil {
			return nil, "", fmt.Errorf("postgres backend not compiled (build with -tags postgres)")
		}
		store, err := postgresOpener(dsn)
		if err != nil {
			return nil, "", err
		}
		return store, dsn, nil
	default:
		return nil, "", fmt.Errorf("unsupported storage backend: %s", backend)
	}
}
