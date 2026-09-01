//go:build postgres

package orchestrator

import (
	"context"
	"fmt"

	"github.com/spawn08/chronos/storage"
	"github.com/spawn08/chronos/storage/adapters/postgres"
)

var postgresOpener = func(dsn string) (storage.Storage, error) {
	store, err := postgres.New(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: %w", err)
	}
	if err := store.Migrate(context.Background()); err != nil {
		return nil, fmt.Errorf("postgres migrate: %w", err)
	}
	return store, nil
}
