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
	"github.com/spawn08/chronos/engine/tool/builtins"
	chronostrace "github.com/spawn08/chronos/os/trace"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/sdk/harness"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"

	"github.com/spawn08/chronos-code/internal/activation"
	"github.com/spawn08/chronos-code/internal/apierror"
	"github.com/spawn08/chronos-code/internal/attention"
	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/authorization"
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
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/projectdocs"
	"github.com/spawn08/chronos-code/internal/retention"
	"github.com/spawn08/chronos-code/internal/router"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos-code/internal/skills"
	"github.com/spawn08/chronos-code/internal/teambuilder"
	"github.com/spawn08/chronos-code/internal/toolcompress"
	"github.com/spawn08/chronos-code/internal/verification"
	"github.com/spawn08/chronos-code/internal/workspace"
	"github.com/spawn08/chronos-code/internal/worktree"

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
	graphScope *graph.RequestScope
	watcher    *graph.Watcher

	sessionMgr *session.Manager
	sessions   map[string]string // agentID -> current sessionID
	sessionMu  sync.RWMutex

	router             *router.Router
	routingConfig      *router.Config
	routingMu          sync.Mutex
	modelOverrides     map[string]bool
	roleModels         *roleModelRegistry
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
	hookActivity       *hookActivityTracker
	learningStore      *learning.SQLStore
	telemetryRecorders []*learning.TelemetryRecorder
	runtimeMemory      *runtimeMemory
	planStore          *plan.SQLStore
	planController     *plan.Controller
	worktreeManager    *worktree.Manager
	planMode           atomic.Bool
	editsMu            sync.Mutex
	edits              []fileCheckpoint
	lastExecMu         sync.Mutex
	lastExecRoute      router.Classification
	lastExecAgent      string
	lastExecModels     map[string]resolvedModel
	operationalMu      sync.RWMutex
	operationalActive  map[string]*operationalExecution
	operationalLast    ExecutionSnapshot
	operationalPlan    *PlanRuntimeIdentity
	lspManager         interface{ Close() error }
	broker             *chronosstream.Broker
	policy             *security.Policy
	mcpFactory         mcpdiscover.ClientFactory
	mcpTimeout         time.Duration
	mcpRuntimes        []*mcpdiscover.Runtime
	mcpWatcher         *mcpdiscover.Watcher
	mcpDiscovery       mcpdiscover.Snapshot
	mcpMu              sync.Mutex
	mcpClosed          bool
	capabilities       RuntimeCapabilityManifest
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
	// VerificationMode applies policy to runtime-derived obligations and events.
	// Supplied values are retained as explicitly trusted adapter input.
	VerificationMode        verification.Mode
	VerificationObligations []verification.Obligation
	VerificationEvents      []execution.Event
	// BoundedContext prevents ambient prompt augmentation for durable plan nodes.
	BoundedContext bool
}

// ExecutionResult carries the common identity and either a blocking response
// or a streaming response channel, according to the request mode.
type ExecutionResult struct {
	AgentID          string
	SessionID        string
	TaskID           string
	PPDDecision      *router.PPDDecision
	MemoryIntent     *memory.IntentResult
	ContextReport    ContextReport
	Response         *model.ChatResponse
	Stream           <-chan *model.ChatResponse
	Verification     verification.Decision
	Completion       <-chan ExecutionCompletion
	Budget           execution.BudgetSnapshot
	StopReason       execution.StopReason
	ChangedPaths     []string
	EvidenceIDs      []execution.EvidenceID
	Usage            model.Usage
	CostMicrodollars int64
}

