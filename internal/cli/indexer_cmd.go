package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/graph"
)

const indexerUsage = "usage: chronos-code indexer mcp [--repo [name=]dir ...]"

func runIndexer() error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runIndexerCommand(ctx, cfg, os.Args[2:], os.Stdin, os.Stdout)
}

// runIndexerCommand dispatches "chronos-code indexer". "mcp" serves the
// code index's graph and impact tools to MCP hosts over stdio (M9); stdout
// carries only JSON-RPC.
func runIndexerCommand(ctx context.Context, cfg *config.Config, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "mcp" {
		return errors.New(indexerUsage)
	}
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		return fmt.Errorf("resolve project paths: %w", err)
	}
	federation, err := indexerFederation(paths.Root, cfg.Workspace.Indexer.FederationRoots(paths.Root), args[1:])
	if err != nil {
		return err
	}
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		return fmt.Errorf("create project data directory: %w", err)
	}
	scope, err := graph.NewIndexScope(ctx, graph.IndexScopeOptions{
		Root: paths.Root, DataDir: paths.Dir, IndexOnStart: true, Watch: true, Federation: federation,
		Precise: cfg.Workspace.Indexer.PreciseOrDefault(),
	})
	if err != nil {
		return fmt.Errorf("open code index: %w", err)
	}
	defer scope.Close()
	if err := graph.NewMCPServer(scope, Version).ServeStdio(ctx, stdin, stdout); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

// indexerFederation combines workspace.indexer.federation with --repo
// flags ([name=]dir, relative to the workspace root).
func indexerFederation(root string, configured []config.FederatedRepo, args []string) ([]graph.FederatedRoot, error) {
	out := make([]graph.FederatedRoot, 0, len(configured))
	for _, r := range configured {
		out = append(out, graph.FederatedRoot{Name: r.Name, Root: r.Root})
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		var value string
		switch {
		case arg == "--repo" && i+1 < len(args):
			i++
			value = args[i]
		case strings.HasPrefix(arg, "--repo="):
			value = strings.TrimPrefix(arg, "--repo=")
		default:
			return nil, errors.New(indexerUsage)
		}
		name, dir, named := strings.Cut(value, "=")
		if !named {
			name, dir = "", value
		}
		if dir == "" {
			return nil, errors.New(indexerUsage)
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		out = append(out, graph.FederatedRoot{Name: name, Root: dir})
	}
	return out, nil
}
