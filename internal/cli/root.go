package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/mcpdiscover"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/modelinfo"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/retention"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos-code/internal/server"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos-code/internal/skills"
	"github.com/spawn08/chronos-code/internal/tui"
	"github.com/spawn08/chronos/engine/mcp"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var (
	configPath          string
	debugMode           bool
	streamMode          = true
	permissionMode      string
	yoloMode            bool
	usdBudgetCap        budget.Microdollars
	usdBudgetSet        bool
	resumeSessionID     string
	jsonMode            bool
	planModeFlag        bool
	skipPermissions     bool
	providerOverride    string
	modelOverride       string
	providerOverrideSet bool
	modelOverrideSet    bool
	mcpConnectNames     []string
	mcpConfigFiles      []string
	strictMCPConfig     bool
	ephemeralRun        bool
	skillSourcesRun     string
	projectDocsBudget   int
	policyFilesRun      []string
)

// Specialists retain the lean PRD P1-005 ceiling. The primary agent also owns
// requirement understanding, continuity, integration, and completion criteria.
const (
	systemPromptTokenBudget        = 800
	primarySystemPromptTokenBudget = 1600
)

func Execute() error {
	if err := stripGlobalFlags(); err != nil {
		return fmt.Errorf("parse flags: %w", err)
	}
	if len(os.Args) < 2 {
		return runREPL()
	}
	switch os.Args[1] {
	case "repl", "interactive":
		return runREPL()
	case "run":
		return runHeadless()
	case "init":
		return runInit()
	case "config":
		return runConfig()
	case "login":
		return runLogin()
	case "logout":
		return runLogout()
	case "whoami":
		return runWhoami()
	case "providers":
		return runProviders()
	case "models":
		return runModels()
	case "agents":
		return runAgents()
	case "auth":
		return runAuth()
	case "session":
		return runSession()
	case "memory":
		return runMemory()
	case "mcp":
		return runMCP()
	case "indexer":
		return runIndexer()
	case "learn":
		return runLearn()
	case "eval":
		return runEval()
	case "team":
		return runTeam()
	case "plan":
		return runPlan()
	case "delivery":
		return runDeliveryDB()
	case "skills":
		return runSkills()
	case "cleanup":
		return runCleanup()
	case "serve":
		return runServe()
	case "version", "--version", "-V":
		return printVersion()
	case "help", "-h", "--help":
		return printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		return printUsage()
	}
}

func stripGlobalFlags() error {
	var cleaned []string
	args := os.Args
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "-c" || arg == "--config":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", arg)
			}
			configPath = args[i+1]
			i += 2
			continue
		case strings.HasPrefix(arg, "-c="):
			configPath = strings.TrimPrefix(arg, "-c=")
			i++
			continue
		case strings.HasPrefix(arg, "--config="):
			configPath = strings.TrimPrefix(arg, "--config=")
			i++
			continue
		case arg == "--debug":
			debugMode = true
			i++
			continue
		case arg == "--provider" || arg == "--model":
			if i+1 >= len(args) {
				return fmt.Errorf("%s requires a value", arg)
			}
			if err := setModelSelectionFlag(arg, args[i+1]); err != nil {
				return err
			}
			i += 2
			continue
		case strings.HasPrefix(arg, "--provider="):
			if err := setModelSelectionFlag("--provider", strings.TrimPrefix(arg, "--provider=")); err != nil {
				return err
			}
			i++
			continue
		case strings.HasPrefix(arg, "--model="):
			if err := setModelSelectionFlag("--model", strings.TrimPrefix(arg, "--model=")); err != nil {
				return err
			}
			i++
			continue
		case arg == "--stream" || arg == "-s":
			streamMode = true
			i++
			continue
		case arg == "--no-stream":
			streamMode = false
			i++
			continue
		case arg == "--permission-mode":
			if i+1 >= len(args) {
				return fmt.Errorf("--permission-mode requires a value")
			}
			permissionMode = args[i+1]
			i += 2
			continue
		case strings.HasPrefix(arg, "--permission-mode="):
			permissionMode = strings.TrimPrefix(arg, "--permission-mode=")
			i++
			continue
		case arg == "--yolo":
			yoloMode = true
			i++
			continue
		case arg == "--budget":
			if i+1 >= len(args) {
				return fmt.Errorf("--budget requires a value")
			}
			cap, err := parseUSDBudget(args[i+1])
			if err != nil {
				return fmt.Errorf("--budget: %w", err)
			}
			usdBudgetCap = cap
			usdBudgetSet = true
			i += 2
			continue
		case strings.HasPrefix(arg, "--budget="):
			cap, err := parseUSDBudget(strings.TrimPrefix(arg, "--budget="))
			if err != nil {
				return fmt.Errorf("--budget: %w", err)
			}
			usdBudgetCap = cap
			usdBudgetSet = true
			i++
			continue
		case arg == "--resume":
			if i+1 >= len(args) {
				return fmt.Errorf("--resume requires a session id")
			}
			resumeSessionID = args[i+1]
			i += 2
			continue
		case strings.HasPrefix(arg, "--resume="):
			resumeSessionID = strings.TrimPrefix(arg, "--resume=")
			i++
			continue
		case arg == "--json":
			jsonMode = true
			i++
			continue
		case arg == "--mcp-connect":
			if i+1 >= len(args) {
				return fmt.Errorf("--mcp-connect requires a server name")
			}
			mcpConnectNames = append(mcpConnectNames, splitNonEmpty(args[i+1])...)
			i += 2
			continue
		case strings.HasPrefix(arg, "--mcp-connect="):
			mcpConnectNames = append(mcpConnectNames, splitNonEmpty(strings.TrimPrefix(arg, "--mcp-connect="))...)
			i++
			continue
		case arg == "--plan-mode":
			planModeFlag = true
			i++
			continue
		case arg == "--dangerously-skip-permissions":
			skipPermissions = true
			i++
			continue
		}
		cleaned = append(cleaned, arg)
		i++
	}
	os.Args = cleaned
	if yoloMode && permissionMode == "deny" {
		return fmt.Errorf("--yolo conflicts with --permission-mode deny")
	}
	if skipPermissions && permissionMode == "deny" {
		return fmt.Errorf("--dangerously-skip-permissions conflicts with --permission-mode deny")
	}
	return nil
}

func setModelSelectionFlag(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s requires a non-empty value", name)
	}
	if name == "--provider" {
		providerOverride = auth.CanonicalProvider(value)
		providerOverrideSet = true
	} else {
		modelOverride = value
		modelOverrideSet = true
	}
	return nil
}

func parseUSDBudget(value string) (budget.Microdollars, error) {
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && (parts[1] == "" || len(parts[1]) > 6)) {
		return 0, fmt.Errorf("must be a non-negative decimal USD amount with at most 6 decimal places")
	}
	for _, part := range parts {
		for i := 0; i < len(part); i++ {
			if part[i] < '0' || part[i] > '9' {
				return 0, fmt.Errorf("must be a non-negative decimal USD amount with at most 6 decimal places")
			}
		}
	}

	whole, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount overflows microdollars")
	}
	fraction := uint64(0)
	if len(parts) == 2 {
		padded := parts[1] + strings.Repeat("0", 6-len(parts[1]))
		fraction, _ = strconv.ParseUint(padded, 10, 64)
	}

	const maxMicrodollars = uint64(1<<63 - 1)
	if whole > maxMicrodollars/1_000_000 {
		return 0, fmt.Errorf("amount overflows microdollars")
	}
	microdollars := whole*1_000_000 + fraction
	if microdollars > maxMicrodollars {
		return 0, fmt.Errorf("amount overflows microdollars")
	}
	return budget.Microdollars(microdollars), nil
}

// runtimeCapabilities lists the headless features this build supports, so an
// embedding daemon can refuse a binary that is too old. Add a name only when
// the feature works end to end.
var runtimeCapabilities = []string{
	"stream-json", "mcp-http", "mcp-config", "prompt-stdin",
	"system-prompt", "max-turns", "thinking", "ephemeral",
	"mcp-status", "require-mcp", "skill-sources", "project-docs-budget", "policy-file",
}

type versionInfo struct {
	Version      string   `json:"version"`
	Commit       string   `json:"commit"`
	BuildDate    string   `json:"build_date"`
	GoVersion    string   `json:"go_version"`
	Platform     string   `json:"platform"`
	Capabilities []string `json:"capabilities"`
}

func currentVersionInfo() versionInfo {
	return versionInfo{
		Version: Version, Commit: Commit, BuildDate: BuildDate,
		GoVersion: runtime.Version(), Platform: runtime.GOOS + "/" + runtime.GOARCH,
		Capabilities: append([]string(nil), runtimeCapabilities...),
	}
}

