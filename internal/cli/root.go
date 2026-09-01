package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/memory"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/server"
	"github.com/spawn08/chronos-code/internal/session"
	"github.com/spawn08/chronos-code/internal/tui"
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
	case "auth":
		return runAuth()
	case "session":
		return runSession()
	case "memory":
		return runMemory()
	case "learn":
		return runLearn()
	case "eval":
		return runEval()
	case "team":
		return runTeam()
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
	return nil
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
  chronos-code config show        Print resolved configuration
  chronos-code config validate    Validate all config files
  chronos-code auth login <provider> --api-key <key>   Store a BYO API key
  chronos-code auth login <provider> --oauth-pkce ...   Authorization Code + PKCE login
  chronos-code auth login <provider> --device-code ...  Device Authorization Grant login
  chronos-code auth logout <provider>                   Remove a stored credential
  chronos-code auth status [provider]                   Show authentication state
  chronos-code auth refresh <provider> ...              Refresh an OAuth credential
  chronos-code auth whoami [provider]                   Show the effective credential source (anthropic/openai default)
  chronos-code auth providers                            List providers with a resolvable credential right now
  chronos-code session list [agent]                     List sessions
  chronos-code session delete <id>                      Delete a session and its history
  chronos-code session export <id> <path>                Export a session to JSON
  chronos-code memory list [category]                   List remembered notes
  chronos-code memory search <query>                    Search remembered notes
  chronos-code memory forget <id>                        Remove a remembered note
  chronos-code learn suggest [agent]                     Distill traced sessions into a reviewable suggestion
  chronos-code learn list                                List pending suggestions
  chronos-code learn show <id>                           Show a suggestion's full YAML and rationale
  chronos-code learn accept <id>                         Apply a suggestion (agent or pattern)
  chronos-code learn reject <id>                          Discard a suggestion
  chronos-code eval run [--update-baseline] [--md <path>]  Run the token-efficiency eval suite
  chronos-code team list                                   List configured teams
  chronos-code team run <team_id> <message>                Run a team on a task
  chronos-code serve [--listen :8430] [--auth api_key]     Start HTTP server for team deployment
  chronos-code version            Print version information
  chronos-code help               Show this help

Global flags:
  -c, --config <path>             Path to config file
  --debug                         Enable debug logging
  -s, --stream                    Enable streaming output
  --no-stream                     Disable streaming output
  --permission-mode <mode>        Tool permission mode (prompt, auto_approve, deny)
  --resume <session-id>           Resume a specific session instead of the latest one
`)
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
	if err := orch.SetPermissionMode(permissionMode); err != nil {
		return nil, fmt.Errorf("apply --permission-mode: %w", err)
	}
	return orch, nil
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

	ctx := context.Background()
	if strings.HasPrefix(message, "@") {
		parts := strings.SplitN(message[1:], " ", 2)
		if len(parts) == 2 {
			if err := orch.SwitchAgent(parts[0]); err != nil {
				return err
			}
			message = parts[1]
		}
	} else if agentID, matched := orch.Route(ctx, message); matched {
		_ = orch.SwitchAgent(agentID)
	}

	if streamMode {
		ch, err := orch.ChatStream(ctx, message)
		if err != nil {
			return err
		}
		usage, err := tui.StreamResponse(ch, os.Stdout)
		if err != nil {
			return err
		}
		tui.PrintUsage(usage, os.Stderr)
		return nil
	}

	resp, err := orch.Chat(ctx, message)
	if err != nil {
		return err
	}
	tui.PrintResponse(resp, os.Stdout)
	return nil
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
			return fmt.Errorf("auth login: specify --api-key, --oauth-pkce, or --device-code")
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
		for _, p := range []string{"anthropic", "openai"} {
			printResolvedCredential(auth.Resolve(ctx, store, p))
		}
		return nil

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
