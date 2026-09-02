package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spawn08/chronos/sdk/agent"

	"github.com/spawn08/chronos-code/internal/auth"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/learning"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/session"
)

// defaultLearningOutputDir mirrors LearningConfig.OutputDir's embedded
// default (internal/defaults/config.yaml), used only if a project overrides
// config.yaml without setting output_dir.
const defaultLearningOutputDir = ".chronos-code/learned"

// defaultMinSessionsBeforeDistill mirrors LearningConfig's embedded default,
// used only as a defensive fallback if it's left unset (zero value) by some
// config override.
const defaultMinSessionsBeforeDistill = 3

func runLearn() error {
	if len(os.Args) < 3 {
		return fmt.Errorf("usage: chronos-code learn [suggest|list|show|accept|reject|candidates|promote]")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	outputDir := cfg.Learning.OutputDir
	if outputDir == "" {
		outputDir = defaultLearningOutputDir
	}
	store := learning.NewStore(outputDir)

	switch os.Args[2] {
	case "suggest":
		agentID := "coder"
		if len(os.Args) >= 4 {
			agentID = os.Args[3]
		}
		return learnSuggest(cfg, store, agentID)
	case "list":
		suggestions, err := store.List()
		if err != nil {
			return err
		}
		printSuggestions(suggestions)
		return nil
	case "show":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code learn show <id>")
		}
		sug, err := store.Get(os.Args[3])
		if err != nil {
			return err
		}
		printSuggestionDetail(sug)
		return nil
	case "accept":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code learn accept <id>")
		}
		if err := store.Accept(os.Args[3]); err != nil {
			return err
		}
		fmt.Printf("accepted %q — takes effect the next time chronos-code starts\n", os.Args[3])
		return nil
	case "reject":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code learn reject <id>")
		}
		if err := store.Reject(os.Args[3]); err != nil {
			return err
		}
		fmt.Printf("rejected %q\n", os.Args[3])
		return nil
	case "candidates":
		return learnCandidates(cfg)
	case "promote":
		if len(os.Args) < 4 {
			return fmt.Errorf("usage: chronos-code learn promote <trigger-hash>")
		}
		return learnPromote(cfg, os.Args[3])
	default:
		return fmt.Errorf("unknown learn command: %s", os.Args[2])
	}
}

func learnCandidates(cfg *config.Config) error {
	ctx := context.Background()
	store, root, err := openWorkspaceLearningStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close(ctx)

	candidates, err := store.Candidates(ctx, root)
	if err != nil {
		return err
	}
	if len(candidates) == 0 {
		fmt.Println("no learning candidates")
		return nil
	}
	for _, candidate := range candidates {
		fmt.Printf("%s  success %d  failure %d  tools %s\n",
			candidate.TriggerHash, candidate.SuccessCount, candidate.FailCount, strings.Join(candidate.ToolSequence, ","))
	}
	return nil
}

func learnPromote(cfg *config.Config, triggerHash string) error {
	ctx := context.Background()
	store, root, err := openWorkspaceLearningStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer store.Close(ctx)

	candidates, err := store.Candidates(ctx, root)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if candidate.TriggerHash != triggerHash {
			continue
		}
		skillsDir, err := userSkillsDir(cfg)
		if err != nil {
			return err
		}
		path, err := learning.PromoteCandidate(skillsDir, "learned-"+candidate.TriggerHash, candidate)
		if err != nil {
			return fmt.Errorf("promote candidate %q: %w", triggerHash, err)
		}
		fmt.Printf("promoted candidate %q to %s\n", triggerHash, path)
		return nil
	}
	return fmt.Errorf("learning candidate %q not found", triggerHash)
}

func openWorkspaceLearningStore(ctx context.Context, cfg *config.Config) (*learning.SQLStore, string, error) {
	root := cfg.Workspace.Root
	if root == "" {
		root = config.WorkspaceRoot()
	}
	dir := filepath.Join(root, config.ConfigDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create learning directory %q: %w", dir, err)
	}
	store, err := learning.OpenSQLStore(ctx, filepath.Join(dir, "memory.db"))
	if err != nil {
		return nil, "", fmt.Errorf("open workspace learning store: %w", err)
	}
	return store, root, nil
}

func userSkillsDir(cfg *config.Config) (string, error) {
	if cfg.SkillsDir != "" {
		return cfg.SkillsDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user skills directory: %w", err)
	}
	return filepath.Join(home, config.ConfigDirName, "skills"), nil
}