func printVersion() error {
	// --json is a global flag consumed by stripGlobalFlags.
	if jsonMode {
		return json.NewEncoder(os.Stdout).Encode(currentVersionInfo())
	}
	fmt.Printf("chronos-code %s (%s) built %s\n", Version, Commit, BuildDate)
	fmt.Printf("go: %s\n", runtime.Version())
	fmt.Printf("os/arch: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	return nil
}

func printUsage() error {
	fmt.Print(`chronos-code — AI coding agent harness built on Chronos

Usage:
  chronos-code                    Start interactive REPL (default)
  chronos-code run <message>      Execute a single task and exit
  chronos-code init               Initialize .chronos-code/ in current project
  chronos-code login <provider>    Sign in with a built-in provider (e.g. anthropic)
  chronos-code login <provider> --api-key <key>  Store a BYO API key
  chronos-code logout <provider>   Remove a stored credential
  chronos-code whoami [provider]   Show the effective credential source
  chronos-code providers          List built-in and resolvable providers
  chronos-code models [provider]  List live models, with an explicit static fallback
  chronos-code models refresh     Refresh the models.dev catalog (prices, context windows, picker)
  chronos-code agents list        List resolved built-in, user, and project agents
  chronos-code config show        Print resolved configuration
  chronos-code config validate    Validate all config files
  chronos-code auth login <provider> --api-key <key>   Store a BYO API key
  chronos-code auth login <provider> --oauth-pkce ...   Authorization Code + PKCE login
  chronos-code auth login <provider> --device-code ...  Device Authorization Grant login
  chronos-code auth logout <provider>                   Remove a stored credential
  chronos-code auth status [provider]                   Show authentication state
  chronos-code auth refresh <provider> ...              Refresh an OAuth credential
  chronos-code auth whoami [provider]                   Show the effective credential source
  chronos-code auth providers                           List built-in and resolvable providers
  chronos-code session list [agent]                     List sessions
  chronos-code session delete <id>                      Delete a session and its history
  chronos-code session export <id> <path>                Export a session to JSON
  chronos-code memory list [category]                   List remembered notes
  chronos-code memory search <query>                    Search remembered notes
  chronos-code memory forget <id>                        Remove a remembered note
  chronos-code memory layers list --scope <scope> [--kind <kind>]
  chronos-code memory layers search <query> --scope <scope> [--kind <kind>]
  chronos-code memory layers forget <id> --scope <scope>  Invalidate a layered memory
  chronos-code memory layers --help                      Show scopes, kinds, and retrieval limits
  chronos-code mcp add <name> --command <cmd> [--arg <arg> ...] [--scope project|user]
  chronos-code mcp add <name> --url <https-url> [--scope project|user]
  chronos-code mcp list [--scope project|user]           List canonical MCP servers with secrets redacted
  chronos-code mcp remove <name> [--scope project|user]  Remove a canonical MCP server
  chronos-code mcp test <name> [--timeout 10s] [--scope project|user]  Initialize, list tools, and close
  MCP transports: stdio, HTTPS SSE, and streamable HTTP (--transport stdio|sse|streamable-http). Credential values must remain ${ENV_VAR} references.
  chronos-code indexer mcp [--repo [name=]dir ...]        Serve the code index's graph tools to MCP hosts over stdio
  chronos-code learn suggest [agent]                     Distill traced sessions into a reviewable suggestion
  chronos-code learn list                                List pending suggestions
  chronos-code learn show <id>                           Show a suggestion's full YAML and rationale
  chronos-code learn accept <id>                         Apply a suggestion (agent or pattern)
  chronos-code learn reject <id>                          Discard a suggestion
  chronos-code eval run [--update-baseline] [--md <path>]  Run the token-efficiency eval suite
  chronos-code eval ppd --validate-only                    Validate the PPD benchmark registration
  chronos-code eval ppd --report [--baseline <path>]       Report and gate completed PPD evidence
  chronos-code team list                                   List configured teams
  chronos-code team run <team_id> <message>                Run a team on a task
  chronos-code plan <operation> --db <path> ...             Inspect or operate a durable plan database
  chronos-code delivery backup <path> | restore <source> <new-path>  Offline delivery database snapshots
  chronos-code skills list                                  List discovered skills (project + user + bundled)
  chronos-code skills show <name>                           Show a skill's metadata and body
  chronos-code cleanup status [scope]                       Inventory retention-managed resources
  chronos-code cleanup run [--dry-run] [--scope <scope>]    Apply one bounded cleanup batch per scope
  chronos-code cleanup prune <scope> [--dry-run]             Prune one scope (plan_db also requires tenant/repository)
  chronos-code serve [--listen :8430] [--auth api_key] [--tenant-id <id>] [--request-timeout 5m] [--instance-id <id>]  Start HTTP server for team deployment
  chronos-code serve --delivery-read-only-worker            Process explicitly queued deliveries as read-only, then park for verification
  chronos-code serve --delivery-plan-worker                 Also run admitted plan generations in private worktrees; retain candidate patches, never write the checkout
  chronos-code version, --version  Print version information
  chronos-code help               Show this help

Global flags:
  -c, --config <path>             Path to config file
  --provider <name>               Override the primary agent provider (env: CHRONOS_CODE_PROVIDER)
  --model <id>                    Override the primary agent model (env: CHRONOS_CODE_MODEL)
  --debug                         Enable debug logging
  -s, --stream                    Enable streaming output
  --no-stream                     Disable streaming output
  --permission-mode <mode>        Tool permission mode (prompt, auto_approve, deny)
  --yolo                          Auto-approve policy-allowed tools; never overrides deny or destructive confirm
  --budget <usd>                  Per-session USD cap (up to 6 decimal places; omitted means unlimited)
  --resume <session-id>           Resume a specific prior session
  --json                          Headless run: print one JSON object and exit
  --dangerously-skip-permissions  Run every tool call without asking, including shell commands outside the
                                  allowlist and MCP tools; policy blocks still apply. Use only in sandboxes/CI
  --mcp-connect <name>            Approve and connect a discovered MCP server for this session (repeatable or
                                  comma-separated); the non-interactive form of /mcp connect
  --plan-mode                     Start in plan mode: plan first, then implement once the plan is approved
                                  (the TUI asks for approval; headless runs auto-approve)
`)
	return nil
}

func runAgents() error {
	if len(os.Args) < 3 || os.Args[2] != "list" || len(os.Args) > 3 {
		return fmt.Errorf("usage: chronos-code agents list")
	}
	cfg, err := loadConfigForInspection()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	for _, configured := range cfg.Agents {
		fmt.Printf("%-16s %-24s %s/%s\n", configured.ID, configured.Name, configured.Model.Provider, configured.Model.Model)
	}
	return nil
}

func loadAndBuild() (*orchestrator.Orchestrator, error) {
	orch, _, err := loadConfigAndBuild()
	return orch, err
}

func loadConfigAndBuild() (*orchestrator.Orchestrator, *config.Config, error) {
	cfg, err := loadConfigWithModelSelection()
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}
	initModelsCatalog(cfg, true)
	if resumeSessionID != "" {
		if err := locateResumeSession(context.Background(), cfg, resumeSessionID); err != nil {
			return nil, nil, err
		}
	}
	cfg.MCP.CallerConfigs = mcpConfigFiles
	cfg.MCP.Strict = strictMCPConfig
	cfg.Ephemeral = ephemeralRun || os.Getenv("CHRONOS_CODE_EPHEMERAL") == "1"
	cfg.SkillSources = skillSourcesRun
	cfg.ProjectDocsBudget = projectDocsBudget
	cfg.PolicyFiles = policyFilesRun
	if cfg.SkillSources == "" {
		cfg.SkillSources = os.Getenv("CHRONOS_CODE_SKILL_SOURCES")
	}
	if _, err := skills.ParseSources(cfg.SkillSources); err != nil {
		return nil, nil, fmt.Errorf("CHRONOS_CODE_SKILL_SOURCES: %w", err)
	}
	ctx := context.Background()
	orch, err := orchestrator.New(ctx, cfg, resumeSessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("build orchestrator: %w", err)
	}
	if usdBudgetSet {
		orch.SetUSDCap(usdBudgetCap)
	}
	if err := orch.SetPermissionMode(effectivePermissionMode()); err != nil {
		_ = orch.Close()
		return nil, nil, fmt.Errorf("apply --permission-mode: %w", err)
	}
	if planModeFlag {
		orch.SetPlanMode(true)
	}
	if skipPermissions {
		orch.SetSkipPermissions(true)
		fmt.Fprintln(os.Stderr, "warning: --dangerously-skip-permissions: every tool call runs without asking (policy blocks and paths outside the workspace are still refused)")
	}
	return orch, cfg, nil
}

// errProviderModelRequired marks a provider selection that cannot name a
// model (for example Azure without a deployment). Read-only commands such as
// `models` and `config show` fall back to the unselected config on it so the
// user can still inspect what to set.
var errProviderModelRequired = errors.New("provider has no model")

// providerModelError carries the user-facing explanation while matching
// errProviderModelRequired under errors.Is.
type providerModelError struct{ msg string }

func (e *providerModelError) Error() string { return e.msg }
func (e *providerModelError) Unwrap() error { return errProviderModelRequired }

