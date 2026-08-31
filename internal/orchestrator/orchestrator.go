package orchestrator

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/sdk/agent"
	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/sqlite"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/graph"
	"github.com/spawn08/chronos-code/internal/toolcompress"
)

type Orchestrator struct {
	agents     map[string]*agent.Agent
	order      []string
	active     string
	store      storage.Storage
	cfg        *config.Config
	graphStore *graph.Store
	watcher    *graph.Watcher
}

func New(ctx context.Context, cfg *config.Config) (*Orchestrator, error) {
	store, err := openStorage(cfg)
	if err != nil {
		return nil, fmt.Errorf("open storage: %w", err)
	}

	agents, err := agent.BuildAll(ctx, &cfg.FileConfig)
	if err != nil {
		return nil, fmt.Errorf("build agents: %w", err)
	}

	order := make([]string, 0, len(agents))
	for _, ac := range cfg.Agents {
		if _, ok := agents[ac.ID]; ok {
			order = append(order, ac.ID)
		}
	}
	sort.Strings(order)

	for _, a := range agents {
		if a.Storage == nil {
			a.Storage = store
		}
	}

	graphStore, watcher := setupGraph(ctx, cfg, agents)

	for _, a := range agents {
		toolcompress.Wrap(a, cfg.Tools.CompressionThresholdTokens)
		if err := a.ConnectMCP(ctx); err != nil {
			fmt.Printf("warning: MCP connect for %s: %v\n", a.ID, err)
		}
	}

	active := "coder"
	if _, ok := agents[active]; !ok && len(order) > 0 {
		active = order[0]
	}

	return &Orchestrator{
		agents:     agents,
		order:      order,
		active:     active,
		store:      store,
		cfg:        cfg,
		graphStore: graphStore,
		watcher:    watcher,
	}, nil
}

// setupGraph opens the code graph store, indexes the workspace on start
// (PRD P1-007), registers the T0 graph tools on every agent, and starts a
// background watcher to keep the graph fresh. Any failure (e.g. no Go module
// at the workspace root) is logged as a warning and treated as non-fatal —
// the harness still works without the graph, just without the T0 navigation
// tools.
func setupGraph(ctx context.Context, cfg *config.Config, agents map[string]*agent.Agent) (*graph.Store, *graph.Watcher) {
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
			fmt.Printf("warning: create graph db dir %s: %v\n", dir, err)
			return nil, nil
		}
	}

	graphStore, err := graph.OpenStore(dbPath)
	if err != nil {
		fmt.Printf("warning: open graph store: %v\n", err)
		return nil, nil
	}

	ix := graph.NewIndexer(graphStore, root)
	indexOnStart := cfg.Workspace.IndexOnStart == nil || *cfg.Workspace.IndexOnStart
	if indexOnStart {
		stats, err := ix.IndexAll(ctx)
		if err != nil {
			fmt.Printf("warning: index code graph: %v\n", err)
		} else {
			fmt.Printf("code graph: indexed %d files, %d symbols, %d edges in %s\n",
				stats.Files, stats.Symbols, stats.Edges, stats.Elapsed.Round(1e6))
		}
	}

	for _, a := range agents {
		for _, def := range graph.Tools(graphStore, root) {
			a.Tools.Register(def)
		}
	}

	var watcher *graph.Watcher
	if indexOnStart {
		watcher, err = graph.Watch(ctx, ix)
		if err != nil {
			fmt.Printf("warning: start graph watcher: %v\n", err)
			watcher = nil
		}
	}

	return graphStore, watcher
}

func (o *Orchestrator) Chat(ctx context.Context, message string) (*model.ChatResponse, error) {
	a := o.ActiveAgent()
	if a == nil {
		return nil, fmt.Errorf("no active agent")
	}
	return a.Chat(ctx, message)
}

func (o *Orchestrator) ChatStream(ctx context.Context, message string) (<-chan *model.ChatResponse, error) {
	a := o.ActiveAgent()
	if a == nil {
		return nil, fmt.Errorf("no active agent")
	}
	return a.ChatStream(ctx, message)
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

func (o *Orchestrator) ActiveID() string {
	return o.active
}

func (o *Orchestrator) ListAgents() []string {
	return o.order
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

func (o *Orchestrator) Close() error {
	for _, a := range o.agents {
		a.CloseMCP()
	}
	if o.watcher != nil {
		o.watcher.Close()
	}
	if o.graphStore != nil {
		o.graphStore.Close()
	}
	if o.store != nil {
		return o.store.Close()
	}
	return nil
}

func openStorage(cfg *config.Config) (storage.Storage, error) {
	backend := "sqlite"
	dsn := ".chronos-code/sessions.db"
	if cfg.Defaults != nil {
		if cfg.Defaults.Storage.Backend != "" {
			backend = cfg.Defaults.Storage.Backend
		}
		if cfg.Defaults.Storage.DSN != "" {
			dsn = cfg.Defaults.Storage.DSN
		}
	}
	switch backend {
	case "sqlite":
		if dir := filepath.Dir(dsn); dir != "." && dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("create storage dir %s: %w", dir, err)
			}
		}
		store, err := sqlite.New(dsn)
		if err != nil {
			return nil, fmt.Errorf("sqlite: %w", err)
		}
		if err := store.Migrate(context.Background()); err != nil {
			return nil, fmt.Errorf("sqlite migrate: %w", err)
		}
		return store, nil
	default:
		return nil, fmt.Errorf("unsupported storage backend: %s", backend)
	}
}