type ExecutionCompletion struct {
	Verification     verification.Decision
	Budget           execution.BudgetSnapshot
	StopReason       execution.StopReason
	ChangedPaths     []string
	EvidenceIDs      []execution.EvidenceID
	Usage            model.Usage
	CostMicrodollars int64
	Err              error
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
	if cfg == nil {
		cfg = &config.Config{}
	}
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		return nil, fmt.Errorf("resolve runtime paths: %w", err)
	}
	if err := retention.Recover(paths.Dir, 100); err != nil {
		return nil, fmt.Errorf("recover interrupted cleanup: %w", err)
	}
	// Keep caller configuration (including embedded-path provenance) intact.
	configuredGraphDB := cfg.Workspace.GraphDB
	runtimeConfig := *cfg
	runtimeConfig.Workspace.Root = paths.Root
	runtimeConfig.Workspace.GraphDB = paths.GraphDB
	cfg = &runtimeConfig
	store, dsn, err := openStorage(cfg)
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}
	if err := writeProjectMetadata(paths); err != nil {
		fmt.Fprintf(os.Stderr, "warning: write project data metadata: %v\n", err)
	}
	var learningStore *learning.SQLStore
	var languageServerManager *lsp.Manager
	var mcpRuntimes []*mcpdiscover.Runtime
	worktreeManager, err := worktree.New(paths.Dir, nil)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("configure worktree manager: %w", err)
	}
	if err := worktreeManager.Prune(ctx); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("recover worktrees: %w", err)
	}
	orch := &Orchestrator{store: store, worktreeManager: worktreeManager}
	defer func() {
		if err == nil {
			return
		}
		err = errors.Join(err, orch.Close())
	}()

	buildConfig := *cfg
	buildConfig.Agents = append([]agent.AgentConfig(nil), cfg.Agents...)
	if cfg.Defaults != nil {
		defaultsCopy := *cfg.Defaults
		buildConfig.Defaults = &defaultsCopy
	}
	applyStoredCredentials(ctx, &buildConfig)

	agents, err := agent.BuildAllWithOptions(ctx, &buildConfig.FileConfig, agent.BuildAllOptions{DefaultStorage: store})
	if err != nil {
		return nil, fmt.Errorf("build agents: %w", err)
	}
	orch.agents = agents

	order := make([]string, 0, len(agents))
	for _, ac := range cfg.Agents {
		if _, ok := agents[ac.ID]; ok {
			order = append(order, ac.ID)
		}
	}
	sort.Strings(order)

	setupTracing(store, agents)

	projectDir, userDir, discoverErr := config.Discover()
	if discoverErr != nil {
		fmt.Fprintf(os.Stderr, "warning: discover project config dir: %v (falling back to embedded defaults)\n", discoverErr)
	}
	projectDir = paths.LegacyDir

	sessionMgr := session.NewManager(store, dsn)
	sessions := setupSessions(ctx, cfg, sessionMgr, agents, resumeSessionID)

	if err := migrateDefaultDatabase(ctx, paths, paths.GraphDB, "graph.db", configuredGraphDB); err != nil {
		return nil, err
	}
	graphStore, graphScope, watcher := setupGraph(ctx, cfg, agents)
	orch.graphStore, orch.graphScope, orch.watcher = graphStore, graphScope, watcher

	root := cfg.Workspace.Root
	if root == "" {
		root = config.WorkspaceRoot()
	}
	learningStore, err = setupLearningTelemetry(ctx, cfg, root, agents, paths)
	if err != nil {
		return nil, fmt.Errorf("configure learning telemetry: %w", err)
	}
	orch.learningStore = learningStore
	for _, a := range agents {
		for _, hook := range a.Hooks {
			if recorder, ok := hook.(*learning.TelemetryRecorder); ok {
				orch.telemetryRecorders = append(orch.telemetryRecorders, recorder)
			}
		}
	}
	wsInfo := setupWorkspace(root, agents)

	if cfg.Session.RecallPriorSummariesEnabled() {
		setupSessionSummaries(sessionMgr, agents)
	}
	memStore := setupMemory(cfg, agents)
	orch.runtimeMemory, err = setupRuntimeMemory(ctx, cfg, paths, agents)
	if err != nil {
		return nil, fmt.Errorf("configure layered memory: %w", err)
	}
	if cfg.Learning.PatternInjectionEnabled() {
		setupLearnedPatterns(ctx, cfg, root, agents)
	}

	skillCatalog := setupSkills(cfg, root, agents)
	languageServerManager = setupLSP(root, wsInfo, agents)
	orch.lspManager = languageServerManager

	rt, routingConfig := setupRouter(ctx, cfg, projectDir, selectPrimaryAgent(agents, order))
	setupPricing(projectDir, userDir)

	policy, err := setupSecurity(projectDir, userDir, root, store, agents)
	if err != nil {
		return nil, fmt.Errorf("configure security: %w", err)
	}
	installWorkspaceShells(agents, root, time.Duration(policy.MaxExecSeconds)*time.Second)
	grCfg := setupGuardrails(cfg, projectDir, agents, policy.SecretPatterns)

	maxTokens, _ := grCfg.TokenBudget()
	tracker := budget.NewTracker(maxTokens, cfg.Tools.CompressionThresholdTokens)
	attBudget := attention.NewBudgeter(100)
	for _, a := range agents {
		a.Hooks = append(a.Hooks, attBudget)
	}

	actBuf := activation.NewBuffer(50)
	broker := chronosstream.NewBroker(chronosstream.WithBufferSize(256))
	orch.actBuf, orch.broker = actBuf, broker
	for _, a := range agents {
		a.Broker = broker
	}
	roleModels := newRoleModelRegistry(agents)
	if err := setupSubAgentsWithModels(agents, roleModels); err != nil {
		return nil, fmt.Errorf("configure subagent delegation: %w", err)
	}
	var hookRunner *security.HookRunner
	hookActivity := &hookActivityTracker{}
	if len(cfg.Hooks.PreToolCall)+len(cfg.Hooks.PostToolCall)+len(cfg.Hooks.UserPromptSubmit) > 0 {
		if err := security.AdmitHooks(cfg.Hooks, policy); err != nil {
			return nil, fmt.Errorf("admit user hooks: %w", err)
		}
		hookRunner, err = security.NewHookRunner(root)
		if err != nil {
			return nil, fmt.Errorf("configure user hooks: %w", err)
		}
	}
	var discovered mcpdiscover.Snapshot
	if cfg.MCP.DiscoveryEnabled() {
		discovered = mcpdiscover.Load(root)
		if discovered.Err != nil {
			fmt.Fprintln(os.Stderr, "warning: one or more MCP discovery sources failed; healthy sources remain available")
		}
	}
	mcpPool := mcpdiscover.NewSharedClientFactory(nil)
	mcpRuntimes = setupMCPRuntimes(ctx, agents, discovered.Servers, policy, mcpdiscover.DefaultConnectTimeout, mcpPool.NewClient)
	orch.mcpRuntimes = mcpRuntimes
	for _, a := range agents {
		// Finish logical implementations before adding cross-cutting wrappers:
		// outlines, ranges, grep and cache hits all enter the same pipeline.
		incctx.Wrap(a, root)
		incctx.WrapGrep(a, root)
		if graphStore != nil {
			activation.Wrap(a, graphStore, actBuf)
		}
		wrapToolPipeline(a, tracker, cfg.Hooks, hookRunner, hookActivity)
	}

	teams := setupTeams(cfg, agents)

	normalizeToolPermissions(agents)
	capabilityManifest, capabilityWarnings, err := validateRuntimeCapabilities(cfg, agents, graphStore != nil, routingConfig)
	if err != nil {
		return nil, err
	}
	for _, warning := range capabilityWarnings {
		fmt.Fprintf(os.Stderr, "warning: %s\n", warning)
	}
	planStore, err := plan.OpenSQLStore(ctx, paths.PlansDB)
	if err != nil {
		return nil, fmt.Errorf("open plan store: %w", err)
	}
	orch.planStore = planStore

	// Project-document compression can invoke a model. Keep it behind the
	// capability contract so invalid runtime prompts fail before any model call.
	pdWatcher := setupProjectDocs(ctx, cfg, root, agents)
	orch.projectDocsWatcher = pdWatcher
	setupConversationPins(store, agents)

	active := selectPrimaryAgent(agents, order)

	*orch = Orchestrator{
		agents:             agents,
		order:              order,
		active:             active,
		primary:            active,
		store:              store,
		cfg:                cfg,
		graphStore:         graphStore,
		graphScope:         graphScope,
		watcher:            watcher,
		sessionMgr:         sessionMgr,
		sessions:           sessions,
		router:             rt,
		routingConfig:      routingConfig,
		modelOverrides:     make(map[string]bool),
		roleModels:         roleModels,
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
		hookActivity:       hookActivity,
		learningStore:      learningStore,
		telemetryRecorders: orch.telemetryRecorders,
		runtimeMemory:      orch.runtimeMemory,
		planStore:          planStore,
		worktreeManager:    worktreeManager,
		lspManager:         languageServerManager,
		broker:             broker,
		policy:             policy,
		mcpFactory:         mcpPool.NewClient,
		mcpTimeout:         mcpdiscover.DefaultConnectTimeout,
		mcpRuntimes:        mcpRuntimes,
		mcpDiscovery:       discovered,
		capabilities:       capabilityManifest,
	}
	if cfg.PrimaryModelSelected() {
		orch.modelOverrides[active] = true
	}
	for i, id := range sortedAgentIDs(agents) {
		if i < len(orch.mcpRuntimes) {
			orch.mcpRuntimes[i].SetAgent(id)
			orch.mcpRuntimes[i].SetDiscoveryMetadata(discovered, orch.mcpToolTransform(agents[id]))
		}
	}
	implementationAgent := "coder"
	if _, ok := agents[implementationAgent]; !ok {
		if routingConfig == nil || active != routingConfig.PPD.Specialist {
			implementationAgent = active
		} else {
			implementationAgent = ""
		}
	}
	orch.planController = plan.NewController(planStore, &planNodeExecutor{
		runner: orch, worktrees: worktreeManager, repositoryRoot: root, implementationAgent: implementationAgent, permissionChecker: orch.permissionChecker,
	}, nil, nil, plan.ControllerConfig{})
	orch.SetApprovalHandler(nil)
	for _, a := range agents {
		// Context guard runs before budget: it trims messages that would exceed
		// the model's context window, preventing 413/400 token-limit errors that
		// the SDK's tool-calling loop doesn't guard against.
		a.Hooks = append(a.Hooks, sessionUXHook{orchestrator: orch})
		a.Hooks = append(a.Hooks, newContextGuardHook(a.Model.Model(), len(a.Tools.List()), contextGuardOptions{
			ContextLimit: a.ContextCfg.MaxContextTokens,
		}))
		// Admit both session and durable delivery budgets as one final hook.
		// If the second reservation fails before the provider call, undo the first.
		a.Hooks = append(a.Hooks, deliveryBudgetHook{session: budgetHook{tracker: tracker, orchestrator: orch, agentID: a.ID}})
	}
	if cfg.MCP.DiscoveryEnabled() {
		orch.mcpWatcher, err = mcpdiscover.Watch(ctx, root, orch.reloadMCPDiscovery)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: watch MCP discovery sources: %v\n", err)
		}
	}
	return orch, nil
}

func installWorkspaceShells(agents map[string]*agent.Agent, root string, timeout time.Duration) {
	for _, a := range agents {
		if a == nil || a.Tools == nil {
			continue
		}
		for _, name := range []string{"shell", "shell_auto"} {
			current, ok := a.Tools.Get(name)
			if !ok {
				continue
			}
			replacement := security.NewWorkspaceShellTool(root, timeout)
			replacement.Name = name
			replacement.Permission = current.Permission
			replacement.RequiresConfirmation = current.RequiresConfirmation
			replacement.RequiresUserInput = current.RequiresUserInput
			a.Tools.Register(replacement)
		}
	}
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
	if cfg.Defaults != nil {
		cfg.Defaults.Model = resolveModelConfig(ctx, cfg, store, "", cfg.Defaults.Model.Provider, cfg.Defaults.Model.Model)
	}
	for i := range cfg.Agents {
		mc := cfg.Agents[i].Model
		cfg.Agents[i].Model = resolveModelConfig(ctx, cfg, store, cfg.Agents[i].ID, mc.Provider, mc.Model)
	}
}

func resolveModelConfig(ctx context.Context, cfg *config.Config, store *auth.Store, agentID, provider, modelID string) agent.ModelConfig {
	provider = auth.CanonicalProvider(provider)
	mc := agent.ModelConfig{Provider: provider, Model: modelID}
	if cfg != nil {
		if cfg.Defaults != nil && auth.CanonicalProvider(cfg.Defaults.Model.Provider) == provider {
			mc = cfg.Defaults.Model
		}
		for _, configured := range cfg.Agents {
			if configured.ID == agentID && auth.CanonicalProvider(configured.Model.Provider) == provider {
				mc = configured.Model
				break
			}
		}
	}
	mc.Provider = provider
	mc.Model = modelID
	if provider == "azure" {
		mc.Deployment = modelID
	}
	if mc.APIKey == "" {
		mc.APIKey = auth.Resolve(ctx, store, provider).Token
	}
	if cfg != nil {
		for name, override := range cfg.Providers {
			if auth.CanonicalProvider(name) == provider && override.BaseURL != "" {
				mc.BaseURL = override.BaseURL
				break
			}
		}
	}
	return mc
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

func wrapUserToolHooks(a *agent.Agent, configured config.HooksConfig, runner *security.HookRunner, activity *hookActivityTracker) {
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
				_, err := runner.Run(ctx, hook, vars)
				activity.record("pre_tool_call", hook.Name, err != nil)
				if err != nil {
					return nil, fmt.Errorf("pre-tool hook %q: %w", hook.Name, err)
				}
			}

			result, handlerErr := original(ctx, args)
			vars["tool_output"] = result
			for _, hook := range configured.PostToolCall {
				_, err := runner.Run(ctx, hook, vars)
				activity.record("post_tool_call", hook.Name, err != nil)
			}
			return result, handlerErr
		}
		a.Tools.Register(&wrapped)
	}
}

// hookActivityTracker records the most recently fired user-configured hook
// (pre_tool_call, post_tool_call, or user_prompt_submit) so the TUI status
// bar can surface it live without polling security.HookRunner directly.
type hookActivityTracker struct {
	last atomic.Pointer[hookActivityRecord]
}

type hookActivityRecord struct {
	kind   string
	name   string
	at     time.Time
	failed bool
}

func (t *hookActivityTracker) record(kind, name string, failed bool) {
	if t == nil {
		return
	}
	t.last.Store(&hookActivityRecord{kind: kind, name: name, at: time.Now(), failed: failed})
}