func loadConfigWithModelSelection() (*config.Config, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	provider, providerSource := providerOverride, "flag:--provider"
	modelID, modelSource := modelOverride, "flag:--model"
	if !providerOverrideSet {
		provider = strings.TrimSpace(os.Getenv("CHRONOS_CODE_PROVIDER"))
		if provider != "" {
			providerSource = "env:CHRONOS_CODE_PROVIDER"
		} else {
			providerSource = ""
		}
	}
	if !modelOverrideSet {
		modelID = strings.TrimSpace(os.Getenv("CHRONOS_CODE_MODEL"))
		if modelID != "" {
			modelSource = "env:CHRONOS_CODE_MODEL"
		} else {
			modelSource = ""
		}
	}
	store := auth.NewStore()
	authorized := func(provider string) bool {
		return auth.Resolve(context.Background(), store, provider).Token != ""
	}
	provider = auth.CanonicalProvider(provider)
	if provider == "" {
		if prefix, rest, ok := splitModelProviderPrefix(modelID, cfg); ok {
			provider, modelID = prefix, rest
			providerSource = "model prefix"
		}
	}
	if provider == "" && modelID == "" {
		_, current, _, _ := cfg.PrimaryAgentModel()
		provider = credentialProvider(cfg, current, authorized)
		if provider == "" {
			return cfg, nil
		}
		providerSource = "auto:only authorized provider"
		if provider == "azure" && azureEnvConfigured() {
			providerSource = "auto:azure environment"
		}
	}
	_, current, _, _ := cfg.PrimaryAgentModel()
	if provider == "" {
		if info, ok := modelinfo.LookupByModel(modelID); ok {
			provider = info.Provider
			providerSource = "model:catalog"
		} else {
			provider = auth.CanonicalProvider(current.Provider)
			providerSource = ""
			var owners, authorizedOwners []string
			for _, info := range modelinfo.All() {
				owner := auth.CanonicalProvider(info.Provider)
				if info.Model != modelID || slices.Contains(owners, owner) {
					continue
				}
				owners = append(owners, owner)
				if authorized(owner) {
					authorizedOwners = append(authorizedOwners, owner)
				}
			}
			switch {
			case slices.Contains(owners, provider):
				// The configured provider serves this model.
			case len(owners) == 1:
				provider, providerSource = owners[0], "model:catalog"
			case len(authorizedOwners) == 1:
				provider, providerSource = authorizedOwners[0], "model:catalog (only authorized provider)"
			case len(owners) > 1:
				return nil, fmt.Errorf("model %q is not unique to a provider; supply --provider or CHRONOS_CODE_PROVIDER", modelID)
			default:
				// An unlisted model (typically a custom Azure deployment
				// name) cannot be placed from the catalog. If the configured
				// provider has no credential and exactly one other does,
				// that is the only provider that can serve it.
				if alt := credentialProvider(cfg, current, authorized); alt != "" {
					provider, providerSource = alt, "auto:only authorized provider"
				}
			}
		}
	}
	if modelID == "" && provider != auth.CanonicalProvider(current.Provider) {
		for _, configured := range cfg.Agents {
			if auth.CanonicalProvider(configured.Model.Provider) == provider && configured.Model.Model != "" {
				modelID = configured.Model.Model
				modelSource = "configured agent " + configured.ID
				break
			}
		}
		if modelID == "" {
			modelID = defaultProviderModel(provider)
			if modelID == "" {
				return nil, providerModelRequiredError(provider, authorized(provider))
			}
			modelSource = "provider default"
		}
	}
	if err := cfg.OverridePrimaryModel(provider, modelID, providerSource, modelSource); err != nil {
		return nil, err
	}
	// An explicit model pins the run: the caller does its own model routing,
	// so the built-in router must not move the turn to another tier.
	if modelSource != "" {
		cfg.Router.Enabled = false
	}
	return cfg, nil
}

// providerModelRequiredError explains how to name a model for a provider that
// has no usable default. Azure is special: it has no default because the
// model is the user's own deployment name.
func providerModelRequiredError(provider string, hasCredential bool) error {
	if provider == "azure" {
		msg := "azure needs a deployment name: set AZURE_OPENAI_DEPLOYMENT, or supply --model <deployment> or CHRONOS_CODE_MODEL"
		if !hasCredential {
			msg += "; no Azure API key was found either (set AZURE_OPENAI_API_KEY or run chronos-code login azure)"
		}
		return &providerModelError{msg: msg}
	}
	return &providerModelError{msg: fmt.Sprintf("provider %q does not have a configured model; supply --model or CHRONOS_CODE_MODEL (see chronos-code models %s)", provider, provider)}
}

// loadConfigForInspection is loadConfigWithModelSelection for commands that
// only read configuration (models, config show, agents list). When the
// selection cannot name a model it warns and returns the unselected config
// rather than failing, since these commands are how a user finds the model.
func loadConfigForInspection() (*config.Config, error) {
	cfg, err := loadConfigWithModelSelection()
	if errors.Is(err, errProviderModelRequired) {
		fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		return config.Load(configPath)
	}
	return cfg, err
}

