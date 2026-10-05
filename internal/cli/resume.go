package cli

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos/storage/adapters/sqlite"
)

// errSessionNotFound reports that --resume named a session that no project
// database in the active data home contains.
var errSessionNotFound = errors.New("session not found")

// ExitSessionNotFound is the process status for an unknown --resume id, so a
// caller can retry without resume.
const ExitSessionNotFound = 8

// locateResumeSession finds the sessions database holding id. The current
// project's database is checked first, then every other project under the
// same data home, so a session survives a changed working directory. When it
// lives elsewhere, cfg is pointed at that database and the run continues in
// the current directory. Non-SQLite backends are left unchecked.
func locateResumeSession(ctx context.Context, cfg *config.Config, id string) error {
	paths, err := cfg.ResolveProjectPaths("")
	if err != nil {
		return fmt.Errorf("resolve project paths: %w", err)
	}
	if cfg.Defaults != nil && cfg.Defaults.Storage.Backend != "" && cfg.Defaults.Storage.Backend != "sqlite" {
		return nil
	}
	candidates := []string{paths.SessionsDB}
	home := filepath.Dir(filepath.Dir(paths.Dir))
	others, _ := filepath.Glob(filepath.Join(home, "projects", "*", "sessions.db"))
	for _, other := range others {
		if filepath.Clean(other) != filepath.Clean(paths.SessionsDB) {
			candidates = append(candidates, other)
		}
	}
	for i, path := range candidates {
		found, err := databaseHasSession(ctx, path, id)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if i > 0 {
			cfg.SessionsDBOverride = path
		}
		return nil
	}
	return fmt.Errorf("%w: %q", errSessionNotFound, id)
}

func databaseHasSession(ctx context.Context, path, id string) (bool, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("stat sessions database: %w", err)
	}
	store, err := sqlite.New(path)
	if err != nil {
		return false, fmt.Errorf("open sessions database %s: %w", path, err)
	}
	defer store.Close()
	if _, err := store.GetSession(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("look up session in %s: %w", path, err)
	}
	return true, nil
}
