package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/budget"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/mcpdiscover"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/server"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos-code/internal/tui"
	"github.com/spawn08/chronos/engine/mcp"
	"github.com/spawn08/chronos/engine/model"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var (
	configPath      string
	debugMode       bool
	streamMode      = true
	permissionMode  string
	yoloMode        bool
	usdBudgetCap    budget.Microdollars
	usdBudgetSet    bool
	resumeSessionID string
)

// systemPromptTokenBudget is the target ceiling for an agent's base system
// prompt (PRD P1-005): keeping it lean maximizes the stable, cacheable prefix
// of every request.
const systemPromptTokenBudget = 800

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
	case "learn":
		return runLearn()
	case "eval":
		return runEval()
	case "team":
		return runTeam()
	case "plan":
		return runPlan()
	case "skills":
		return runSkills()
	case "serve":
		return runServe()
	case "version":
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
		}
		cleaned = append(cleaned, arg)
		i++
	}
	os.Args = cleaned
	if yoloMode && permissionMode == "deny" {
		return fmt.Errorf("--yolo conflicts with --permission-mode deny")
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

func printVersion() error {
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
  chronos-code mcp add <name> --command <cmd> [--arg <arg> ...] [--scope project|user]
  chronos-code mcp add <name> --url <https-url> [--scope project|user]
  chronos-code mcp list [--scope project|user]           List canonical MCP servers with secrets redacted
  chronos-code mcp remove <name> [--scope project|user]  Remove a canonical MCP server
  chronos-code mcp test <name> [--timeout 10s] [--scope project|user]  Initialize, list tools, and close
  MCP transports: stdio and HTTPS SSE only; HTTP transport is not supported. Credential values must remain ${ENV_VAR} references.
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
  chronos-code skills list                                  List discovered skills (project + user + bundled)
  chronos-code skills show <name>                           Show a skill's metadata and body
  chronos-code serve [--listen :8430] [--auth api_key]     Start HTTP server for team deployment
  chronos-code version            Print version information
  chronos-code help               Show this help

Global flags:
  -c, --config <path>             Path to config file
  --debug                         Enable debug logging
  -s, --stream                    Enable streaming output
  --no-stream                     Disable streaming output
  --permission-mode <mode>        Tool permission mode (prompt, auto_approve, deny)
  --yolo                          Auto-approve policy-allowed tools; never overrides deny or destructive confirm
  --budget <usd>                  Per-session USD cap (up to 6 decimal places; omitted means unlimited)
  --resume <session-id>           Resume a specific prior session
`)
	return nil
}

func runAgents() error {
	if len(os.Args) < 3 || os.Args[2] != "list" || len(os.Args) > 3 {
		return fmt.Errorf("usage: chronos-code agents list")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	for _, configured := range cfg.Agents {
		fmt.Printf("%-16s %-24s %s/%s\n", configured.ID, configured.Name, configured.Model.Provider, configured.Model.Model)
	}
	return nil
}

func loadAndBuild() (*orchestrator.Orchestrator, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	ctx := context.Background()
	orch, err := orchestrator.New(ctx, cfg, resumeSessionID)
	if err != nil {
		return nil, fmt.Errorf("build orchestrator: %w", err)
	}
	if usdBudgetSet {
		orch.SetUSDCap(usdBudgetCap)
	}
	if err := orch.SetPermissionMode(effectivePermissionMode()); err != nil {
		return nil, fmt.Errorf("apply --permission-mode: %w", err)
	}
	return orch, nil
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

	return tui.RunTUI(orch, streamMode)
}

func runHeadless() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code run <message>")
	}
	message := strings.Join(os.Args[2:], " ")

	orch, err := loadAndBuild()
	if err != nil {
		return err
	}
	defer orch.Close()

	if strings.HasPrefix(message, "@") {
		parts := strings.SplitN(message[1:], " ", 2)
		if len(parts) == 2 {
			if err := orch.SwitchAgent(parts[0]); err != nil {
				return err
			}
			message = parts[1]
		}
	}

	request := orchestrator.ExecutionRequest{Message: message, VerificationMode: orch.VerificationMode()}
	if streamMode {
		request.Mode = orchestrator.ExecutionStreaming
	}
	_, err = RunExecution(context.Background(), orch, request, os.Stdout, os.Stderr)
	return err
}

// RunExecution executes and renders one headless CLI request. It is kept
// separate from argument/config handling so the adapter can be exercised with
// a deterministic orchestrator.
func RunExecution(ctx context.Context, orch *orchestrator.Orchestrator, request orchestrator.ExecutionRequest, stdout, stderr io.Writer) (orchestrator.ExecutionResult, error) {
	result, err := orch.Execute(ctx, request)
	if err != nil {
		return result, err
	}
	if request.Mode == orchestrator.ExecutionStreaming {
		usage, streamErr := tui.StreamResponse(result.Stream, stdout)
		if streamErr != nil {
			return result, streamErr
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
		cfg, err := config.Load(configPath)
		if err != nil {
			return err
		}
		fmt.Printf("Provider:  %s\n", resolveProvider(cfg))
		fmt.Printf("Model:     %s\n", resolveModel(cfg))
		fmt.Printf("Storage:   %s\n", resolveStorage(cfg))
		fmt.Printf("Agents:    %d\n", len(cfg.Agents))
		for _, a := range cfg.Agents {
			counter := model.NewTokenCounter(a.Model.Model)
			tokens := counter.CountString(a.System)
			flag := ""
			if tokens > systemPromptTokenBudget {
				flag = fmt.Sprintf(" [over %d-token budget]", systemPromptTokenBudget)
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
	provider := os.Args[2]
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
	store := auth.NewStore()
	if err := auth.Logout(store, os.Args[2]); err != nil {
		return err
	}
	fmt.Printf("logged out of %q\n", os.Args[2])
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
		provider := os.Args[3]
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
		if err := auth.Logout(store, os.Args[3]); err != nil {
			return err
		}
		fmt.Printf("logged out of %q\n", os.Args[3])
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
		provider := os.Args[3]
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
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code memory [list|search|forget]")
	}
	store := memory.NewStore(filepath.Join(config.ConfigDirName, "memory"))

	switch os.Args[2] {
	case "list":
		category := memory.Category("")
		if len(os.Args) >= 4 {
			category = memory.Category(os.Args[3])
		}
		records, err := store.List(category)
		if err != nil {
			return err
		}
		printMemoryRecords(records)
		return nil
	case "search":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code memory search <query>")
		}
		query := strings.Join(os.Args[3:], " ")
		records, err := store.Search(query)
		if err != nil {
			return err
		}
		printMemoryRecords(records)
		return nil
	case "forget":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code memory forget <id>")
		}
		if err := store.Forget(os.Args[3]); err != nil {
			return err
		}
		fmt.Printf("forgot %q\n", os.Args[3])
		return nil
	default:
		return fmt.Errorf("unknown memory command: %s", os.Args[2])
	}
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
		return mcpdiscover.ManagedServer{}, fmt.Errorf("usage: chronos-code mcp add <name> (--command <cmd> [--arg <arg> ...] | --url <https-url>) [--transport stdio|sse] [--scope project|user]")
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

func printMemoryRecords(records []memory.Record) {
	if len(records) == 0 {
		fmt.Println("no memory records")
		return
	}
	for _, r := range records {
		fmt.Printf("%s  [%-8s] %s\n", r.ID, r.Category, r.Content)
	}
}

func resolveProvider(cfg *config.Config) string {
	if cfg.Defaults != nil && cfg.Defaults.Model.Provider != "" {
		return cfg.Defaults.Model.Provider
	}
	if len(cfg.Agents) > 0 {
		return cfg.Agents[0].Model.Provider
	}
	return "unknown"
}

func resolveModel(cfg *config.Config) string {
	if cfg.Defaults != nil && cfg.Defaults.Model.Model != "" {
		return cfg.Defaults.Model.Model
	}
	if len(cfg.Agents) > 0 {
		return cfg.Agents[0].Model.Model
	}
	return "unknown"
}

func resolveStorage(cfg *config.Config) string {
	if cfg.Defaults != nil && cfg.Defaults.Storage.Backend != "" {
		return cfg.Defaults.Storage.Backend + " (" + cfg.Defaults.Storage.DSN + ")"
	}
	return "sqlite (default)"
}

func runServe() error {
	args := os.Args[2:]

	orch, err := loadAndBuild()
	if err != nil {
		return err
	}
	defer orch.Close()

	cfg := server.ServerConfig{
		Listen:          flagValue(args, "listen", ":8430"),
		AuthType:        flagValue(args, "auth", "api_key"),
		APIKey:          flagValue(args, "api-key", os.Getenv("CHRONOS_CODE_API_KEY")),
		CORSOrigins:     flagValue(args, "cors-origins", "*"),
		RateLimitPerMin: 60,
	}
	if rateStr := flagValue(args, "rate-limit", ""); rateStr != "" {
		var rate int
		fmt.Sscanf(rateStr, "%d", &rate)
		if rate > 0 {
			cfg.RateLimitPerMin = rate
		}
	}
	if cfg.AuthType == "api_key" && cfg.APIKey == "" {
		return fmt.Errorf("serve: --api-key or CHRONOS_CODE_API_KEY required when --auth=api_key; use --auth=none to disable")
	}

	srv := server.New(orch, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

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