// azureEnvConfigured reports whether Azure-specific environment variables are
// set. They are opt-in signals, unlike a bare OPENAI_API_KEY that many tools
// export.
func azureEnvConfigured() bool {
	for _, name := range []string{"AZURE_OPENAI_ENDPOINT", "AZURE_OPENAI_BASE_URL", "AZURE_OPENAI_DEPLOYMENT"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

// splitModelProviderPrefix recognizes "<provider>/<model>" (for example
// azure/my-deployment or anthropic/claude-sonnet-4-5) when the prefix names a
// known provider. Unknown prefixes are left alone because some model IDs
// legitimately contain a slash.
func splitModelProviderPrefix(modelID string, cfg *config.Config) (provider, model string, ok bool) {
	prefix, rest, found := strings.Cut(modelID, "/")
	if !found || prefix == "" || rest == "" {
		return "", "", false
	}
	canonical := auth.CanonicalProvider(prefix)
	known := map[string]bool{"anthropic": true, "openai": true, "azure": true}
	for _, name := range configuredProviders(cfg) {
		known[name] = true
	}
	for _, info := range modelinfo.All() {
		known[auth.CanonicalProvider(info.Provider)] = true
	}
	if !known[canonical] {
		return "", "", false
	}
	return canonical, rest, true
}

// Credentials select a provider only when the configured primary has no usable
// credential and exactly one other provider is authorized. An explicit model
// or provider flag/env always wins; multiple credentials leave YAML in charge,
// except that Azure wins when its own environment (endpoint or deployment) is
// set, and an Azure deployment with no credential anywhere selects Azure so the
// failure names Azure instead of surfacing as an Anthropic auth error.
func credentialProvider(cfg *config.Config, current agent.ModelConfig, authorized func(string) bool) string {
	if current.APIKey != "" || authorized(current.Provider) {
		return ""
	}
	providers := configuredProviders(cfg)
	seen := make(map[string]bool, len(providers))
	for _, provider := range providers {
		seen[provider] = true
	}
	for _, info := range modelinfo.All() {
		if !seen[info.Provider] {
			providers = append(providers, info.Provider)
			seen[info.Provider] = true
		}
	}
	var candidates []string
	for _, provider := range providers {
		if provider == "" || provider == auth.CanonicalProvider(current.Provider) || !authorized(provider) {
			continue
		}
		candidates = append(candidates, provider)
	}
	switch {
	case len(candidates) == 1:
		return candidates[0]
	case len(candidates) > 1:
		if slices.Contains(candidates, "azure") && azureEnvConfigured() {
			return "azure"
		}
		return ""
	case auth.CanonicalProvider(current.Provider) != "azure" && strings.TrimSpace(os.Getenv("AZURE_OPENAI_DEPLOYMENT")) != "":
		// A deployment name is deliberate (unlike an ambient endpoint), and
		// without a credential nothing else could run anyway.
		return "azure"
	}
	return ""
}

// Provider-only selection uses a known API default, not the arbitrary first
// result of a live model list (which has no portable ranking or availability
// guarantee). Other providers need a configured model or an explicit ID.
func defaultProviderModel(provider string) string {
	switch provider {
	case "anthropic":
		return "claude-sonnet-5-5"
	case "openai":
		return "gpt-4o"
	case "gemini", "google":
		return "gemini-2.0-flash"
	case "mistral":
		return "mistral-large-latest"
	case "azure":
		return strings.TrimSpace(os.Getenv("AZURE_OPENAI_DEPLOYMENT"))
	default:
		return ""
	}
}

func effectivePermissionMode() string {
	if yoloMode {
		return "auto_approve"
	}
	return permissionMode
}

func runREPL() error {
	orch, err := loadAndBuild()
	if err != nil {
		return err
	}
	defer orch.Close()
	if err := connectRequestedMCP(context.Background(), orch, os.Stderr); err != nil {
		return err
	}

	return tui.RunTUI(orch, streamMode)
}

// connectRequestedMCP approves and connects the servers named with
// --mcp-connect for this session: the non-interactive equivalent of the
// TUI's /mcp connect. Trust is exact-identity and in-memory, as there.
func connectRequestedMCP(ctx context.Context, orch *orchestrator.Orchestrator, stderr io.Writer) error {
	for _, name := range mcpConnectNames {
		status, err := orch.ConnectMCP(ctx, name)
		if err != nil {
			return fmt.Errorf("--mcp-connect %s: %w", name, err)
		}
		fmt.Fprintf(stderr, "mcp: connected %s (%d tools)\n", status.Name, status.Tools)
	}
	return nil
}

// explicitSkillInvocation reports whether message is "/<skill> <task>" for a
// discovered skill, as the TUI accepts, returning the skill and the task.
func explicitSkillInvocation(orch *orchestrator.Orchestrator, message string) (skill, task string, ok bool) {
	if !strings.HasPrefix(message, "/") {
		return "", "", false
	}
	parts := strings.SplitN(message, " ", 2)
	name := strings.TrimPrefix(parts[0], "/")
	for _, info := range orch.ListSkills() {
		if strings.EqualFold(info.Name, name) {
			if len(parts) == 2 {
				task = strings.TrimSpace(parts[1])
			}
			return info.Name, task, true
		}
	}
	return "", "", false
}

func runHeadless() error {
	opts, words, err := parseRunFlags(os.Args[2:])
	if err != nil {
		return headlessEarlyFailure(opts, os.Stdout, err)
	}
	if opts.help {
		return printUsage()
	}
	if opts.outputFormat == outputJSON {
		jsonMode = true
	}
	message, err := resolvePrompt(opts, words, os.Stdin)
	if err != nil {
		return headlessEarlyFailure(opts, os.Stdout, err)
	}
	systemPrompt, err := resolveSystemPrompt(opts)
	if err != nil {
		return headlessEarlyFailure(opts, os.Stdout, err)
	}

	// SIGINT/SIGTERM cancel the run; the terminal event is still written and
	// a watchdog bounds how long shutdown may take.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	go cancelWatchdog(ctx, cancelGracePeriod, os.Exit)

	mcpConfigFiles, strictMCPConfig, ephemeralRun, skillSourcesRun = opts.mcpConfigs, opts.strictMCPConfig, opts.ephemeral, opts.skillSources
	projectDocsBudget, policyFilesRun = opts.projectDocsBudget, opts.policyFiles
	orch, err := loadAndBuild()
	if errors.Is(err, errSessionNotFound) {
		if opts.streamJSON() {
			newStreamEmitter(os.Stdout).terminateInvalid(execution.ErrorSessionNotFound, execution.ErrorCategoryRequest, err.Error(), execution.StatusInvalidRequest, execution.StopInvalidRequest)
		}
		return &ExitError{Code: ExitSessionNotFound, Err: err}
	}
	if err != nil {
		return headlessLoadFailure(opts, os.Stdout, err)
	}
	defer orch.Close()
	orch.AppendSystemPrompt(systemPrompt)
	if opts.thinking != "" {
		if err := orch.SetThinking(opts.thinking); err != nil {
			return headlessEarlyFailure(opts, os.Stdout, err)
		}
	}

	if err := connectRequestedMCP(ctx, orch, os.Stderr); err != nil {
		return headlessEarlyFailure(opts, os.Stdout, err)
	}

	if strings.HasPrefix(message, "@") {
		parts := strings.SplitN(message[1:], " ", 2)
		if len(parts) == 2 {
			if err := orch.SwitchAgent(parts[0]); err != nil {
				return headlessEarlyFailure(opts, os.Stdout, err)
			}
			message = parts[1]
		}
	}

	if name, task, ok := explicitSkillInvocation(orch, message); ok {
		if task == "" {
			return headlessEarlyFailure(opts, os.Stdout, fmt.Errorf("usage: chronos-code run \"/%s <task>\"", name))
		}
		if ctx, err = orch.WithSkill(ctx, name); err != nil {
			return headlessEarlyFailure(opts, os.Stdout, err)
		}
		message = task
	}

	request := orchestrator.ExecutionRequest{Message: message, VerificationMode: orch.VerificationMode(), MaxTurns: opts.maxTurns}
	if opts.streamJSON() {
		cwd, _ := os.Getwd()
		return runStreamJSON(ctx, orch, request, newStreamEmitter(os.Stdout), cwd, opts.requiredMCP)
	}
	if err := requiredMCPFailure(mcpStatusReport(orch, opts.requiredMCP), opts.requiredMCP); err != nil {
		return headlessMCPUnavailable(os.Stdout, orch.CurrentSessionID(), err)
	}
	if jsonMode {
		request.Mode = orchestrator.ExecutionBlocking
		return RunJSONExecution(ctx, orch, request, os.Stdout)
	}
	if streamMode {
		request.Mode = orchestrator.ExecutionStreaming
	}
	_, err = RunExecution(ctx, orch, request, os.Stdout, os.Stderr)
	return err
}

// headlessEarlyFailure reports a failure that happened before execution
// started (bad flags, unreadable prompt) in the selected output format. The
// message is a caller error, so the status is invalid_request.
func headlessEarlyFailure(opts runOptions, stdout io.Writer, err error) error {
	switch {
	case opts.streamJSON():
		newStreamEmitter(stdout).terminateInvalid(execution.ErrorInvalidRequest, execution.ErrorCategoryRequest, err.Error(), execution.StatusInvalidRequest, execution.StopInvalidRequest)
		return &ExitError{Code: ExitInvalidRequest, Err: err}
	case jsonMode:
		return writeInvalidJSONResult(stdout, err.Error())
	default:
		return err
	}
}

// headlessLoadFailure reports a configuration or startup failure.
func headlessLoadFailure(opts runOptions, stdout io.Writer, err error) error {
	envelope := orchestrator.ExecutionEnvelope(orchestrator.ExecutionResult{}, err, orchestrator.EnvelopeMetadata{})
	switch {
	case opts.streamJSON():
		newStreamEmitter(stdout).terminateWithEnvelope(envelope)
		if envelope.Status == execution.StatusSucceeded {
			return err
		}
		return &ExitError{Code: ExitCodeForStatus(envelope.Status), Err: err}
	case jsonMode:
		return writeJSONResult(stdout, envelope, err)
	default:
		return err
	}
}

// headlessMCPUnavailable reports a failed --require-mcp check for the text
// and json output formats.
func headlessMCPUnavailable(stdout io.Writer, sessionID string, err error) error {
	if jsonMode {
		failure := mcpUnavailableError(err)
		envelope := execution.ExecutionEnvelope{
			SchemaVersion: execution.SchemaVersionV1, SessionID: sessionID, Status: execution.StatusFailed, StopReason: execution.StopMCPUnavailable,
			ChangedPaths: []string{}, Verification: execution.EnvelopeVerification{Status: execution.VerificationPending, Obligations: []execution.VerificationObligation{}},
			Error: &failure,
		}
		encoder := json.NewEncoder(stdout)
		encoder.SetEscapeHTML(false)
		if encodeErr := encoder.Encode(envelope); encodeErr != nil {
			return fmt.Errorf("encode execution result: %w", encodeErr)
		}
	}
	return &ExitError{Code: ExitMCPUnavailable, Err: err}
}

// cancelGracePeriod bounds shutdown after SIGINT/SIGTERM.
const cancelGracePeriod = 10 * time.Second

// cancelWatchdog forces the process to exit with ExitCancelled when shutdown
// takes longer than grace after ctx is cancelled.
func cancelWatchdog(ctx context.Context, grace time.Duration, exit func(int)) {
	<-ctx.Done()
	time.Sleep(grace)
	exit(ExitCancelled)
}

// ExitError carries the stable process status for autonomous callers.
type ExitError struct {
	Code int
	Err  error
}

const (
	ExitSuccess             = 0
	ExitFailure             = 1
	ExitInvalidRequest      = 2
	ExitApprovalBlocked     = 3
	ExitRetryableProvider   = 4
	ExitTimeout             = 5
	ExitBudgetExhausted     = 6
	ExitVerificationFailure = 7
	// ExitMCPUnavailable: a server named with --require-mcp did not connect.
	ExitMCPUnavailable = 9
	// ExitCancelled follows the shell convention for SIGINT (128+2).
	ExitCancelled = 130
)

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("execution exited with status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }
func (e *ExitError) ExitCode() int { return e.Code }

// RunJSONExecution emits exactly one ExecutionEnvelope document.
// A plan approved during a plan-mode run (headless runs auto-approve) is
// implemented in a second execution; the envelope reports that execution.
func RunJSONExecution(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest, stdout io.Writer) error {
	request.Mode = orchestrator.ExecutionBlocking
	result, err := orch.Execute(ctx, request)
	if plan, approved := orch.TakeApprovedPlan(); approved && err == nil {
		request.Message = orchestrator.ImplementApprovedPlanMessage(plan)
		result, err = orch.Execute(ctx, request)
	}
	envelope := orchestrator.ExecutionEnvelope(result, err, orchestrator.EnvelopeMetadata{})
	return writeJSONResult(stdout, envelope, err)
}

func writeInvalidJSONResult(stdout io.Writer, message string) error {
	envelope := execution.ExecutionEnvelope{
		SchemaVersion: execution.SchemaVersionV1, Status: execution.StatusInvalidRequest, StopReason: execution.StopInvalidRequest,
		Content: "", ChangedPaths: []string{}, Verification: execution.EnvelopeVerification{Status: execution.VerificationPending, Obligations: []execution.VerificationObligation{}},
		Error: &execution.Error{Code: execution.ErrorInvalidRequest, Category: execution.ErrorCategoryRequest, Message: message},
	}
	return writeJSONResult(stdout, envelope, fmt.Errorf("%s", message))
}

func writeJSONResult(stdout io.Writer, envelope execution.ExecutionEnvelope, executionErr error) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(envelope); err != nil {
		return fmt.Errorf("encode execution result: %w", err)
	}
	if envelope.Status == execution.StatusSucceeded {
		return nil
	}
	return &ExitError{Code: ExitCodeForStatus(envelope.Status), Err: executionErr}
}

func ExitCodeForStatus(status execution.Status) int {
	switch status {
	case execution.StatusSucceeded:
		return ExitSuccess
	case execution.StatusInvalidRequest:
		return ExitInvalidRequest
	case execution.StatusApprovalBlocked:
		return ExitApprovalBlocked
	case execution.StatusRetryableProvider:
		return ExitRetryableProvider
	case execution.StatusTimedOut:
		return ExitTimeout
	case execution.StatusBudgetExhausted:
		return ExitBudgetExhausted
	case execution.StatusVerificationFailed:
		return ExitVerificationFailure
	case execution.StatusCancelled:
		return ExitCancelled
	default:
		return ExitFailure
	}
}

// RunExecution executes and renders one headless CLI request. It is kept
// separate from argument/config handling so the adapter can be exercised with
// a deterministic orchestrator. A plan approved during a plan-mode run
// (headless runs auto-approve) is implemented in a follow-up execution.
func RunExecution(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest, stdout, stderr io.Writer) (orchestrator.ExecutionResult, error) {
	result, err := runExecutionOnce(ctx, orch, request, stdout, stderr)
	plan, approved := orch.TakeApprovedPlan()
	if err != nil || !approved {
		return result, err
	}
	fmt.Fprintln(stderr, "plan approved · plan mode off · implementing")
	request.Message = orchestrator.ImplementApprovedPlanMessage(plan)
	return runExecutionOnce(ctx, orch, request, stdout, stderr)
}

func runExecutionOnce(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest, stdout, stderr io.Writer) (orchestrator.ExecutionResult, error) {
	result, err := orch.Execute(ctx, request)
	if err != nil {
		return result, err
	}
	if request.Mode == orchestrator.ExecutionStreaming {
		usage, streamErr := tui.StreamResponse(result.Stream, stdout)
		completion, completed := <-result.Completion
		if completed {
			result = orchestrator.ApplyCompletion(result, completion)
		}
		if streamErr != nil {
			return result, streamErr
		}
		if completed && completion.Err != nil {
			return result, completion.Err
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		tui.PrintUsage(usage, stderr)
		return result, nil
	}
	tui.PrintResponse(result.Response, stdout)
	return result, nil
}

func runConfig() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code config [show|validate]")
	}
	switch os.Args[2] {
	case "show":
		cfg, err := loadConfigForInspection()
		if err != nil {
			return err
		}
		primaryID, primaryModel, providerSource, modelSource := cfg.PrimaryAgentModel()
		fmt.Printf("Primary:   %s\n", primaryID)
		fmt.Printf("Provider:  %s (source: %s)\n", primaryModel.Provider, providerSource)
		fmt.Printf("Model:     %s (source: %s)\n", primaryModel.Model, modelSource)
		fmt.Printf("Storage:   %s\n", resolveStorage(cfg))
		fmt.Printf("Agents:    %d\n", len(cfg.Agents))
		for _, a := range cfg.Agents {
			counter := model.NewTokenCounter(a.Model.Model)
			tokens := counter.CountString(a.System)
			budget := systemPromptTokenBudget
			if a.ID == orchestrator.DefaultPrimaryAgentID {
				budget = primarySystemPromptTokenBudget
			}
			flag := ""
			if tokens > budget {
				flag = fmt.Sprintf(" [over %d-token budget]", budget)
			}
			fmt.Printf("  - %s: %s (%d tokens%s)\n", a.ID, a.Name, tokens, flag)
		}
		return nil
	case "validate":
		_, err := config.Load(configPath)
		if err != nil {
			return fmt.Errorf("validation failed: %w", err)
		}
		fmt.Println("config is valid")
		return nil
	default:
		return fmt.Errorf("unknown config command: %s", os.Args[2])
	}
}

func runModels() error {
	if len(os.Args) >= 3 && os.Args[2] == "refresh" {
		return runModelsRefresh()
	}
	if len(os.Args) > 3 {
		return fmt.Errorf("usage: chronos-code models [provider] | models refresh")
	}
	cfg, err := loadConfigForInspection()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	initModelsCatalog(cfg, false)
	requested := ""
	if len(os.Args) == 3 {
		requested = auth.CanonicalProvider(os.Args[2])
	}
	providers := configuredProviders(cfg)
	if requested != "" {
		providers = []string{requested}
	} else {
		seen := make(map[string]struct{}, len(providers))
		for _, provider := range providers {
			seen[provider] = struct{}{}
		}
		for _, info := range modelinfo.All() {
			provider := auth.CanonicalProvider(info.Provider)
			if _, ok := seen[provider]; ok || auth.Resolve(context.Background(), auth.NewStore(), provider).Token == "" {
				continue
			}
			seen[provider] = struct{}{}
			providers = append(providers, provider)
		}
		sort.Strings(providers)
	}
	if len(providers) == 0 {
		return fmt.Errorf("no configured providers")
	}
	store := auth.NewStore()
	ctx := context.Background()
	type jsonProviderModels struct {
		Provider string   `json:"provider"`
		Status   string   `json:"status"`
		Models   []string `json:"models"`
	}
	var jsonOut []jsonProviderModels
	for _, provider := range providers {
		resolved := auth.Resolve(ctx, store, provider)
		modelCfg := configuredProviderModel(cfg, provider)
		apiKey := modelCfg.APIKey
		if apiKey == "" {
			apiKey = resolved.Token
		}
		var list []modelinfo.Info
		status := "static (live unavailable: not authorized)"
		if apiKey != "" {
			list, err = modelinfo.FetchLive(ctx, provider, apiKey, modelinfo.LiveConfig{
				BaseURL:  modelCfg.BaseURL,
				Endpoint: modelCfg.Endpoint,
			})
			if err == nil {
				if len(list) > 0 {
					status = "live"
				} else {
					status = "static (live unavailable: no models returned)"
				}
			} else {
				status = "static (live unavailable: " + err.Error() + ")"
			}
		}
		if len(list) == 0 {
			list = staticProviderModels(cfg, provider)
		}
		if jsonMode {
			entry := jsonProviderModels{Provider: provider, Status: status, Models: make([]string, 0, len(list))}
			for _, info := range list {
				entry.Models = append(entry.Models, info.Model)
			}
			jsonOut = append(jsonOut, entry)
			continue
		}
		fmt.Printf("%s [%s]\n", provider, status)
		for _, info := range list {
			fmt.Printf("  %s\n", info.Model)
		}
	}
	if jsonMode {
		return json.NewEncoder(os.Stdout).Encode(map[string]any{"providers": jsonOut})
	}
	return nil
}

func configuredProviders(cfg *config.Config) []string {
	set := make(map[string]struct{})
	for _, configured := range cfg.Agents {
		if provider := auth.CanonicalProvider(configured.Model.Provider); provider != "" {
			set[provider] = struct{}{}
		}
	}
	for provider := range cfg.Providers {
		set[auth.CanonicalProvider(provider)] = struct{}{}
	}
	providers := make([]string, 0, len(set))
	for provider := range set {
		providers = append(providers, provider)
	}
	sort.Strings(providers)
	return providers
}

func configuredProviderModel(cfg *config.Config, provider string) agent.ModelConfig {
	provider = auth.CanonicalProvider(provider)
	for _, configured := range cfg.Agents {
		if auth.CanonicalProvider(configured.Model.Provider) == provider {
			mc := configured.Model
			for name, override := range cfg.Providers {
				if auth.CanonicalProvider(name) == provider && override.BaseURL != "" {
					mc.BaseURL = override.BaseURL
					break
				}
			}
			return mc
		}
	}
	mc := agent.ModelConfig{Provider: provider}
	for name, override := range cfg.Providers {
		if auth.CanonicalProvider(name) == provider && override.BaseURL != "" {
			mc.BaseURL = override.BaseURL
			break
		}
	}
	return mc
}

func staticProviderModels(cfg *config.Config, provider string) []modelinfo.Info {
	provider = auth.CanonicalProvider(provider)
	var list []modelinfo.Info
	seen := make(map[string]struct{})
	for _, info := range modelinfo.All() {
		if auth.CanonicalProvider(info.Provider) == provider {
			list = append(list, info)
			seen[info.Model] = struct{}{}
		}
	}
	for _, configured := range cfg.Agents {
		if auth.CanonicalProvider(configured.Model.Provider) == provider && configured.Model.Model != "" {
			if _, ok := seen[configured.Model.Model]; !ok {
				list = append(list, modelinfo.Info{Provider: provider, Model: configured.Model.Model})
				seen[configured.Model.Model] = struct{}{}
			}
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Model < list[j].Model })
	return list
}

// flagValue scans args for "--name value" or "--name=value" and returns the
// value, or def if not present.
func flagValue(args []string, name, def string) string {
	prefix := "--" + name
	for i, a := range args {
		if a == prefix && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, prefix+"=") {
			return strings.TrimPrefix(a, prefix+"=")
		}
	}
	return def
}

