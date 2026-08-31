package cli

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/tui"
	"github.com/spawn08/chronos/engine/model"
)

var (
	Version   = "dev"
	Commit    = "none"
	BuildDate = "unknown"
)

var (
	configPath     string
	debugMode      bool
	streamMode     = true
	permissionMode string
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
  chronos-code auth status        Show authentication state
  chronos-code version            Print version information
  chronos-code help               Show this help

Global flags:
  -c, --config <path>             Path to config file
  --debug                         Enable debug logging
  -s, --stream                    Enable streaming output
  --no-stream                     Disable streaming output
  --permission-mode <mode>        Tool permission mode (prompt, auto_approve, deny)
`)
	return nil
}

func loadAndBuild() (*orchestrator.Orchestrator, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	ctx := context.Background()
	orch, err := orchestrator.New(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("build orchestrator: %w", err)
	}
	return orch, nil
}

func runREPL() error {
	orch, err := loadAndBuild()
	if err != nil {
		return err
	}
	defer orch.Close()

	r := tui.NewREPL(orch, streamMode)
	return r.Start()
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

func runAuth() error {
	return fmt.Errorf("auth not yet implemented")
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
