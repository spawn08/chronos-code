package modelsdev

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Options configure Init and Refresh.
type Options struct {
	// CachePath is the catalog cache file (see CachePath).
	CachePath string
	// URL is the models.dev base URL; the catalog is <URL>/api.json.
	URL       string
	UserAgent string
	// AutoRefresh allows Init to refresh a stale cache in the background.
	// It is ignored when DisableFetchEnv is set.
	AutoRefresh bool
	Client      *http.Client
	Now         func() time.Time
}

// Refresh fetches the catalog, writes the cache, and applies it. On failure
// the cache and the active catalog are left unchanged.
func Refresh(ctx context.Context, opts Options) (Catalog, error) {
	catalog, err := Fetch(ctx, opts.Client, opts.URL, opts.UserAgent)
	if err != nil {
		return nil, err
	}
	if err := Save(opts.CachePath, catalog); err != nil {
		return nil, err
	}
	Apply(catalog)
	return catalog, nil
}

// Init applies the cached catalog, if any, synchronously so the first model
// call already uses it, then starts a background refresh when the cache is
// missing or older than CacheTTL and automatic fetching is allowed. It never
// blocks on the network. The returned channel receives the background
// result (nil when no refresh was needed) and is closed; callers may ignore
// it. A corrupt cache is reported and treated as missing.
func Init(ctx context.Context, opts Options) (<-chan error, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}
	catalog, modTime, loadErr := Load(opts.CachePath)
	if loadErr == nil {
		Apply(catalog)
	} else if errors.Is(loadErr, fs.ErrNotExist) {
		loadErr = nil
	}

	done := make(chan error, 1)
	if !opts.AutoRefresh || fetchDisabled() || (loadErr == nil && catalog != nil && !Stale(modTime, now())) {
		close(done)
		return done, loadErr
	}
	go func() {
		defer close(done)
		if _, err := Refresh(ctx, opts); err != nil {
			done <- fmt.Errorf("background models.dev refresh: %w", err)
		}
	}()
	return done, loadErr
}

// fetchDisabled reports whether DisableFetchEnv opts out of automatic
// fetches. Any value other than a false boolean ("0", "false") disables.
func fetchDisabled() bool {
	value := strings.TrimSpace(os.Getenv(DisableFetchEnv))
	if value == "" {
		return false
	}
	disabled, err := strconv.ParseBool(value)
	return err != nil || disabled
}