func hasFlag(args []string, name string) bool {
	prefix := "--" + name
	for _, a := range args {
		if a == prefix || strings.HasPrefix(a, prefix+"=") {
			return true
		}
	}
	return false
}

func oauthConfigFromFlags(provider string, args []string) auth.ProviderOAuthConfig {
	scopes := flagValue(args, "scopes", "")
	var scopeList []string
	if scopes != "" {
		scopeList = strings.Split(scopes, ",")
	}
	port := 8765
	if p := flagValue(args, "port", ""); p != "" {
		fmt.Sscanf(p, "%d", &port)
	}
	return auth.ProviderOAuthConfig{
		Provider:     provider,
		ClientID:     flagValue(args, "client-id", ""),
		AuthURL:      flagValue(args, "auth-url", ""),
		TokenURL:     flagValue(args, "token-url", ""),
		Scopes:       scopeList,
		RedirectPort: port,
	}
}

func runLogin() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code login <provider> [--api-key <key>]")
	}
	provider := auth.CanonicalProvider(os.Args[2])
	rest := os.Args[3:]

	if hasFlag(rest, "api-key") {
		key := flagValue(rest, "api-key", "")
		if key == "" {
			return fmt.Errorf("--api-key requires a value")
		}
		store := auth.NewStore()
		if err := auth.LoginAPIKey(store, provider, key); err != nil {
			return err
		}
		fmt.Printf("stored API key for %q\n", provider)
		return nil
	}

	cfg, ok := auth.LookupProvider(provider)
	if !ok {
		return fmt.Errorf("no built-in OAuth config for %q; use: chronos-code auth login %s --oauth-pkce --client-id <id> --auth-url <url> --token-url <url>", provider, provider)
	}
	store := auth.NewStore()
	ctx := context.Background()
	fmt.Printf("signing in to %s via OAuth PKCE...\n", provider)
	if err := auth.LoginPKCE(ctx, store, cfg, func(url string) {
		fmt.Printf("open this URL to sign in:\n  %s\n", url)
	}); err != nil {
		return err
	}
	fmt.Printf("logged in to %q via OAuth PKCE\n", provider)
	return nil
}

func runLogout() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code logout <provider>")
	}
	provider := auth.CanonicalProvider(os.Args[2])
	store := auth.NewStore()
	if err := auth.Logout(store, provider); err != nil {
		return err
	}
	fmt.Printf("logged out of %q\n", provider)
	return nil
}

func runWhoami() error {
	store := auth.NewStore()
	ctx := context.Background()
	providers := []string{"anthropic", "openai"}
	if len(os.Args) >= 3 {
		providers = []string{os.Args[2]}
	}
	for _, p := range providers {
		printResolvedCredential(auth.Resolve(ctx, store, p))
	}
	return nil
}

