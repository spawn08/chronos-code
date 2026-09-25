package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/modelsdev"
)

func modelsCatalogOptions(cfg *config.Config) (modelsdev.Options, error) {
	_, userDir, err := config.Discover()
	if err != nil {
		return modelsdev.Options{}, fmt.Errorf("locate user config dir: %w", err)
	}
	return modelsdev.Options{
		CachePath:   modelsdev.CachePath(userDir),
		URL:         cfg.Models.SourceURL(),
		UserAgent:   "chronos-code/" + Version,
		AutoRefresh: cfg.Models.AutoRefreshEnabled(),
	}, nil
}

// initModelsCatalog applies the cached models.dev catalog and, when
// allowRefresh is set and config/env permit, refreshes a stale cache in the
// background. It never blocks on the network; background failures are
// silent because the TUI owns the terminal, and the next start retries.
func initModelsCatalog(cfg *config.Config, allowRefresh bool) {
	opts, err := modelsCatalogOptions(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: models.dev catalog: %v\n", err)
		return
	}
	opts.AutoRefresh = opts.AutoRefresh && allowRefresh
	if _, err := modelsdev.Init(context.Background(), opts); err != nil {
		fmt.Fprintf(os.Stderr, "warning: models.dev catalog cache ignored: %v\n", err)
	}
}

func runModelsRefresh() error {
	if len(os.Args) != 3 {
		return fmt.Errorf("usage: chronos-code models refresh")
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	opts, err := modelsCatalogOptions(cfg)
	if err != nil {
		return err
	}
	return runModelsRefreshCommand(context.Background(), opts, os.Stdout)
}

// runModelsRefreshCommand fetches the catalog on explicit request, ignoring
// auto_refresh and the fetch opt-out, which only govern automatic fetches.
func runModelsRefreshCommand(ctx context.Context, opts modelsdev.Options, stdout io.Writer) error {
	catalog, err := modelsdev.Refresh(ctx, opts)
	if err != nil {
		return fmt.Errorf("refresh models.dev catalog: %w", err)
	}
	stats := catalog.Stats()
	fmt.Fprintf(stdout, "models.dev catalog refreshed: %d providers, %d models, %d priced\ncache: %s\n",
		stats.Providers, stats.Models, stats.Priced, opts.CachePath)
	return nil
}