// learnSuggest implements `chronos-code learn suggest [agent]` (PRD P3-002):
// it gathers every session's execution traces for agentID (captured via
// orchestrator's setupTracing, PRD P3-001), reduces them to an aggregated
// Report, and — once at least cfg.Learning.MinSessionsBeforeDistill sessions
// exist — asks the configured distillation model for a candidate Suggestion,
// which is saved for review rather than applied automatically.
func learnSuggest(cfg *config.Config, store *learning.Store, agentID string) error {
	if !cfg.Learning.Enabled {
		return fmt.Errorf("learning is disabled (learning.enabled: false in config)")
	}

	storeBackend, dsn, err := orchestrator.OpenStorageForCLI(cfg)
	if err != nil {
		return fmt.Errorf("open storage: %w", err)
	}
	defer storeBackend.Close()

	ctx := context.Background()
	sessionMgr := session.NewManager(storeBackend, dsn)
	sessions, err := sessionMgr.List(ctx, agentID, 1000, 0)
	if err != nil {
		return fmt.Errorf("list sessions for %q: %w", agentID, err)
	}

	minSessions := cfg.Learning.MinSessionsBeforeDistill
	if minSessions <= 0 {
		minSessions = defaultMinSessionsBeforeDistill
	}
	if len(sessions) < minSessions {
		fmt.Printf("%d session(s) recorded for %q, need %d before distillation runs\n", len(sessions), agentID, minSessions)
		return nil
	}

	sessionIDs := make([]string, len(sessions))
	for i, s := range sessions {
		sessionIDs[i] = s.ID
	}

	report, err := learning.Analyze(ctx, storeBackend, agentID, sessionIDs)
	if err != nil {
		return fmt.Errorf("analyze traces: %w", err)
	}
	if report.Empty() {
		fmt.Println("no execution traces recorded for these sessions yet — nothing to distill")
		return nil
	}

	distCfg, err := loadLearningConfig()
	if err != nil {
		return fmt.Errorf("load learning.yaml: %w", err)
	}
	mc := distCfg.ModelConfig()
	resolveStoredAPIKey(ctx, &mc)
	provider, err := agent.BuildProvider(mc)
	if err != nil {
		return fmt.Errorf("build distillation model provider: %w", err)
	}

	distiller := learning.NewDistiller(provider, distCfg)
	sug, err := distiller.Suggest(ctx, report)
	if err != nil {
		return fmt.Errorf("distill: %w", err)
	}
	if sug == nil {
		fmt.Println("distillation model found no actionable pattern in these sessions")
		return nil
	}

	if err := store.Save(sug); err != nil {
		return fmt.Errorf("save suggestion: %w", err)
	}
	fmt.Printf("new suggestion %q: [%s] %s (confidence %.2f)\n", sug.ID, sug.Kind, sug.Title, sug.Confidence)
	fmt.Printf("review with: chronos-code learn show %s\n", sug.ID)
	return nil
}

// loadLearningConfig reads learning.yaml, preferring a project override at
// <projectDir>/learning.yaml over the embedded default — the same
// override-then-fallback precedence orchestrator.readOverridableFile uses
// for routing.yaml/guardrails/security.yaml.
func loadLearningConfig() (*learning.Config, error) {
	projectDir, _, err := config.Discover()
	if err != nil {
		return nil, err
	}
	if projectDir != "" {
		if data, err := os.ReadFile(filepath.Join(projectDir, "learning.yaml")); err == nil {
			return learning.Parse(data)
		}
	}
	data, err := defaults.ReadFile("learning.yaml")
	if err != nil {
		return nil, err
	}
	return learning.Parse(data)
}

// resolveStoredAPIKey fills mc.APIKey from the provider's full
// authentication precedence chain (PRD P2-010; ROADMAP.md §5.3) when the
// caller didn't already set one — mirroring
// orchestrator.applyStoredCredentials, but scoped to the single ModelConfig
// the distillation model uses, since `learn suggest` doesn't build a full
// Orchestrator.
func resolveStoredAPIKey(ctx context.Context, mc *agent.ModelConfig) {
	if mc.APIKey != "" || mc.Provider == "" {
		return
	}
	if token := auth.Resolve(ctx, auth.NewStore(), mc.Provider).Token; token != "" {
		mc.APIKey = token
	}
}

func printSuggestions(suggestions []*learning.Suggestion) {
	if len(suggestions) == 0 {
		fmt.Println("no pending suggestions")
		return
	}
	for _, s := range suggestions {
		fmt.Printf("%s  [%-7s] %-40s confidence %.2f  (%s)\n", s.ID, s.Kind, s.Title, s.Confidence, s.CreatedAt.Format("2006-01-02 15:04"))
	}
}

func printSuggestionDetail(s *learning.Suggestion) {
	fmt.Printf("ID:         %s\n", s.ID)
	fmt.Printf("Kind:       %s\n", s.Kind)
	if s.AgentID != "" {
		fmt.Printf("Agent ID:   %s\n", s.AgentID)
	}
	fmt.Printf("Title:      %s\n", s.Title)
	fmt.Printf("Confidence: %.2f\n", s.Confidence)
	fmt.Printf("Source:     agent %q, %d session(s)\n", s.SourceAgentID, len(s.SourceSessions))
	fmt.Printf("Created:    %s\n", s.CreatedAt.Format("2006-01-02 15:04"))
	fmt.Printf("Rationale:  %s\n\n", s.Rationale)
	fmt.Println("--- YAML ---")
	fmt.Println(s.YAML)
}