// runProviders lists every built-in OAuth provider plus its currently
// resolvable credential and any configured base_url override. It backs both
// the top-level `chronos-code providers` command and `chronos-code auth
// providers` so the two stay identical by construction.
func runProviders() error {
	store := auth.NewStore()
	ctx := context.Background()

	var overrides map[string]config.ProviderOverride
	if cfg, err := config.Load(configPath); err == nil {
		overrides = cfg.Providers
	}

	fmt.Println("built-in OAuth providers:")
	for _, name := range auth.ListProviders() {
		fmt.Printf("  %s\n", name)
	}
	fmt.Println("\nresolvable credentials:")
	for _, p := range []string{"anthropic", "openai"} {
		printResolvedCredential(auth.Resolve(ctx, store, p))
		if override, ok := overrides[p]; ok && override.BaseURL != "" {
			fmt.Printf("%-15s base_url: %s\n", "", override.BaseURL)
		}
	}
	return nil
}

func runAuth() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code auth [login|logout|status|refresh|whoami|providers]")
	}
	store := auth.NewStore()
	ctx := context.Background()

	switch os.Args[2] {
	case "login":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code auth login <provider> [--api-key <key> | --oauth-pkce ... | --device-code ...]")
		}
		provider := auth.CanonicalProvider(os.Args[3])
		rest := os.Args[4:]
		switch {
		case hasFlag(rest, "api-key"):
			key := flagValue(rest, "api-key", "")
			if key == "" {
				return fmt.Errorf("--api-key requires a value")
			}
			if err := auth.LoginAPIKey(store, provider, key); err != nil {
				return err
			}
			fmt.Printf("stored API key for %q\n", provider)
			return nil
		case hasFlag(rest, "oauth-pkce"):
			cfg := oauthConfigFromFlags(provider, rest)
			if cfg.ClientID == "" || cfg.AuthURL == "" || cfg.TokenURL == "" {
				return fmt.Errorf("--oauth-pkce requires --client-id, --auth-url, and --token-url")
			}
			if err := auth.LoginPKCE(ctx, store, cfg, func(url string) { fmt.Printf("open this URL to sign in:\n  %s\n", url) }); err != nil {
				return err
			}
			fmt.Printf("logged in to %q via OAuth PKCE\n", provider)
			return nil
		case hasFlag(rest, "device-code"):
			cfg := oauthConfigFromFlags(provider, rest)
			if cfg.ClientID == "" || cfg.AuthURL == "" || cfg.TokenURL == "" {
				return fmt.Errorf("--device-code requires --client-id, --auth-url, and --token-url")
			}
			if err := auth.LoginDeviceCode(ctx, store, cfg, func(userCode, verificationURI string) {
				fmt.Printf("visit %s and enter code: %s\n", verificationURI, userCode)
			}); err != nil {
				return err
			}
			fmt.Printf("logged in to %q via device code\n", provider)
			return nil
		default:
			if cfg, ok := auth.LookupProvider(provider); ok {
				fmt.Printf("signing in to %s via OAuth PKCE...\n", provider)
				if err := auth.LoginPKCE(ctx, store, cfg, func(url string) { fmt.Printf("open this URL to sign in:\n  %s\n", url) }); err != nil {
					return err
				}
				fmt.Printf("logged in to %q via OAuth PKCE\n", provider)
				return nil
			}
			return fmt.Errorf("auth login: specify --api-key, --oauth-pkce, or --device-code (or use: chronos-code login %s for built-in providers)", provider)
		}

	case "logout":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code auth logout <provider>")
		}
		provider := auth.CanonicalProvider(os.Args[3])
		if err := auth.Logout(store, provider); err != nil {
			return err
		}
		fmt.Printf("logged out of %q\n", provider)
		return nil

	case "status":
		if len(os.Args) >= 4 {
			st, err := auth.GetStatus(store, os.Args[3])
			if err != nil {
				return err
			}
			printAuthStatus(st)
			return nil
		}
		statuses, err := auth.ListStatus(store)
		if err != nil {
			return err
		}
		if len(statuses) == 0 {
			fmt.Println("not authenticated with any provider")
			return nil
		}
		for _, st := range statuses {
			printAuthStatus(st)
		}
		return nil

	case "refresh":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code auth refresh <provider> --client-id <id> --token-url <url>")
		}
		provider := auth.CanonicalProvider(os.Args[3])
		cfg := oauthConfigFromFlags(provider, os.Args[4:])
		if err := auth.Refresh(ctx, store, cfg); err != nil {
			return err
		}
		fmt.Printf("refreshed credential for %q\n", provider)
		return nil

	case "whoami":
		providers := []string{"anthropic", "openai"}
		if len(os.Args) >= 4 {
			providers = []string{os.Args[3]}
		}
		for _, p := range providers {
			printResolvedCredential(auth.Resolve(ctx, store, p))
		}
		return nil

	case "providers":
		return runProviders()

	default:
		return fmt.Errorf("unknown auth command: %s", os.Args[2])
	}
}

// printResolvedCredential reports which link of the provider's precedence
// chain (ROADMAP.md §5.3) is currently effective, without ever printing the
// token itself.
func printResolvedCredential(rc auth.ResolvedCredential) {
	if rc.Source == auth.SourceNone {
		fmt.Printf("%-15s no credential resolved (env vars, chronos-code login, and external CLI reuse all empty)\n", rc.Provider)
		return
	}
	expiry := "never"
	switch {
	case rc.ExpiresAt.IsZero():
		// leave as "never"
	case time.Until(rc.ExpiresAt) <= 0:
		expiry = "expired"
	default:
		expiry = time.Until(rc.ExpiresAt).String()
	}
	fmt.Printf("%-15s source: %-22s method: %-12s expires: %s\n", rc.Provider, rc.Source, rc.Method, expiry)
}

func printAuthStatus(st auth.Status) {
	if !st.Authenticated {
		fmt.Printf("%-15s not authenticated\n", st.Provider)
		return
	}
	fmt.Printf("%-15s %s, expires: %s\n", st.Provider, st.Method, st.ExpiresIn)
}

func runSession() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code session [list|delete|export]")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	store, dsn, err := orchestrator.OpenStorageForCLI(cfg)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer store.Close()
	mgr := session.NewManager(store, dsn)
	ctx := context.Background()

	switch os.Args[2] {
	case "list":
		agentID := "coder"
		if len(os.Args) >= 4 {
			agentID = os.Args[3]
		}
		sessions, err := mgr.List(ctx, agentID, 50, 0)
		if err != nil {
			return err
		}
		if len(sessions) == 0 {
			fmt.Printf("no sessions for agent %q\n", agentID)
			return nil
		}
		for _, s := range sessions {
			fmt.Printf("%s  %-10s updated %s\n", s.ID, s.Status, s.UpdatedAt.Format("2006-01-02 15:04"))
		}
		return nil
	case "delete":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code session delete <id>")
		}
		if err := mgr.Delete(ctx, os.Args[3]); err != nil {
			return err
		}
		fmt.Printf("deleted session %q\n", os.Args[3])
		return nil
	case "export":
		if len(os.Args) < 5 {
			return fmt.Errorf("usage: chronos-code session export <id> <path>")
		}
		if err := mgr.Export(ctx, os.Args[3], os.Args[4]); err != nil {
			return err
		}
		fmt.Printf("exported session %q to %s\n", os.Args[3], os.Args[4])
		return nil
	default:
		return fmt.Errorf("unknown session command: %s", os.Args[2])
	}
}

func runMemory() error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return runMemoryCommand(context.Background(), cfg, os.Args[2:], os.Stdout)
}

func runMemoryCommand(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: chronos-code memory [list|search|forget|layers]")
	}
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		return fmt.Errorf("resolve memory paths: %w", err)
	}
	if args[0] == "layers" {
		return runMemoryLayers(ctx, cfg, paths, args[1:], stdout)
	}
	store := memory.NewStore(filepath.Join(paths.LegacyDir, "memory")).ForContext(ctx)

	switch args[0] {
	case "list":
		category := memory.Category("")
		if len(args) >= 2 {
			category = memory.Category(args[1])
		}
		records, err := store.List(category)
		if err != nil {
			return err
		}
		printMemoryRecords(stdout, records)
		return nil
	case "search":
		if len(args) < 2 {
			return fmt.Errorf("usage: chronos-code memory search <query>")
		}
		query := strings.Join(args[1:], " ")
		records, err := store.Search(query)
		if err != nil {
			return err
		}
		printMemoryRecords(stdout, records)
		return nil
	case "forget":
		if len(args) < 2 {
			return fmt.Errorf("usage: chronos-code memory forget <id>")
		}
		if err := store.Forget(args[1]); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "forgot %q\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown memory command: %s", args[0])
	}
}

const memoryLayersUsage = `usage:
  chronos-code memory layers list --scope <scope> [--kind <kind>]
  chronos-code memory layers search <query> --scope <scope> [--kind <kind>]
  chronos-code memory layers forget <id> --scope <scope>

Scopes: project, user, tenant, organization (requires memory.organization_id).
Kinds: episodic, procedural, organizational, semantic (requires memory.semantic_enabled: true).
Omitting --kind lists/searches all enabled kinds. Forget invalidates one ID in the exact scope.
List/search return up to 100 active records and 64 KiB of record data, newest/relevance first.
Identity comes from the canonical project, OS uid, configured organization, and context tenant
(default tenant for standalone CLI). Identity overrides are not accepted.
`

