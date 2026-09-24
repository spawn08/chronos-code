package cli

import (
	"context"
	"fmt"
	"os"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
)

// Delivery database commands are offline operator controls. Autonomous task
// submission remains behind the worker and verification release gates.
func runDeliveryDB() error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load delivery configuration: %w", err)
	}
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		return fmt.Errorf("resolve delivery database: %w", err)
	}
	return runDeliveryDBCommand(context.Background(), paths, os.Args[2:])
}

func runDeliveryDBCommand(ctx context.Context, paths config.ProjectPaths, args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: chronos-code delivery backup <path> | delivery restore <source> <new-path>")
	}
	switch args[0] {
	case "backup":
		if len(args) != 2 {
			return fmt.Errorf("usage: chronos-code delivery backup <path>")
		}
		if _, err := os.Stat(paths.DeliveriesDB); err != nil {
			return fmt.Errorf("inspect delivery database: %w", err)
		}
		store, err := execution.OpenDeliveryStore(ctx, paths.DeliveriesDB)
		if err != nil {
			return fmt.Errorf("open delivery database: %w", err)
		}
		defer store.Close()
		if err := store.Backup(ctx, args[1]); err != nil {
			return err
		}
		fmt.Printf("delivery backup saved to %s\n", args[1])
		return nil
	case "restore":
		if len(args) != 3 {
			return fmt.Errorf("usage: chronos-code delivery restore <source> <new-path>")
		}
		if err := execution.RestoreDeliveryStore(ctx, args[1], args[2]); err != nil {
			return err
		}
		fmt.Printf("delivery backup restored to %s (configure an offline instance to use it)\n", args[2])
		return nil
	default:
		return fmt.Errorf("unknown delivery command %q", args[0])
	}
}