// LastHookActivity reports the most recently fired user hook and how long
// ago it ran, for the status bar's "hook:" segment. ok is false if no
// configured hook has fired yet this session.
func (o *Orchestrator) LastHookActivity() (label string, age time.Duration, ok bool) {
	if o.hookActivity == nil {
		return "", 0, false
	}
	rec := o.hookActivity.last.Load()
	if rec == nil {
		return "", 0, false
	}
	label = rec.kind + ":" + rec.name
	if rec.failed {
		label += " ✗"
	}
	return label, time.Since(rec.at), true
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

const (
	budgetReservationMetadataKey = "chronos_code_budget_reservation"
	budgetUnpricedMetadataKey    = "chronos_code_budget_unpriced"
)

type budgetReservation struct {
	tracker *budget.Tracker
	id      budget.ReservationID
}

// budgetUnpriced marks a call to a model without a price entry so After can
// still account its tokens and flag the session cost as incomplete.
type budgetUnpriced struct {
	tracker   *budget.Tracker
	sessionID string
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
		if _, durable := execution.OperationLeaseFromContext(ctx); durable {
			if err := h.tracker.Before(ctx, evt); err != nil {
				return fmt.Errorf("delivery model window: %w", err)
			}
		} else {
			h.recoverTokenBudget(ctx, evt)
		}
		if err := claimTurnModelCall(ctx); err != nil {
			return err
		}
	} else if err := h.tracker.Before(ctx, evt); err != nil {
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
	sessionID := storage.SessionFromContext(ctx)
	id, err := tracker.Reserve(sessionID, modelID,
		model.NewTokenCounter(modelID).CountTokens(req.Messages), req.MaxTokens)
	if err != nil {
		if errors.Is(err, budget.ErrUnknownModel) && !tracker.HasUSDCap() {
			if evt.Metadata == nil {
				evt.Metadata = make(map[string]any)
			}
			evt.Metadata[budgetUnpricedMetadataKey] = budgetUnpriced{tracker: tracker, sessionID: sessionID}
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

// recoverTokenBudget clears a spent operational cap so the call can proceed.
// The tracker still records usage; hitting the cap compact/resets rather than
// aborting spawn_subagent and in-progress tool loops.
func (h budgetHook) recoverTokenBudget(ctx context.Context, evt *hooks.Event) {
	if h.tracker.Before(ctx, evt) == nil {
		return
	}
	if h.orchestrator != nil {
		_ = h.orchestrator.CompactActiveSession(ctx)
	}
	if h.tracker.Before(ctx, evt) == nil {
		return
	}
	sessionID := storage.SessionFromContext(ctx)
	if sessionID == "" {
		sessionID = h.agentID
	}
	h.tracker.ResetSession(sessionID)
}

func (h budgetHook) After(ctx context.Context, evt *hooks.Event) error {
	ctx = h.withFallbackSession(ctx)
	if err := h.tracker.After(ctx, evt); err != nil {
		return err
	}
	if evt.Type != hooks.EventModelCallAfter {
		return nil
	}
	if unpriced, ok := evt.Metadata[budgetUnpricedMetadataKey].(budgetUnpriced); ok {
		delete(evt.Metadata, budgetUnpricedMetadataKey)
		if resp, ok := evt.Output.(*model.ChatResponse); ok && resp != nil && evt.Error == nil {
			unpriced.tracker.RecordUnpriced(unpriced.sessionID, resp.Usage)
		}
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
			fmt.Fprintf(os.Stderr, "warning: ensure session for %s: %v\n", a.ID, err)
		}
		sessions[id] = sid
	}
	return sessions
}

func setupLearningTelemetry(ctx context.Context, cfg *config.Config, root string, agents map[string]*agent.Agent, resolved ...config.ProjectPaths) (*learning.SQLStore, error) {
	if !cfg.Learning.Enabled {
		return nil, nil
	}
	dir := filepath.Join(root, config.ConfigDirName)
	dbPath := filepath.Join(dir, "memory.db")
	if len(resolved) > 0 {
		dbPath = resolved[0].TelemetryDB
		dir = filepath.Dir(dbPath)
		if err := snapshotLegacyDatabase(ctx, filepath.Join(resolved[0].LegacyDir, "memory.db"), dbPath); err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create learning telemetry directory %s: %w", dir, err)
	}
	store, err := learning.OpenSQLStore(ctx, dbPath)
	if err != nil {
		return nil, fmt.Errorf("open learning telemetry: %w", err)
	}
	for _, a := range agents {
		if len(resolved) > 0 {
			a.Hooks = append(a.Hooks, learning.NewAsyncTelemetryRecorder(store, root, a.ID, learning.TelemetryRecorderOptions{}))
		} else {
			// Compatibility for synchronous helper callers; New owns async workers.
			a.Hooks = append(a.Hooks, learning.NewTelemetryRecorder(store, root, a.ID))
		}
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
		fmt.Fprintf(os.Stderr, "warning: detect workspace: %v\n", err)
		return nil
	}
	for _, a := range agents {
		a.Tools.Register(workspace.Tool(info))
		previous := a.ContextPinsFn
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var pins []model.Message
			if previous != nil {
				pins = append(pins, previous(ctx)...)
			}
			selected, detectErr := workspace.ForContext(ctx, info)
			if detectErr != nil {
				return pins
			}
			return append(pins, model.Message{Role: model.RoleSystem, Content: selected.Banner()})
		}
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

// Keep short follow-ups grounded when the SDK compacts or trims old turns
// before the per-call context guard can see them. Only the current session's
// recent user requests are eligible; the latest one is already the live task.
func setupConversationPins(store storage.Storage, agents map[string]*agent.Agent) {
	for _, a := range agents {
		previous := a.ContextPinsFn
		a.ContextPinsFn = func(ctx context.Context) []model.Message {
			var pins []model.Message
			if previous != nil {
				pins = previous(ctx)
			}
			query, _ := ctx.Value(messageKey{}).(string)
			sessionID := storage.SessionFromContext(ctx)
			if len(query) == 0 || len(query) > 256 || sessionID == "" || !refersToPreviousTurn(query) {
				return pins
			}
			events, err := store.ListEvents(ctx, sessionID, 0)
			if err != nil {
				return pins
			}
			var recent []string
			skipCurrent := true
			for i := len(events) - 1; i >= 0 && len(recent) < 4; i-- {
				if events[i].Type != "chat_message" {
					continue
				}
				payload, ok := events[i].Payload.(map[string]any)
				if !ok || payload["role"] != model.RoleUser {
					continue
				}
				if skipCurrent {
					skipCurrent = false
					continue
				}
				text, _ := payload["content"].(string)
				if text != "" {
					recent = append(recent, boundedMemoryText(text, 2048))
				}
			}
			if len(recent) == 0 {
				return pins
			}
			var content strings.Builder
			content.WriteString("Recent user requests in this session (context only; follow the latest user request):")
			for i := len(recent) - 1; i >= 0; i-- {
				fmt.Fprintf(&content, "\n- %s", recent[i])
			}
			return append(pins, model.Message{Role: model.RoleSystem, Content: content.String()})
		}
	}
}

func refersToPreviousTurn(query string) bool {
	for _, word := range strings.Fields(strings.ToLower(query)) {
		switch strings.Trim(word, "`'\"()[]{}<>,:;!?.") {
		case "this", "that", "it", "above", "previous", "same", "continue", "already", "again", "also", "aswell", "yes", "yea":
			return true
		}
	}
	return false
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
	root := cfg.Workspace.Root
	if root == "" {
		root = config.WorkspaceRoot()
	}
	dir := filepath.Join(root, config.ConfigDirName, "memory")
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
	if canonical, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = canonical
	}
	if rel, err := filepath.Rel(root, cwd); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		cwd = root
	}
	bundle, err := projectdocs.Load(root, cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: load project instructions: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "warning: render project instructions: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "warning: watch project instructions: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "warning: read bundled skill catalog: %v\n", err)
		return nil
	}
	bundledDiscovery, err := skills.LoadBundledYAMLReport(bundledData)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: parse bundled skill catalog: %v\n", err)
		return nil
	}
	for _, diagnostic := range bundledDiscovery.Diagnostics {
		fmt.Fprintf(os.Stderr, "warning: skill %s: %s\n", diagnostic.Source, diagnostic.Message)
	}
	discovery, err := skills.DiscoverReport(root, bundledDiscovery.Skills)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: discover skills: %v\n", err)
		return nil
	}
	for _, diagnostic := range discovery.Diagnostics {
		fmt.Fprintf(os.Stderr, "warning: skill %s: %s\n", diagnostic.Source, diagnostic.Message)
	}
	catalog := discovery.Skills
	if len(catalog) == 0 {
		return nil
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
			manifest := skillCapabilityManifest(a)
			if selected, _ := ctx.Value(explicitSkillKey{}).(*skills.Skill); selected != nil {
				content := skills.Render([]*skills.Skill{selected})
				contextSourceSelected(ctx, ContextSourceSkills, 1, len(content), false)
				msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: content})
				return msgs
			}
			query := history.query(ctx, msg)
			selection := skills.SelectWithCapabilities(query, catalog, skills.DefaultTopK, manifest)
			if selection.Context != "" {
				contextSourceSelected(ctx, ContextSourceSkills, len(selection.Selected), len(selection.Context), false)
				msgs = append(msgs, model.Message{Role: model.RoleSystem, Content: selection.Context})
			} else {
				contextSourceOmitted(ctx, ContextSourceSkills, ContextOmittedNotSelected)
			}
			return msgs
		}
	}
	return catalog
}