func runMemoryLayers(ctx context.Context, cfg *config.Config, paths config.ProjectPaths, args []string, stdout io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "help" || args[0] == "-h") {
		_, err := io.WriteString(stdout, memoryLayersUsage)
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("%s", memoryLayersUsage)
	}
	scope, rest, err := parseMCPValueFlag(args[1:], "scope", "")
	if err != nil {
		return err
	}
	if scope == "" {
		return fmt.Errorf("--scope is required\n%s", memoryLayersUsage)
	}
	kind, rest, err := parseMCPValueFlag(rest, "kind", "")
	if err != nil {
		return err
	}
	for _, arg := range rest {
		if strings.HasPrefix(arg, "-") {
			return fmt.Errorf("unknown memory layers flag %q\n%s", arg, memoryLayersUsage)
		}
	}
	query := memory.LayerQuery{Scope: memory.Scope(scope), Kind: memory.Kind(kind)}
	switch args[0] {
	case "list":
		if len(rest) != 0 {
			return fmt.Errorf("%s", memoryLayersUsage)
		}
	case "search":
		query.Query = strings.Join(rest, " ")
		if strings.TrimSpace(query.Query) == "" {
			return fmt.Errorf("search requires a query\n%s", memoryLayersUsage)
		}
	case "forget":
		if len(rest) != 1 || kind != "" {
			return fmt.Errorf("%s", memoryLayersUsage)
		}
	default:
		return fmt.Errorf("unknown memory layers command %q\n%s", args[0], memoryLayersUsage)
	}
	// Shared DB and partition identities mirror runtime setup. Administration
	// uses the store's maximum retrieval budget rather than the prompt budget.
	options := memory.LayerOptions{
		ProjectID: paths.ID, UserID: fmt.Sprintf("uid:%d", os.Getuid()),
		OrganizationID: cfg.Memory.OrganizationID, SemanticEnabled: cfg.Memory.SemanticMemoryEnabled(),
		Budget: memory.RetrievalBudget{MaxRecords: memory.MaxLayerRecords, MaxBytes: memory.MaxLayerRecallBytes},
	}
	home := filepath.Dir(filepath.Dir(paths.Dir))
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create shared memory directory: %w", err)
	}
	store, err := memory.OpenLayerStore(ctx, filepath.Join(home, "memory.db"))
	if err != nil {
		return fmt.Errorf("open layered memory: %w", err)
	}
	defer store.Close()
	if args[0] == "forget" {
		if err := store.Forget(ctx, options, query.Scope, rest[0]); err != nil {
			return err
		}
		_, err := fmt.Fprintf(stdout, "forgot %q in %s scope\n", rest[0], scope)
		return err
	}
	records, err := store.Recall(ctx, options, query)
	if err != nil {
		return err
	}
	// JSON retains procedural steps and provenance for administration.
	return json.NewEncoder(stdout).Encode(records)
}

type mcpTestClient interface {
	Connect(context.Context) error
	ListTools(context.Context) ([]mcp.ToolInfo, error)
	Close() error
}

func runMCP() error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve project directory: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("resolve user home: %w", err)
	}
	return runMCPCommand(context.Background(), os.Args[2:], root, home, os.Stdout, func(cfg mcp.ServerConfig) (mcpTestClient, error) {
		return mcp.NewClient(cfg)
	})
}

func runMCPCommand(ctx context.Context, args []string, root, home string, stdout io.Writer, newClient func(mcp.ServerConfig) (mcpTestClient, error)) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: chronos-code mcp [add|list|remove|test]")
	}
	scope, rest, err := parseMCPValueFlag(args[1:], "scope", mcpdiscover.ScopeProject)
	if err != nil {
		return err
	}
	path, err := mcpdiscover.CanonicalPath(root, home, scope)
	if err != nil {
		return err
	}
	userScope := scope == mcpdiscover.ScopeUser

	switch args[0] {
	case "add":
		server, err := parseMCPAdd(rest)
		if err != nil {
			return err
		}
		if err := mcpdiscover.AddManaged(path, server, userScope); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "added MCP server %q to %s scope\n", server.Name, scope)
		return nil
	case "list":
		if len(rest) != 0 {
			return fmt.Errorf("usage: chronos-code mcp list [--scope project|user]")
		}
		servers, err := mcpdiscover.ListManaged(path)
		if err != nil {
			return err
		}
		if len(servers) == 0 {
			fmt.Fprintf(stdout, "no MCP servers in %s scope\n", scope)
			return nil
		}
		for _, server := range servers {
			fmt.Fprintf(stdout, "%s\t%s\t%s\tpermission=%s\tstatus=configured\n", server.Name, server.Transport, mcpdiscover.RedactedEndpoint(server), server.Permission)
		}
		return nil
	case "remove":
		if len(rest) != 1 {
			return fmt.Errorf("usage: chronos-code mcp remove <name> [--scope project|user]")
		}
		if err := mcpdiscover.RemoveManaged(path, rest[0], userScope); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "removed MCP server %q from %s scope\n", rest[0], scope)
		return nil
	case "test":
		timeoutText, remaining, err := parseMCPValueFlag(rest, "timeout", "10s")
		if err != nil {
			return err
		}
		if len(remaining) != 1 {
			return fmt.Errorf("usage: chronos-code mcp test <name> [--timeout 10s] [--scope project|user]")
		}
		timeout, err := time.ParseDuration(timeoutText)
		if err != nil || timeout <= 0 {
			return fmt.Errorf("invalid MCP test timeout %q: must be a positive duration", timeoutText)
		}
		servers, err := mcpdiscover.ListManaged(path)
		if err != nil {
			return err
		}
		for _, server := range servers {
			if server.Name == remaining[0] {
				return testMCPServer(ctx, server, timeout, stdout, newClient)
			}
		}
		return fmt.Errorf("MCP server %q does not exist", remaining[0])
	default:
		return fmt.Errorf("unknown mcp command: %s", args[0])
	}
}

func parseMCPAdd(args []string) (mcpdiscover.ManagedServer, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "--") {
		return mcpdiscover.ManagedServer{}, fmt.Errorf("usage: chronos-code mcp add <name> (--command <cmd> [--arg <arg> ...] | --url <https-url>) [--transport stdio|sse|streamable-http] [--scope project|user]")
	}
	server := mcpdiscover.ManagedServer{Name: args[0], Permission: "require_approval"}
	for i := 1; i < len(args); i++ {
		name, value, consumed, err := mcpFlag(args, i)
		if err != nil {
			return mcpdiscover.ManagedServer{}, err
		}
		i += consumed
		switch name {
		case "command":
			if server.Command != "" {
				return mcpdiscover.ManagedServer{}, fmt.Errorf("--command may be specified only once")
			}
			server.Command = value
		case "arg":
			server.Args = append(server.Args, value)
		case "url":
			if server.URL != "" {
				return mcpdiscover.ManagedServer{}, fmt.Errorf("--url may be specified only once")
			}
			server.URL = value
		case "transport":
			if server.Transport != "" {
				return mcpdiscover.ManagedServer{}, fmt.Errorf("--transport may be specified only once")
			}
			server.Transport = mcp.Transport(value)
		default:
			return mcpdiscover.ManagedServer{}, fmt.Errorf("unknown mcp add flag --%s", name)
		}
	}
	if server.Transport == "" {
		if server.URL != "" {
			server.Transport = mcp.TransportSSE
		} else {
			server.Transport = mcp.TransportStdio
		}
	}
	if err := mcpdiscover.ValidateManagedServer(server); err != nil {
		return mcpdiscover.ManagedServer{}, err
	}
	return server, nil
}

func parseMCPValueFlag(args []string, target, defaultValue string) (string, []string, error) {
	value := defaultValue
	found := false
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg != "--"+target && !strings.HasPrefix(arg, "--"+target+"=") {
			rest = append(rest, arg)
			continue
		}
		if found {
			return "", nil, fmt.Errorf("--%s may be specified only once", target)
		}
		found = true
		if arg == "--"+target {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return "", nil, fmt.Errorf("--%s requires a value", target)
			}
			i++
			value = args[i]
		} else {
			value = strings.TrimPrefix(arg, "--"+target+"=")
			if value == "" {
				return "", nil, fmt.Errorf("--%s requires a value", target)
			}
		}
	}
	return value, rest, nil
}

func mcpFlag(args []string, index int) (string, string, int, error) {
	arg := args[index]
	if !strings.HasPrefix(arg, "--") {
		return "", "", 0, fmt.Errorf("unexpected mcp add argument %q", arg)
	}
	flag := strings.TrimPrefix(arg, "--")
	if name, value, ok := strings.Cut(flag, "="); ok {
		if value == "" {
			return "", "", 0, fmt.Errorf("--%s requires a value", name)
		}
		return name, value, 0, nil
	}
	if index+1 >= len(args) || strings.HasPrefix(args[index+1], "--") {
		return "", "", 0, fmt.Errorf("--%s requires a value", flag)
	}
	return flag, args[index+1], 1, nil
}

func testMCPServer(ctx context.Context, server mcpdiscover.ManagedServer, timeout time.Duration, stdout io.Writer, newClient func(mcp.ServerConfig) (mcpTestClient, error)) error {
	client, err := newClient(mcp.ServerConfig{
		Name:       server.Name,
		Transport:  server.Transport,
		Command:    server.Command,
		Args:       server.Args,
		URL:        server.URL,
		Permission: server.Permission,
	})
	if err != nil {
		return fmt.Errorf("test MCP server %q: create client: %w", server.Name, err)
	}
	defer client.Close()

	testCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := client.Connect(testCtx); err != nil {
		return fmt.Errorf("test MCP server %q: initialize: %w", server.Name, err)
	}
	tools, err := client.ListTools(testCtx)
	if err != nil {
		return fmt.Errorf("test MCP server %q: list tools: %w", server.Name, err)
	}
	fmt.Fprintf(stdout, "%s\t%s\tpermission=%s\tstatus=ok\ttools=%d\n", server.Name, server.Transport, server.Permission, len(tools))
	return nil
}

func runTeam() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code team [list|run]")
	}
	switch os.Args[2] {
	case "list":
		orch, err := loadAndBuild()
		if err != nil {
			return err
		}
		defer orch.Close()
		teams := orch.ListTeams()
		if len(teams) == 0 {
			fmt.Println("no teams configured")
			return nil
		}
		for _, id := range teams {
			t, _ := orch.GetTeam(id)
			fmt.Printf("%-20s %s (%s)\n", id, t.Name, t.Strategy)
		}
		return nil
	case "run":
		if len(os.Args) < 5 {
			return fmt.Errorf("usage: chronos-code team run <team_id> <message>")
		}
		teamID := os.Args[3]
		message := strings.Join(os.Args[4:], " ")
		orch, err := loadAndBuild()
		if err != nil {
			return err
		}
		defer orch.Close()
		result, err := orch.RunTeam(context.Background(), teamID, message)
		if err != nil {
			return err
		}
		fmt.Println(result)
		return nil
	default:
		return fmt.Errorf("unknown team command: %s", os.Args[2])
	}
}

