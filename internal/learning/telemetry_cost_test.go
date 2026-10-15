package learning

import (
	"context"
	"database/sql"
	"math"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos/engine/hooks"
	"github.com/spawn08/chronos/engine/model"
	"github.com/spawn08/chronos/storage"

	"github.com/spawn08/chronos-code/internal/budget"
)

func TestTelemetryRecordsCacheTokensAndCost(t *testing.T) {
	tests := []struct {
		name         string
		model        string
		usage        model.Usage
		wantUnpriced int
	}{
		{
			name:  "priced anthropic call with cache",
			model: "claude-sonnet-5-5",
			usage: model.Usage{PromptTokens: 400, CompletionTokens: 1000, CacheReadTokens: 100000, CacheCreationTokens: 20000},
		},
		{
			name:         "unknown model is unpriced, not free",
			model:        "unknown-model-x",
			usage:        model.Usage{PromptTokens: 10, CompletionTokens: 10},
			wantUnpriced: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := storage.WithSession(context.Background(), "s1")
			store := openTestSQLStore(t)
			recorder := NewTelemetryRecorder(store, "/repo", "coder")
			evt := &hooks.Event{Type: hooks.EventModelCallBefore, Name: tt.model, Metadata: map[string]any{"correlation_id": "c1"}}
			if err := recorder.Before(ctx, evt); err != nil {
				t.Fatalf("Before: %v", err)
			}
			evt.Type = hooks.EventModelCallAfter
			// As in production: the response carries no model; the request does.
			evt.Input = &model.ChatRequest{Model: tt.model}
			evt.Output = &model.ChatResponse{Usage: tt.usage}
			if err := recorder.After(ctx, evt); err != nil {
				t.Fatalf("After: %v", err)
			}

			var input, output, cacheRead, cacheWrite, unpriced int
			var cost float64
			var ended sql.NullString
			if err := store.db.QueryRow(`SELECT input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, cost_usd, unpriced_calls, ended_at FROM sessions WHERE id = 's1'`).
				Scan(&input, &output, &cacheRead, &cacheWrite, &cost, &unpriced, &ended); err != nil {
				t.Fatalf("query: %v", err)
			}
			if input != tt.usage.UncachedPromptTokens() || output != tt.usage.CompletionTokens ||
				cacheRead != tt.usage.CacheReadTokens || cacheWrite != tt.usage.CacheCreationTokens {
				t.Errorf("tokens = %d/%d/%d/%d, want %d/%d/%d/%d", input, output, cacheRead, cacheWrite,
					tt.usage.UncachedPromptTokens(), tt.usage.CompletionTokens, tt.usage.CacheReadTokens, tt.usage.CacheCreationTokens)
			}
			if unpriced != tt.wantUnpriced {
				t.Errorf("unpriced_calls = %d, want %d", unpriced, tt.wantUnpriced)
			}
			if !ended.Valid {
				t.Error("ended_at not set after a completed call")
			}
			want := 0.0
			if tt.wantUnpriced == 0 {
				price, err := budget.PriceForModel(tt.model)
				if err != nil {
					t.Fatalf("PriceForModel: %v", err)
				}
				micros, err := price.IncurredCost(tt.usage)
				if err != nil {
					t.Fatalf("IncurredCost: %v", err)
				}
				want = float64(micros) / 1e6
				if want == 0 {
					t.Fatal("expected a non-zero cost for a priced model")
				}
			}
			if math.Abs(cost-want) > 1e-9 {
				t.Errorf("cost_usd = %v, want %v (budget cost)", cost, want)
			}
		})
	}
}

func TestMigrateUpgradesVersionOneDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(schemaV1 + "; PRAGMA user_version = 1;"); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sessions (id, repo_path, started_at, input_tokens) VALUES ('old', '/r', '2026-01-01T00:00:00Z', 7)`); err != nil {
		t.Fatalf("seed row: %v", err)
	}
	db.Close()

	store, err := OpenSQLStore(ctx, path)
	if err != nil {
		t.Fatalf("OpenSQLStore: %v", err)
	}
	defer store.Close(ctx)
	var input, cacheRead int
	if err := store.db.QueryRow(`SELECT input_tokens, cache_read_tokens FROM sessions WHERE id = 'old'`).Scan(&input, &cacheRead); err != nil {
		t.Fatalf("query migrated row: %v", err)
	}
	if input != 7 || cacheRead != 0 {
		t.Errorf("migrated row = %d/%d, want 7/0", input, cacheRead)
	}
}