func skillCapabilityManifest(a *agent.Agent) skills.CapabilityManifest {
	manifest := skills.CapabilityManifest{Tools: make(map[string]struct{})}
	if a == nil {
		return manifest
	}
	manifest.AgentID = a.ID
	if a.Model != nil {
		manifest.ModelID = a.Model.Model()
	}
	if a.Tools != nil {
		for _, definition := range a.Tools.List() {
			if definition != nil && definition.Handler != nil && definition.Permission != tool.PermDeny {
				manifest.Tools[definition.Name] = struct{}{}
			}
		}
	}
	return manifest
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
func setupRouter(ctx context.Context, cfg *config.Config, projectDir string, defaultAgent string) (*router.Router, *router.Config) {
	if !cfg.Router.Enabled {
		return nil, nil
	}
	data, err := readOverridableFile(projectDir, "routing.yaml", "routing.yaml")
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: load routing.yaml: %v\n", err)
		return nil, nil
	}
	rcfg, err := router.Parse(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: parse routing.yaml: %v\n", err)
		return nil, nil
	}
	if defaultAgent == "" {
		defaultAgent = DefaultPrimaryAgentID
	}
	rt, err := router.New(rcfg, defaultAgent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: build router: %v\n", err)
		return nil, nil
	}
	if rcfg.Router.Model.Provider != "" && rcfg.Router.Model.Model != "" {
		modelConfig := resolveModelConfig(ctx, cfg, auth.NewStore(), defaultAgent, rcfg.Router.Model.Provider, rcfg.Router.Model.Model)
		if modelConfig.APIKey != "" {
			provider, err := agent.BuildProvider(modelConfig)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: build T1 router classifier: %v\n", err)
			} else if t1 := router.NewT1Classifier(provider, rcfg); t1 != nil {
				rt.SetT1(t1)
			}
		}
	}
	return rt, rcfg
}

// setupPricing merges user then project pricing.yaml overlays onto the
// embedded price table. Failures are warnings: cost reporting falls back to
// the bundled prices rather than blocking startup.
func setupPricing(projectDir, userDir string) {
	overlays := make([]budget.PricingOverlay, 0, 2)
	for _, candidate := range []struct{ source, dir string }{
		{source: "user", dir: userDir},
		{source: "project", dir: projectDir},
	} {
		if candidate.dir == "" {
			continue
		}
		path := filepath.Join(candidate.dir, "pricing.yaml")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: read %s: %v\n", path, err)
			continue
		}
		overlays = append(overlays, budget.PricingOverlay{Source: path, Data: data})
	}
	if err := budget.LoadPricing(overlays...); err != nil {
		fmt.Fprintf(os.Stderr, "warning: %v (using bundled model prices)\n", err)
	}
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
		fmt.Fprintf(os.Stderr, "warning: load guardrails config: %v\n", err)
		return &guardrail.Config{}
	}
	grCfg, err := guardrail.ParseConfig(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: parse guardrails config: %v\n", err)
		return &guardrail.Config{}
	}

	rules, err := guardrail.BuildRules(grCfg, secretPatterns)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: build guardrail rules: %v\n", err)
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
func setupGraph(ctx context.Context, cfg *config.Config, agents map[string]*agent.Agent) (*graph.Store, *graph.RequestScope, *graph.Watcher) {
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
			fmt.Fprintf(os.Stderr, "warning: create graph db dir %s: %v\n", dir, err)
			return nil, nil, nil
		}
	}

	graphStore, err := graph.OpenStore(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: open graph store: %v\n", err)
		return nil, nil, nil
	}

	ix := graph.NewIndexer(graphStore, root)
	indexOnStart := cfg.Workspace.IndexOnStart == nil || *cfg.Workspace.IndexOnStart
	if indexOnStart {
		stats, err := ix.IndexAll(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: index code graph: %v\n", err)
		} else {
			fmt.Fprintf(os.Stderr, "code graph: indexed %d files, %d symbols, %d edges in %s\n",
				stats.Files, stats.Symbols, stats.Edges, stats.Elapsed.Round(1e6))
		}
	}

	requestScope := graph.NewRequestScope(graphStore, root)
	for _, a := range agents {
		for _, def := range requestScope.Tools() {
			a.Tools.Register(def)
		}
		for _, def := range requestScope.ImpactTools() {
			a.Tools.Register(def)
		}
	}

	var watcher *graph.Watcher
	if indexOnStart {
		watcher, err = graph.Watch(ctx, ix)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: start graph watcher: %v\n", err)
			watcher = nil
		}
	}

	return graphStore, requestScope, watcher
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
	classification := router.ClassifyTask(request.Message)
	if agentID == "" {
		agentID = o.active
		o.recordRoute(classification, agentID)
		if o.router != nil {
			routeIntent, routeSpecialist, routeMatched = o.router.ClassifyWithFallback(ctx, request.Message)
			if _, ok := o.agents[routeSpecialist]; !ok {
				routeMatched = false
				routeSpecialist = ""
			}
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
			return ExecutionResult{AgentID: agentID, PPDDecision: ppdDecision}, fmt.Errorf("delivery strategist %q not found", agentID)
		}
		return ExecutionResult{}, fmt.Errorf("agent %q not found", agentID)
	}
	ctx = o.applyResolvedModel(ctx, agentID, request.Message)
	sessionID := request.SessionID
	if sessionID == "" {
		sessionID = o.sessionID(agentID)
	}
	taskID := request.TaskID
	if taskID == "" {
		taskID = session.NewSessionID()
	}
	ctx = context.WithValue(ctx, taskIDKey{}, taskID)
	runIdentity := agent.RunIdentity{TaskID: taskID, RoleID: agentID, SessionID: sessionID, InvocationID: session.NewSessionID()}
	if parent, ok := agent.RunIdentityFromContext(ctx); ok {
		runIdentity.TenantID = parent.TenantID
		runIdentity.RepositoryID = parent.RepositoryID
		runIdentity.DeliveryID = parent.DeliveryID
		runIdentity.NodeID = parent.NodeID
		runIdentity.AttemptID = parent.AttemptID
		runIdentity.GoalRevision = parent.GoalRevision
		runIdentity.ArtifactSnapshot = parent.ArtifactSnapshot
		runIdentity.PolicyRevision = parent.PolicyRevision
		runIdentity.ParentInvocationID = parent.InvocationID
	}
	if authorized, ok := authorization.FromContext(ctx); ok {
		runIdentity.TenantID = authorized.TenantID
		runIdentity.RepositoryID = authorized.RepositoryID
	}
	ctx = agent.WithRunIdentity(ctx, runIdentity)
	workspaceRoot := ""
	if requestRoot, ok := builtins.WorkspaceRootFromContext(ctx); ok {
		workspaceRoot = requestRoot
	} else if o.workspace != nil {
		workspaceRoot = o.workspace.Root
	} else if o.cfg != nil {
		workspaceRoot = o.cfg.Workspace.Root
	}
	ctx = builtins.WithWorkspaceRoot(ctx, workspaceRoot)
	ctx = executionEffectContext(ctx, o.PlanMode())
	taskRuntime, err := o.openTaskRuntime(taskID, request.TaskID != "", workspaceRoot)
	if err != nil {
		return ExecutionResult{}, err
	}
	ctx = withTaskRuntime(ctx, taskRuntime)
	if request.PolicyContext != nil {
		ctx = context.WithValue(ctx, executionPolicyContextKey{}, request.PolicyContext)
	}
	result := ExecutionResult{AgentID: agentID, SessionID: sessionID, TaskID: taskID, PPDDecision: ppdDecision}
	o.beginOperationalExecution(result, request.VerificationMode, taskRuntime)
	streamHandedOff := false
	defer func() {
		if !streamHandedOff {
			o.finishOperationalExecution(result, err)
		}
	}()
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
	message := request.Message
	if request.BoundedContext {
		ctx = o.turnContextFor(ctx, message, agentID, sessionID)
	} else {
		ctx, message, err = o.preparePrompt(ctx, message, agentID, sessionID)
		if err != nil {
			result.ContextReport = o.contextReport(collector)
			return result, err
		}
	}
	if !request.BoundedContext && !hasIntent {
		if hint := formatRoutingHint(agentID, routeIntent, routeSpecialist, routeMatched, classification, o.implementationPath(classification.Complexity), ppdDecision); hint != "" {
			message = hint + "\n\n" + message
		}
	}
	if hasIntent && intent.Action == memory.IntentRecallPast && result.MemoryIntent.Applied {
		ctx = context.WithValue(ctx, messageKey{}, intent.Payload)
	}
	// Predictive context is part of preparation, not a blocking-only feature.
	if !request.BoundedContext && o.graphStore != nil && o.actBuf != nil {
		if preloaded := activation.PredictiveContext(ctx, o.graphStore, o.actBuf, message); preloaded != "" {
			contextSourceSelected(ctx, ContextSourceGraphPrediction, strings.Count(preloaded, "\n"), len(preloaded), false)
			message += "\n\n" + preloaded
		} else {
			contextSourceOmitted(ctx, ContextSourceGraphPrediction, ContextOmittedNotSelected)
		}
	}
	var taskCancel context.CancelFunc
	if o.cfg != nil && o.cfg.Repair.WallTimeSec > 0 {
		ctx, taskCancel = context.WithTimeout(ctx, time.Duration(o.cfg.Repair.WallTimeSec)*time.Second)
	}
	if request.Mode == ExecutionStreaming {
		if o.runtimeMemory == nil {
			result.Stream, err = o.executeStreamWithRecovery(ctx, a, sessionID, message)
			if err == nil {
				result.Stream, result.Completion = o.assessStreamWithRepair(ctx, result.Stream, a, sessionID, request, classification, taskRuntime, taskCancel)
				result.Completion = o.observeOperationalCompletion(result, result.Completion)
				streamHandedOff = true
			} else if taskCancel != nil {
				taskCancel()
			}
			result.ContextReport = o.contextReport(collector)
			return result, err
		}
		streamCtx, cancel := context.WithCancel(ctx)
		result.Stream, err = o.executeStreamWithRecovery(streamCtx, a, sessionID, message)
		if err == nil {
			result.Stream, result.Completion = o.assessStreamWithRepair(streamCtx, result.Stream, a, sessionID, request, classification, taskRuntime, taskCancel)
			result.Completion = o.observeOperationalCompletion(result, result.Completion)
			streamHandedOff = true
			result.Stream = o.runtimeMemory.observeStream(streamCtx, result.Stream, request.Message, cancel)
		} else {
			cancel()
			if taskCancel != nil {
				taskCancel()
			}
			o.recordEpisode(ctx, request.Message, "", err)
		}
		result.ContextReport = o.contextReport(collector)
		return result, err
	}
	if taskCancel != nil {
		defer taskCancel()
	}
	result.Response, err = o.executeBlockingWithRecovery(ctx, a, sessionID, message)
	result.ContextReport = o.contextReport(collector)
	if err != nil {
		populateRuntimeResult(&result, taskRuntime)
		o.recordEpisode(ctx, request.Message, "", err)
		return result, err
	}
	result.Response, result.Verification, result.StopReason, err = o.repairBlocking(ctx, a, sessionID, request, classification, taskRuntime, result.Response)
	result.Budget = taskRuntime.budget.Snapshot()
	populateRuntimeResult(&result, taskRuntime)
	if err != nil {
		o.recordEpisode(ctx, request.Message, "", err)
		return result, err
	}
	decision := result.Verification
	result.Verification = decision
	if !decision.Allowed {
		if result.StopReason == execution.StopSuccess {
			result.StopReason = execution.StopVerificationFailed
		}
		err := &execution.TerminalError{Reason: result.StopReason, Err: fmt.Errorf("verification does not support successful completion")}
		o.recordEpisode(ctx, request.Message, "", err)
		return result, err
	}
	if result.Response != nil {
		o.recordEpisode(ctx, request.Message, result.Response.Content, result.Response.Err)
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
	maxOutputSegments  = 3
	continuationPrompt = "Your previous response was cut off because it reached the output token limit. Continue exactly where it stopped. Do not repeat any text already provided."
)

