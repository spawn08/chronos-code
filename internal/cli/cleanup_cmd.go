package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/plan"
	"github.com/spawn08/chronos-code/internal/retention"
)

const cleanupUsage = "usage: chronos-code cleanup status [scope] | cleanup run [--dry-run] [--scope <scope>] | cleanup prune <scope> [--dry-run] [--tenant <id> --repository <id>]"

func runCleanup() error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	return runCleanupCommand(context.Background(), cfg, os.Args[2:], os.Stdout)
}

func runCleanupCommand(ctx context.Context, cfg *config.Config, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(cleanupUsage)
	}
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		return fmt.Errorf("resolve cleanup paths: %w", err)
	}
	if err := retention.Recover(paths.Dir, cleanupBatchSize(cfg)); err != nil {
		return fmt.Errorf("recover interrupted cleanup: %w", err)
	}
	manager := cleanupManager(cfg, paths, nil)
	switch args[0] {
	case "status":
		if len(args) > 2 {
			return errors.New(cleanupUsage)
		}
		var scopes []string
		if len(args) == 2 {
			scopes = []string{args[1]}
		}
		result, err := manager.Status(ctx, scopes...)
		return writeCleanupResult(stdout, result, err)
	case "run":
		dryRun, scope, _, _, err := parseCleanupFlags(args[1:])
		if err != nil {
			return err
		}
		var scopes []string
		if scope != "" {
			scopes = []string{scope}
		}
		result, err := manager.Run(ctx, dryRun, scopes...)
		return writeCleanupResult(stdout, result, err)
	case "prune":
		if len(args) < 2 || strings.HasPrefix(args[1], "--") {
			return errors.New(cleanupUsage)
		}
		scope := args[1]
		dryRun, flagScope, tenant, repository, err := parseCleanupFlags(args[2:])
		if err != nil {
			return err
		}
		if flagScope != "" {
			return fmt.Errorf("cleanup prune: --scope is not allowed")
		}
		if scope == retention.ScopePlanDB {
			return prunePlanDB(ctx, stdout, paths.PlansDB, tenant, repository, cleanupBatchSize(cfg), dryRun)
		}
		if tenant != "" || repository != "" {
			return fmt.Errorf("cleanup prune: tenant and repository apply only to plan_db")
		}
		result, err := manager.Prune(ctx, scope, dryRun)
		return writeCleanupResult(stdout, result, err)
	default:
		return errors.New(cleanupUsage)
	}
}

func parseCleanupFlags(args []string) (dryRun bool, scope, tenant, repository string, err error) {
	values := map[string]*string{"scope": &scope, "tenant": &tenant, "repository": &repository}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if !strings.HasPrefix(args[i], "--") {
			return false, "", "", "", fmt.Errorf("cleanup: unexpected argument %q", args[i])
		}
		if name == "dry-run" {
			if hasValue || seen[name] {
				return false, "", "", "", fmt.Errorf("cleanup: invalid --dry-run")
			}
			dryRun, seen[name] = true, true
			continue
		}
		destination, ok := values[name]
		if !ok || seen[name] {
			return false, "", "", "", fmt.Errorf("cleanup: invalid --%s", name)
		}
		if !hasValue {
			i++
			if i >= len(args) {
				return false, "", "", "", fmt.Errorf("cleanup: --%s requires a value", name)
			}
			value = args[i]
		}
		if value == "" {
			return false, "", "", "", fmt.Errorf("cleanup: --%s requires a value", name)
		}
		*destination, seen[name] = value, true
	}
	return
}

func cleanupManager(cfg *config.Config, paths config.ProjectPaths, active []string) *retention.Manager {
	policies := make(map[string]retention.Policy, len(cfg.Retention.Policies))
	for scope, policy := range cfg.Retention.Policies {
		policies[scope] = retention.Policy{MaxAge: time.Duration(policy.MaxAgeDays) * 24 * time.Hour, MaxCount: policy.MaxCount, MaxBytes: policy.MaxBytes}
	}
	return &retention.Manager{Adapters: retention.DefaultAdapters(paths, active), Policies: policies, BatchSize: cleanupBatchSize(cfg)}
}

func cleanupBatchSize(cfg *config.Config) int {
	if cfg.Retention.BatchSize > 0 {
		return cfg.Retention.BatchSize
	}
	return 100
}

func prunePlanDB(ctx context.Context, stdout io.Writer, path, tenant, repository string, limit int, dryRun bool) error {
	if tenant == "" || repository == "" {
		return fmt.Errorf("cleanup prune plan_db: --tenant and --repository are required")
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("open plan store: %w", err)
	}
	store, err := plan.OpenSQLStore(ctx, path)
	if err != nil {
		return fmt.Errorf("open plan store: %w", err)
	}
	defer store.Close()
	result, err := store.Prune(ctx, plan.PruneRequest{Scope: plan.PlanScope{TenantID: plan.TenantID(tenant), RepositoryID: plan.RepositoryID(repository)}, DryRun: dryRun, Limit: limit})
	return writeCleanupResult(stdout, result, err)
}

func writeCleanupResult(stdout io.Writer, value any, err error) error {
	if err != nil {
		return err
	}
	if err := json.NewEncoder(stdout).Encode(value); err != nil {
		return fmt.Errorf("write cleanup result: %w", err)
	}
	return nil
}
