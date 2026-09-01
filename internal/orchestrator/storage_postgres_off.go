//go:build !postgres

package orchestrator

import "github.com/spawn08/chronos/storage"

var postgresOpener func(string) (storage.Storage, error)