// Output-token exhaustion is a successful provider response, not a retryable
// request failure. Continue in the same durable session: the SDK has persisted
// the partial assistant message, so this neither re-submits the original user
// request nor replays committed tool calls. Bound follow-ups to prevent an
// unbounded loop when a model repeatedly exhausts its output limit.
func canContinueOutput(a *agent.Agent, sessionID string) bool {
	return a != nil && a.Storage != nil && sessionID != ""
}

func requestModelProvider(ctx context.Context, a *agent.Agent) model.Provider {
	if provider, ok := agent.ModelProviderFromContext(ctx); ok {
		return provider
	}
	return a.Model
}

// Recovery belongs to the SDK's model-request boundary. Resubmitting a chat
// would append the user message again and could repeat already committed tools.
func (o *Orchestrator) executeStreamWithRecovery(ctx context.Context, a *agent.Agent, sessionID, message string) (<-chan *model.ChatResponse, error) {
	if runtime, ok := taskRuntimeFromContext(ctx); ok {
		if err := runtime.beforeModelCall(); err != nil {
			return nil, err
		}
	}
	var stream <-chan *model.ChatResponse
	var err error
	if canContinueOutput(a, sessionID) {
		stream, err = a.ChatStreamWithSession(ctx, sessionID, message)
	} else {
		stream, err = a.ChatStream(ctx, message)
	}
	if err != nil {
		return nil, apierror.Classify(err)
	}
	if !canContinueOutput(a, sessionID) {
		return stream, nil
	}

	out := make(chan *model.ChatResponse, 64)
	go func() {
		defer close(out)
		for attempt := 0; ; attempt++ {
			maxTokens := false
			for response := range stream {
				if runtime, ok := taskRuntimeFromContext(ctx); ok && response != nil {
					if usageErr := runtime.recordModelUsage(requestModelProvider(ctx, a).Model(), response.Usage); usageErr != nil {
						out <- &model.ChatResponse{Err: usageErr}
						return
					}
				}
				isOutputLimit := response != nil && !response.Delta && response.StopReason == model.StopReasonMaxTokens
				if isOutputLimit {
					maxTokens = true
					// This terminal chunk is only terminal for the individual model
					// call. Suppress it while continuing so stream consumers do not
					// render a spurious incomplete-response error between segments.
					if attempt < maxOutputSegments-1 {
						continue
					}
				}
				select {
				case out <- response:
				case <-ctx.Done():
					return
				}
			}
			if !maxTokens || attempt >= maxOutputSegments-1 || ctx.Err() != nil {
				return
			}
			if runtime, ok := taskRuntimeFromContext(ctx); ok {
				if err := runtime.beforeModelCall(); err != nil {
					out <- &model.ChatResponse{Err: err}
					return
				}
			}
			stream, err = a.ChatStreamWithSession(ctx, sessionID, continuationPrompt)
			if err != nil {
				select {
				case out <- &model.ChatResponse{Err: apierror.Classify(err)}:
				case <-ctx.Done():
				}
				return
			}
		}
	}()
	return out, nil
}