func printMemoryRecords(stdout io.Writer, records []memory.Record) {
	if len(records) == 0 {
		fmt.Fprintln(stdout, "no memory records")
		return
	}
	for _, r := range records {
		fmt.Fprintf(stdout, "%s  [%-8s] %s\n", r.ID, r.Category, r.Content)
	}
}

func resolveStorage(cfg *config.Config) string {
	if cfg.Defaults != nil && cfg.Defaults.Storage.Backend != "" {
		return cfg.Defaults.Storage.Backend + " (" + cfg.Defaults.Storage.DSN + ")"
	}
	return "sqlite (default)"
}

func runServe() error {
	args := os.Args[2:]

	orch, appCfg, err := loadConfigAndBuild()
	if err != nil {
		return err
	}
	defer orch.Close()

	serverDefaults := appCfg.Server
	cfg := server.ServerConfig{
		Listen:          flagValue(args, "listen", firstNonEmpty(serverDefaults.Listen, ":8430")),
		AuthType:        flagValue(args, "auth", firstNonEmpty(serverDefaults.AuthType, "api_key")),
		APIKey:          flagValue(args, "api-key", firstNonEmpty(os.Getenv("CHRONOS_CODE_API_KEY"), serverDefaults.APIKey)),
		TenantID:        flagValue(args, "tenant-id", firstNonEmpty(os.Getenv("CHRONOS_CODE_TENANT_ID"), serverDefaults.TenantID)),
		RepositoryID:    flagValue(args, "repository-id", firstNonEmpty(os.Getenv("CHRONOS_CODE_REPOSITORY_ID"), serverDefaults.RepositoryID, appCfg.Workspace.Root)),
		OIDCIssuer:      flagValue(args, "oidc-issuer", firstNonEmpty(os.Getenv("CHRONOS_CODE_OIDC_ISSUER"), serverDefaults.OIDCIssuer)),
		OIDCClientID:    flagValue(args, "oidc-client-id", firstNonEmpty(os.Getenv("CHRONOS_CODE_OIDC_CLIENT_ID"), serverDefaults.OIDCClientID)),
		CORSOrigins:     flagValue(args, "cors-origins", firstNonEmpty(serverDefaults.CORSOrigins, "*")),
		MaxConcurrent:   serverDefaults.MaxConcurrent,
		RateLimitPerMin: serverDefaults.RateLimitPerMin,
		RequestTimeout:  time.Duration(serverDefaults.RequestTimeoutSec) * time.Second,
		InstanceID:      flagValue(args, "instance-id", firstNonEmpty(os.Getenv("CHRONOS_CODE_INSTANCE_ID"), serverDefaults.InstanceID)),
		FleetInstances:  splitNonEmpty(firstNonEmpty(os.Getenv("CHRONOS_CODE_FLEET_INSTANCES"), strings.Join(serverDefaults.FleetInstances, ","))),
	}
	if paths, pathErr := appCfg.ResolveProjectPaths(""); pathErr == nil {
		cfg.DiskPaths = []string{paths.Dir}
	}
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 1
	}
	if cfg.RateLimitPerMin <= 0 {
		cfg.RateLimitPerMin = 60
	}
	if maxStr := flagValue(args, "max-concurrent", ""); maxStr != "" {
		maxConcurrent, parseErr := strconv.Atoi(maxStr)
		if parseErr != nil || maxConcurrent <= 0 {
			return fmt.Errorf("serve: --max-concurrent must be a positive integer")
		}
		cfg.MaxConcurrent = maxConcurrent
	}
	if rateStr := flagValue(args, "rate-limit", ""); rateStr != "" {
		rate, parseErr := strconv.Atoi(rateStr)
		if parseErr != nil || rate < 0 {
			return fmt.Errorf("serve: --rate-limit must be a non-negative integer")
		}
		cfg.RateLimitPerMin = rate
	}
	if timeoutStr := flagValue(args, "request-timeout", ""); timeoutStr != "" {
		timeout, parseErr := time.ParseDuration(timeoutStr)
		if parseErr != nil || timeout <= 0 {
			return fmt.Errorf("serve: --request-timeout must be a positive duration")
		}
		cfg.RequestTimeout = timeout
	}
	if cfg.AuthType == "api_key" && cfg.APIKey == "" {
		return fmt.Errorf("serve: --api-key or CHRONOS_CODE_API_KEY required when --auth=api_key; use --auth=none to disable")
	}
	if cfg.AuthType == "api_key" && cfg.TenantID == "" {
		return fmt.Errorf("serve: --tenant-id or CHRONOS_CODE_TENANT_ID required when --auth=api_key")
	}
	var deliveryPaths config.ProjectPaths
	if cfg.AuthType == "api_key" || cfg.AuthType == "oidc" {
		paths, err := appCfg.ResolveProjectPaths("")
		if err != nil {
			return fmt.Errorf("serve: resolve delivery paths: %w", err)
		}
		if err := os.MkdirAll(paths.Dir, 0o700); err != nil {
			return fmt.Errorf("serve: create delivery data directory: %w", err)
		}
		cfg.DeliveryStore, err = execution.OpenDeliveryStore(context.Background(), paths.DeliveriesDB)
		if err != nil {
			return fmt.Errorf("serve: open delivery store: %w", err)
		}
		defer cfg.DeliveryStore.Close()
		deliveryPaths = paths
	}
	readOnlyWorker, planWorker := false, false
	for _, arg := range args {
		if arg == "--delivery-read-only-worker" {
			readOnlyWorker = true
		} else if strings.HasPrefix(arg, "--delivery-read-only-worker=") {
			return fmt.Errorf("serve: --delivery-read-only-worker takes no value")
		} else if arg == "--delivery-plan-worker" {
			planWorker = true
		} else if strings.HasPrefix(arg, "--delivery-plan-worker=") {
			return fmt.Errorf("serve: --delivery-plan-worker takes no value")
		}
	}
	if readOnlyWorker || planWorker {
		if cfg.DeliveryStore == nil {
			return fmt.Errorf("serve: delivery read-only worker requires authenticated delivery storage")
		}
		authorizer := authorization.RepositoryAuthorizer{RepositoryID: cfg.RepositoryID, AllowedActions: map[string]struct{}{"delivery.execute": {}}}
		sandboxPolicy := security.SandboxPolicy{
			Image: appCfg.Server.DeliverySandbox.Image, SocketPath: appCfg.Server.DeliverySandbox.SocketPath,
			AllowNetwork: appCfg.Server.DeliverySandbox.AllowNetwork,
		}
		if sandboxPolicy.Image != "" {
			sandbox, err := security.NewContainerShellSandbox(context.Background(), deliveryPaths.Root, sandboxPolicy)
			if err != nil {
				return fmt.Errorf("serve: delivery sandbox preflight: %w", err)
			}
			_ = sandbox.Close()
		}
		readOnlyExecutor, err := orchestrator.NewReadOnlyDeliveryExecutor(orch, authorizer, sandboxPolicy)
		if err != nil {
			return fmt.Errorf("serve: create delivery read-only executor: %w", err)
		}
		var planExecutor *orchestrator.PlanDeliveryExecutor
		if planWorker {
			planExecutor, err = orchestrator.NewCandidatePlanDeliveryExecutor(orch, authorizer, sandboxPolicy)
			if err != nil {
				return fmt.Errorf("serve: delivery plan worker requires plan storage, a worktree manager and an implementation agent: %w", err)
			}
		}
		executor, err := orchestrator.NewRoutedDeliveryExecutor(readOnlyExecutor, planExecutor)
		if err != nil {
			return fmt.Errorf("serve: create delivery executor: %w", err)
		}
		cfg.DeliveryWorker, err = execution.NewWorker(cfg.DeliveryStore, executor, execution.WorkerConfig{
			OwnerID: "serve:" + session.NewSessionID(), Concurrency: 1,
			LeaseDuration: time.Minute, HeartbeatEvery: 15 * time.Second, PollEvery: time.Second,
		})
		if err != nil {
			return fmt.Errorf("serve: create delivery read-only worker: %w", err)
		}
	}

	srv := server.New(orch, cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if appCfg.Retention.Enabled && appCfg.Retention.PeriodicIntervalMinutes > 0 {
		paths, pathErr := appCfg.ResolveProjectPaths("")
		if pathErr != nil {
			return fmt.Errorf("resolve cleanup paths: %w", pathErr)
		}
		manager := cleanupManager(appCfg, paths, orch.ActiveSessionIDs())
		// Automatic server cleanup is intentionally limited to process-local
		// registries. Persistent stores may be shared by multiple instances and
		// are pruned only by an explicit operator command.
		manager.Adapters = []retention.Adapter{srv.RetentionAdapter()}
		interval := time.Duration(appCfg.Retention.PeriodicIntervalMinutes) * time.Minute
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					result, cleanupErr := manager.Run(ctx, false)
					srv.ObserveCleanup(result, cleanupErr)
					if cleanupErr != nil && ctx.Err() == nil {
						fmt.Fprintf(os.Stderr, "warning: periodic cleanup: %v\n", cleanupErr)
					}
				}
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	// Block until the server returns (or is interrupted).
	select {
	case e := <-errCh:
		return e
	case <-ctx.Done():
		shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutCancel()
		return srv.Shutdown(shutCtx)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func splitNonEmpty(value string) []string {
	var result []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result = append(result, item)
		}
	}
	return result
}