func (o *Orchestrator) executeBlockingWithRecovery(ctx context.Context, a *agent.Agent, sessionID, message string) (*model.ChatResponse, error) {
	if runtime, ok := taskRuntimeFromContext(ctx); ok {
		if err := runtime.beforeModelCall(); err != nil {
			return nil, err
		}
	}
	var response *model.ChatResponse
	var err error
	if canContinueOutput(a, sessionID) {
		response, err = a.ChatWithSession(ctx, sessionID, message)
	} else {
		response, err = a.Chat(ctx, message)
	}
	if err != nil {
		return nil, apierror.Classify(err)
	}
	if runtime, ok := taskRuntimeFromContext(ctx); ok && response != nil {
		if err := runtime.recordModelUsage(requestModelProvider(ctx, a).Model(), response.Usage); err != nil {
			return response, err
		}
	}
	if !canContinueOutput(a, sessionID) {
		return response, nil
	}

	for attempt := 0; response != nil && response.StopReason == model.StopReasonMaxTokens && attempt < maxOutputSegments-1; attempt++ {
		partial := response.Content
		if runtime, ok := taskRuntimeFromContext(ctx); ok {
			if err := runtime.beforeModelCall(); err != nil {
				return response, err
			}
		}
		response, err = a.ChatWithSession(ctx, sessionID, continuationPrompt)
		if err != nil {
			return nil, apierror.Classify(err)
		}
		if response != nil {
			if runtime, ok := taskRuntimeFromContext(ctx); ok {
				if err := runtime.recordModelUsage(requestModelProvider(ctx, a).Model(), response.Usage); err != nil {
					return response, err
				}
			}
			response.Content = partial + response.Content
		}
	}
	return response, nil
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

// assessStreamWithRepair preserves streaming content across bounded repair
// continuations and publishes exactly one authoritative terminal completion.
func (o *Orchestrator) assessStreamWithRepair(ctx context.Context, stream <-chan *model.ChatResponse, a *agent.Agent, sessionID string, request ExecutionRequest, classification router.Classification, runtime *taskRuntime, cancel context.CancelFunc) (<-chan *model.ChatResponse, <-chan ExecutionCompletion) {
	assessed := make(chan *model.ChatResponse)
	completed := make(chan ExecutionCompletion, 1)
	go func() {
		defer close(assessed)
		defer close(completed)
		if cancel != nil {
			defer cancel()
		}
		seen := make(map[string]struct{})
		complete := func(decision verification.Decision, reason execution.StopReason, err error) {
			result := ExecutionResult{Verification: decision, Budget: runtime.budget.Snapshot(), StopReason: reason}
			populateRuntimeResult(&result, runtime)
			completed <- ExecutionCompletion{
				Verification: result.Verification, Budget: result.Budget, StopReason: result.StopReason,
				ChangedPaths: result.ChangedPaths, EvidenceIDs: result.EvidenceIDs,
				Usage: result.Usage, CostMicrodollars: result.CostMicrodollars, Err: err,
			}
		}
		for {
			select {
			case <-ctx.Done():
				complete(verification.Decision{}, execution.StopReasonForError(ctx.Err()), ctx.Err())
				return
			case response, ok := <-stream:
				if !ok {
					decision := assessRuntimeVerification(request, classification, runtime)
					if !decision.Disagreement {
						complete(decision, execution.StopSuccess, nil)
						return
					}
					fingerprint := verificationFailureFingerprint(decision)
					if _, repeated := seen[fingerprint]; repeated {
						if decision.Allowed {
							complete(decision, execution.StopSuccess, nil)
							return
						}
						reason := execution.StopRepeatedFailure
						completionErr := error(nil)
						if request.VerificationMode == verification.ModeEnforce {
							completionErr = &execution.TerminalError{Reason: reason, Err: fmt.Errorf("verification does not support successful completion")}
							assessed <- &model.ChatResponse{Err: completionErr}
						}
						complete(decision, reason, completionErr)
						return
					}
					seen[fingerprint] = struct{}{}
					if err := runtime.budget.ConsumeRepairAttempt(); err != nil {
						terminal := &execution.TerminalError{Reason: execution.StopBudgetExhausted, Err: err}
						assessed <- &model.ChatResponse{Err: terminal}
						complete(decision, execution.StopBudgetExhausted, terminal)
						return
					}
					var err error
					stream, err = o.executeStreamWithRecovery(ctx, a, sessionID, buildRepairPrompt(decision, runtime))
					if err != nil {
						assessed <- &model.ChatResponse{Err: err}
						complete(decision, execution.StopReasonForError(err), err)
						return
					}
					continue
				}
				if response != nil && response.Err != nil {
					classified := *response
					classified.Err = apierror.Classify(response.Err)
					response = &classified
				}
				select {
				case assessed <- response:
				case <-ctx.Done():
					return
				}
				if response != nil && response.Err != nil {
					complete(verification.Decision{}, execution.StopReasonForError(response.Err), response.Err)
					return
				}
			}
		}
	}()
	return assessed, completed
}

func assessRuntimeVerification(request ExecutionRequest, classification router.Classification, runtime *taskRuntime) verification.Decision {
	events := append([]execution.Event(nil), request.VerificationEvents...)
	for i := range events {
		if events[i].Provenance == "" {
			events[i].Provenance = execution.ProvenanceTrustedAdapter
		}
	}
	changedPaths := make([]string, 0)
	if runtime != nil {
		if state, err := runtime.snapshot(); err == nil {
			events = append(events, state.Events...)
			for _, write := range state.Writes {
				if len(write.Paths) == 0 {
					changedPaths = append(changedPaths, ".")
				} else {
					changedPaths = append(changedPaths, write.Paths...)
				}
			}
		}
	}
	obligations := append([]verification.Obligation(nil), request.VerificationObligations...)
	obligations = append(obligations, verification.Derive(verification.Input{
		TaskKind: string(classification.Kind), ChangedPaths: changedPaths,
	})...)
	return verification.Assess(request.VerificationMode, true, obligations, events)
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
		o.hookActivity.record("user_prompt_submit", hook.Name, err != nil)
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
	parts := []string{fmt.Sprintf("Path: complexity=%s kind=%s suggested_graph=%s suggested_plan=%s estimated_tools=%d. Advisory only: reassess scope from evidence; complete all requested deliverables and verification within enforced runtime budgets. Suggested approach: %s.", class.Complexity, class.Kind, path.Graph, path.Plan, path.MaxToolCalls, path.Hint)}
	if class.Complexity == "" {
		parts = parts[:0]
	}
	if matched && specialist != "" && specialist != agentID {
		parts = append(parts, fmt.Sprintf("Routing hint: intent=%s specialist=%s. You remain %s; spawn_subagent %s if that specialist loop is needed.", intent, specialist, agentID, specialist))
	}
	if ppd != nil && ppd.Action == router.PPDActionShadow && ppd.Specialist != "" && ppd.Specialist != agentID {
		parts = append(parts, "PPD hint: consider spawn_subagent "+ppd.Specialist+" if this work needs a durable dependency-ordered plan; retain ownership of implementation and verification.")
	}
	return strings.Join(parts, "\n")
}

// Route classifies message via the T0 intent router, falling back to the T1
// cheap-model classifier when configured (PRD P2-006), and returns the
// specialist the classifier would pick. Execute keeps the active Chronos
// Code conversation unless the caller requested an agent or PPD delegates.
// Model routing is applied by Execute after the final agent is known; Route is
// classification-only and never mutates an agent's configured model.
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
	return agentID, matched
}

type requestRouting struct {
	AgentID        string
	Classification router.Classification
	Provider       string
	Model          string
}

type requestRoutingKey struct{}

func requestRoutingFromContext(ctx context.Context) (requestRouting, bool) {
	routing, ok := ctx.Value(requestRoutingKey{}).(requestRouting)
	return routing, ok
}

func (o *Orchestrator) applyResolvedModel(ctx context.Context, agentID, message string) context.Context {
	if agent.ModelProvider(ctx, nil) != nil {
		return ctx
	}
	classification := router.ClassifyTask(message)
	o.routingMu.Lock()
	routingConfig := o.routingConfig
	overridden := o.modelOverrides[agentID]
	selected := o.agents[agentID].Model
	o.routingMu.Unlock()
	if routingConfig != nil && !overridden {
		if spec, ok := routingConfig.ResolveModelForRole(agentID, classification.Complexity, classification.Kind); ok {
			// A bundled route must not send an authorized agent to a provider
			// for which no credential can be resolved.
			if o.cfg == nil || selected == nil || auth.CanonicalProvider(spec.Provider) == auth.CanonicalProvider(selected.Name()) ||
				resolveModelConfig(ctx, o.cfg, auth.NewStore(), agentID, spec.Provider, spec.Model).APIKey != "" {
				if provider, err := o.buildModelProvider(ctx, agentID, spec.Provider, spec.Model); err == nil {
					selected = provider
				}
			}
		}
	}
	if selected == nil {
		return ctx
	}
	o.recordResolvedModel(agentID, selected.Name(), selected.Model())
	ctx = agent.WithModelProvider(ctx, selected)
	return context.WithValue(ctx, requestRoutingKey{}, requestRouting{
		AgentID: agentID, Classification: classification, Provider: selected.Name(), Model: selected.Model(),
	})
}

// SetApprovalHandler installs handler behind the policy checker on every agent
// registry. Replacing the human handler never replaces policy enforcement.
func (o *Orchestrator) SetApprovalHandler(handler tool.ApprovalFunc) {
	for _, a := range o.agents {
		a.Tools.SetApprovalHandler(func(ctx context.Context, toolName string, args map[string]any) (bool, error) {
			// LayerTools are host-bound memory operations, not arbitrary file IO.
			// Preserve their allow policy after generic permission normalization;
			// the registry still enforces explicit denials and handlers enforce
			// scope isolation, semantic opt-in and explicit org publication.
			if o.runtimeMemory != nil && isLayerMemoryTool(toolName) {
				return true, nil
			}
			switch o.permissionChecker.CheckContext(ctx, toolName, args, o.permissionYolo.Load()) {
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

type resolvedModel struct{ provider, model string }

func (o *Orchestrator) recordResolvedModel(agentID, provider, modelID string) {
	o.lastExecMu.Lock()
	defer o.lastExecMu.Unlock()
	if o.lastExecModels == nil {
		o.lastExecModels = make(map[string]resolvedModel)
	}
	o.lastExecModels[agentID] = resolvedModel{provider: provider, model: modelID}
}

// EffectiveModelInfo returns the provider/model that served the active
// agent's most recent request after request-time routing (routing.yaml may
// send it to a different model than the configured one). Before the first
// request, or after /model, it is the configured model.
func (o *Orchestrator) EffectiveModelInfo() (provider, modelID string) {
	o.lastExecMu.Lock()
	resolved, ok := o.lastExecModels[o.active]
	o.lastExecMu.Unlock()
	if ok {
		return resolved.provider, resolved.model
	}
	return o.ActiveModelInfo()
}

// AgentModelInfo returns the configured provider/model for a given agent ID
// (typically a pre-registered subagent spawned via spawn_subagent), for
// display alongside its activity in the status bar. ok is false if no such
// agent is registered or it has no model configured.
func (o *Orchestrator) AgentModelInfo(agentID string) (provider, modelID string, ok bool) {
	a, exists := o.agents[agentID]
	if !exists || a.Model == nil {
		return "", "", false
	}
	return a.Model.Name(), a.Model.Model(), true
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
	provider = auth.CanonicalProvider(provider)
	p, err := o.buildModelProvider(ctx, o.active, provider, modelID)
	if err != nil {
		return err
	}
	o.routingMu.Lock()
	defer o.routingMu.Unlock()
	a := o.agents[o.active]
	if a == nil {
		return fmt.Errorf("no active agent")
	}
	a.Model = p
	o.roleModels.set(o.active, p)
	if o.modelOverrides == nil {
		o.modelOverrides = make(map[string]bool)
	}
	o.modelOverrides[o.active] = true
	o.lastExecMu.Lock()
	delete(o.lastExecModels, o.active)
	o.lastExecMu.Unlock()
	return nil
}

func (o *Orchestrator) buildModelProvider(ctx context.Context, agentID, provider, modelID string) (model.Provider, error) {
	provider = auth.CanonicalProvider(provider)
	mc := resolveModelConfig(ctx, o.cfg, auth.NewStore(), agentID, provider, modelID)
	buildProvider := o.buildProvider
	if buildProvider == nil {
		buildProvider = agent.BuildProvider
	}
	p, err := buildProvider(mc)
	if err != nil {
		return nil, fmt.Errorf("build provider for %s/%s: %w", provider, modelID, err)
	}
	return p, nil
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

// Login stores an API-key credential for provider and immediately rebuilds
// every loaded agent using that provider so the credential takes effect
// without restarting the session.
func (o *Orchestrator) Login(ctx context.Context, provider, apiKey string) error {
	provider = auth.CanonicalProvider(provider)
	if err := auth.LoginAPIKey(auth.NewStore(), provider, apiKey); err != nil {
		return err
	}
	return o.rebuildProviderAgents(ctx, provider)
}

func (o *Orchestrator) rebuildProviderAgents(ctx context.Context, provider string) error {
	for agentID, configured := range o.agents {
		if configured == nil || configured.Model == nil || auth.CanonicalProvider(configured.Model.Name()) != provider {
			continue
		}
		modelID := configured.Model.Model()
		p, err := o.buildModelProvider(ctx, agentID, provider, modelID)
		if err != nil {
			return err
		}
		o.routingMu.Lock()
		configured.Model = p
		o.roleModels.set(agentID, p)
		o.routingMu.Unlock()
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
	if err := auth.LoginPKCE(ctx, auth.NewStore(), cfg, onPromptURL); err != nil {
		return err
	}
	return o.rebuildProviderAgents(ctx, auth.CanonicalProvider(cfg.Provider))
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

// AuthorizedProviders returns candidates with a resolvable credential
// (env var, stored login, external CLI reuse, or agent/default YAML API key).
// It makes no network calls.
// Keychain lookups are memoized by auth.Store for the process lifetime
// (invalidated on login/logout), so repeating this for the TUI status bar
// or /model completions stays cheap after the first scan.
func (o *Orchestrator) AuthorizedProviders(ctx context.Context, candidates []string) []string {
	store := auth.NewStore()
	var out []string
	for _, p := range candidates {
		if auth.Resolve(ctx, store, p).Token != "" {
			out = append(out, p)
			continue
		}
		if o.cfg != nil {
			if o.cfg.Defaults != nil && auth.CanonicalProvider(o.cfg.Defaults.Model.Provider) == auth.CanonicalProvider(p) && o.cfg.Defaults.Model.APIKey != "" {
				out = append(out, p)
				continue
			}
			for _, configured := range o.cfg.Agents {
				if auth.CanonicalProvider(configured.Model.Provider) == auth.CanonicalProvider(p) && configured.Model.APIKey != "" {
					out = append(out, p)
					break
				}
			}
		}
	}
	return out
}

// ListActiveProviderModels attempts to fetch a live model list for the
// active agent's provider. See ListProviderModels for the full contract.
func (o *Orchestrator) ListActiveProviderModels(ctx context.Context) (models []modelinfo.Info, ok bool) {
	provider, _ := o.ActiveModelInfo()
	if provider == "" {
		return nil, false
	}
	return o.ListProviderModels(ctx, provider)
}

// ListProviderModels attempts to fetch a live model list for provider —
// which need not be the active agent's provider, e.g. so a model picker can
// show a provider's real deployments/models before the user has ever
// switched to it — using its resolved credential (modelinfo.FetchLive):
// real model IDs from the vendor's own API, not a hardcoded list. It
// reports ok=false (never an error the caller must handle) when the
// provider has no supported live-listing endpoint, no credential is
// currently resolvable, or the request fails for any reason (network,
// timeout, auth); callers should fall back to modelinfo.All() in that case,
// which is the pre-existing, always-available static registry.
func (o *Orchestrator) ListProviderModels(ctx context.Context, provider string) (models []modelinfo.Info, ok bool) {
	provider = auth.CanonicalProvider(provider)
	agentID := ""
	if o.cfg != nil {
		for _, configured := range o.cfg.Agents {
			if auth.CanonicalProvider(configured.Model.Provider) == provider {
				agentID = configured.ID
				break
			}
		}
	}
	mc := resolveModelConfig(ctx, o.cfg, auth.NewStore(), agentID, provider, "")
	if mc.APIKey == "" {
		return nil, false
	}
	list, err := modelinfo.FetchLive(ctx, provider, mc.APIKey, modelinfo.LiveConfig{BaseURL: mc.BaseURL, Endpoint: mc.Endpoint})
	if err != nil || len(list) == 0 {
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
	var out []mcpdiscover.ServerStatus
	for _, runtime := range o.mcpRuntimes {
		out = append(out, runtime.Statuses()...)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Agent != out[j].Agent {
			return out[i].Agent < out[j].Agent
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func (o *Orchestrator) MCPSourceStatuses() []mcpdiscover.SourceStatus {
	o.mcpMu.Lock()
	defer o.mcpMu.Unlock()
	statuses := append([]mcpdiscover.SourceStatus(nil), o.mcpDiscovery.Sources...)
	for i := range statuses {
		statuses[i].Servers = append([]mcp.ServerConfig(nil), statuses[i].Servers...)
		for j := range statuses[i].Servers {
			statuses[i].Servers[j].Args = append([]string(nil), statuses[i].Servers[j].Args...)
		}
	}
	return statuses
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
	factory := o.mcpFactory
	if factory == nil {
		factory = mcpdiscover.NewSharedClientFactory(nil).NewClient
		o.mcpFactory = factory
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
	type mcpConnection struct {
		agent    *agent.Agent
		runtime  *mcpdiscover.Runtime
		config   mcp.ServerConfig
		identity security.MCPServerIdentity
	}
	connections := make([]mcpConnection, 0, len(agentIDs))
	for i, id := range agentIDs {
		a := o.agents[id]
		if a == nil {
			continue
		}
		if i >= len(o.mcpRuntimes) || o.mcpRuntimes[i] == nil {
			return mcpdiscover.ServerStatus{}, fmt.Errorf("MCP runtime for agent %q is not available", id)
		}
		agentConfig, known := o.mcpRuntimes[i].Server(name)
		if !known {
			agentConfig = cfg
			o.mcpRuntimes[i].RememberServer(agentConfig)
		}
		identity, exists := o.mcpRuntimes[i].Identity(name)
		if !exists {
			return mcpdiscover.ServerStatus{}, fmt.Errorf("MCP server %q identity is unavailable", name)
		}
		if len(connections) > 0 && identity.Digest() != connections[0].identity.Digest() {
			return mcpdiscover.ServerStatus{}, fmt.Errorf("MCP server %q has inconsistent agent configuration identities", name)
		}
		connections = append(connections, mcpConnection{agent: a, runtime: o.mcpRuntimes[i], config: agentConfig, identity: identity})
	}
	if len(connections) == 0 {
		return mcpdiscover.ServerStatus{}, fmt.Errorf("MCP server %q is not configured", name)
	}
	if err := o.policy.AllowMCPServerSessionIdentity(connections[0].identity); err != nil {
		return mcpdiscover.ServerStatus{}, fmt.Errorf("connect MCP server %q: %w", name, err)
	}
	var last mcpdiscover.ServerStatus
	for _, connection := range connections {
		before := registeredToolNameSet(connection.agent)
		last = connection.runtime.ConnectServer(ctx, connection.config, connection.agent.Tools, o.policy, timeout, factory)
		wrapLateTools(connection.agent, before, o)
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
	if o == nil {
		o = &Orchestrator{}
	}
	// A temporary registry limits installation to new definitions. Re-wrapping
	// the existing registry would execute hooks twice and re-compress references.
	partial := &agent.Agent{ID: a.ID, Model: a.Model, Storage: a.Storage, Tools: tool.NewRegistry()}
	for _, def := range a.Tools.List() {
		if _, existed := before[def.Name]; existed || def.Handler == nil {
			continue
		}
		wrapped := *def
		if wrapped.Permission != tool.PermDeny {
			wrapped.Permission = tool.PermRequireApproval
		}
		partial.Tools.Register(&wrapped)
	}
	if len(partial.Tools.List()) == 0 {
		return
	}
	var configured config.HooksConfig
	if o.cfg != nil {
		configured = o.cfg.Hooks
	}
	wrapToolPipeline(partial, o.budget, configured, o.hookRunner, o.hookActivity)
	for _, def := range partial.Tools.List() {
		if _, existed := before[def.Name]; !existed {
			a.Tools.Register(def)
		}
	}
}

func wrapToolPipeline(a *agent.Agent, tracker *budget.Tracker, configured config.HooksConfig, runner *security.HookRunner, activity *hookActivityTracker) {
	toolcompress.RegisterReader(a)
	wrapUserToolHooks(a, configured, runner, activity)
	wrapVerificationEvidence(a)
	wrapDeliveryOperations(a)
	toolcompress.WrapDynamicForTool(a, func(ctx context.Context, name string, args map[string]any) int {
		base := toolcompress.DefaultThresholdTokens
		if tracker != nil {
			base = tracker.CompressionThreshold(sessionOrAgentKey(ctx, a.ID))
		}
		weight := attention.Weight(attention.Classify(name, args))
		if name == "codebase_search" || name == "codebase_map" || name == "codebase_context" {
			weight = attention.Weight(attention.CatGraph)
		}
		return attention.AdjustThreshold(base, weight)
	})
	wrapToolResultCap(a)
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

func sortedAgentIDs(agents map[string]*agent.Agent) []string {
	ids := make([]string, 0, len(agents))
	for id := range agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func (o *Orchestrator) mcpToolTransform(a *agent.Agent) mcpdiscover.ToolTransform {
	return func(definitions []*tool.Definition) []*tool.Definition {
		if a == nil || len(definitions) == 0 {
			return definitions
		}
		partial := &agent.Agent{ID: a.ID, Model: a.Model, Storage: a.Storage, Tools: tool.NewRegistry()}
		for _, definition := range definitions {
			partial.Tools.Register(definition)
		}
		var configured config.HooksConfig
		if o.cfg != nil {
			configured = o.cfg.Hooks
		}
		wrapUserToolHooks(partial, configured, o.hookRunner, o.hookActivity)
		wrapVerificationEvidence(partial)
		toolcompress.WrapDynamicForTool(partial, func(ctx context.Context, name string, args map[string]any) int {
			threshold := toolcompress.DefaultThresholdTokens
			if o.budget != nil {
				threshold = o.budget.CompressionThreshold(sessionOrAgentKey(ctx, a.ID))
			}
			return attention.AdjustThreshold(threshold, attention.Weight(attention.Classify(name, args)))
		})
		wrapToolResultCap(partial)
		return partial.Tools.List()
	}
}

func (o *Orchestrator) reloadMCPDiscovery(snapshot mcpdiscover.Snapshot) {
	o.mcpMu.Lock()
	defer o.mcpMu.Unlock()
	if o.mcpClosed {
		return
	}
	o.mcpDiscovery = snapshot
	for i, id := range sortedAgentIDs(o.agents) {
		if i >= len(o.mcpRuntimes) || o.mcpRuntimes[i] == nil {
			continue
		}
		o.mcpRuntimes[i].SetDiscoveryMetadata(snapshot, o.mcpToolTransform(o.agents[id]))
		reloadCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		o.mcpRuntimes[i].ReloadDiscovery(reloadCtx, snapshot)
		cancel()
	}
}

// RunSubagent invokes the active agent's spawn_subagent tool directly. The
// supplied arguments use the tool's public schema: task plus either agent, or
// system_prompt and an optional tools list for a dynamic subagent.
func (o *Orchestrator) RunSubagent(ctx context.Context, args map[string]any) (string, error) {
	task, _ := args["task"].(string)
	ctx = o.turnContext(ctx, task)
	result, err := o.ExecuteTool(ctx, harness.SpawnToolName, args)
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

// ActiveSessionIDs returns a snapshot used to exclude live resources from
// retention. The returned slice does not alias orchestrator state.
func (o *Orchestrator) ActiveSessionIDs() []string {
	o.sessionMu.RLock()
	defer o.sessionMu.RUnlock()
	ids := make([]string, 0, len(o.sessions))
	seen := make(map[string]bool, len(o.sessions))
	for _, id := range o.sessions {
		if id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
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
// P2-009) without discarding the active agent's conversation. The budget
// tracker is reset before CompactSession so the summarizer model call is
// not blocked by the cap we are recovering from, then reset again so the
// summarizer's own usage does not immediately re-trip the cap.
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
	// Reset before summarizing: CompactSession itself makes a model call,
	// and the budget hook would otherwise abort that call with the same
	// "token budget exceeded" error we are recovering from.
	if o.budget != nil {
		o.budget.ResetSession(sessionID)
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
	var err error
	ctx, err = o.executionTaskContext(ctx, "team:"+teamID)
	if err != nil {
		return "", err
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
		fmt.Fprintf(os.Stderr, "warning: build teams: %v\n", err)
		return nil
	}
	if len(teams) > 0 {
		ids := make([]string, 0, len(teams))
		for id := range teams {
			ids = append(ids, id)
		}
		fmt.Fprintf(os.Stderr, "teams: built %d (%v)\n", len(teams), ids)
	}
	return teams
}

func setupMCPRuntimes(ctx context.Context, agents map[string]*agent.Agent, discovered []mcp.ServerConfig, policy *security.Policy, timeout time.Duration, factory mcpdiscover.ClientFactory) []*mcpdiscover.Runtime {
	if factory == nil {
		factory = mcpdiscover.NewSharedClientFactory(nil).NewClient
	}
	agentIDs := make([]string, 0, len(agents))
	for id := range agents {
		agentIDs = append(agentIDs, id)
	}
	sort.Strings(agentIDs)
	runtimes := make([]*mcpdiscover.Runtime, 0, len(agentIDs))
	for _, id := range agentIDs {
		a := agents[id]
		configured := make([]mcp.ServerConfig, 0, len(a.MCPClients))
		// BuildAll records unconnected SDK clients. Hand their configs to the
		// policy-gated runtime; transport creation is lazy in the shared factory.
		for _, client := range a.MCPClients {
			configured = append(configured, client.Config())
			_ = client.Close()
		}
		a.MCPClients = nil
		runtime := mcpdiscover.Start(ctx, configured, discovered, a.Tools, policy, timeout, factory)
		runtime.SetAgent(id)
		runtimes = append(runtimes, runtime)
		for _, status := range runtime.Statuses() {
			if status.State != mcpdiscover.StateConnected {
				fmt.Fprintf(os.Stderr, "warning: MCP server for %s: %s\n", a.ID, status.State)
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
		if o.runtimeMemory != nil {
			if err := o.runtimeMemory.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close layered memory: %w", err))
			}
		}
		for _, runtime := range runtimes {
			if err := runtime.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close MCP runtime: %w", err))
			}
		}
		if o.mcpWatcher != nil {
			if err := o.mcpWatcher.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close MCP watcher: %w", err))
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
		if o.actBuf != nil {
			if err := o.actBuf.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close activation buffer: %w", err))
			}
		}
		if o.graphScope != nil {
			if err := o.graphScope.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close request graph scope: %w", err))
			}
		}
		if o.graphStore != nil {
			if err := o.graphStore.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close graph store: %w", err))
			}
		}
		for _, recorder := range o.telemetryRecorders {
			if err := recorder.Close(context.Background()); err != nil {
				errs = append(errs, fmt.Errorf("close telemetry recorder: %w", err))
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
		if o.planStore != nil {
			if err := o.planStore.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close plan store: %w", err))
			}
		}
		if o.worktreeManager != nil {
			if err := o.worktreeManager.Close(context.Background()); err != nil {
				errs = append(errs, fmt.Errorf("close worktree manager: %w", err))
			}
		}
		stores := map[storage.Storage]bool{}
		if o.store != nil {
			stores[o.store] = true
		}
		for _, a := range o.agents {
			if a.Storage != nil {
				stores[a.Storage] = true
			}
			// Before MCP runtime handoff, the SDK agent still owns its clients.
			for _, client := range a.MCPClients {
				if err := client.Close(); err != nil {
					errs = append(errs, fmt.Errorf("close agent MCP client: %w", err))
				}
			}
		}
		for store := range stores {
			if err := store.Close(); err != nil {
				errs = append(errs, fmt.Errorf("close storage: %w", err))
			}
		}
		o.closeErr = errors.Join(errs...)
	})
	return o.closeErr
}

func openStorage(cfg *config.Config) (storage.Storage, string, error) {
	if cfg == nil {
		cfg = &config.Config{}
	}
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		return nil, "", fmt.Errorf("resolve storage paths: %w", err)
	}
	backend := "sqlite"
	dsn := paths.SessionsDB
	configuredDSN := ""
	if cfg.Defaults != nil {
		configuredDSN = cfg.Defaults.Storage.DSN
		if cfg.Defaults.Storage.Backend != "" {
			backend = cfg.Defaults.Storage.Backend
		}
	}
	switch backend {
	case "sqlite":
		if err := migrateDefaultDatabase(context.Background(), paths, dsn, "sessions.db", configuredDSN); err != nil {
			return nil, "", err
		}
		if dir := filepath.Dir(dsn); dsn != ":memory:" && !strings.HasPrefix(dsn, "file:") && dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, "", fmt.Errorf("create storage dir %s: %w", dir, err)
			}
		}
		store, err := sqlite.New(dsn)
		if err != nil {
			return nil, "", fmt.Errorf("sqlite: %w", err)
		}
		if err := store.Migrate(context.Background()); err != nil {
			return nil, "", errors.Join(fmt.Errorf("sqlite migrate: %w", err), store.Close())
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
