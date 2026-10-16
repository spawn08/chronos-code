package orchestrator

import (
	"context"
	"crypto/rand"
	"strings"

	"github.com/spawn08/chronos/storage"
)

func (r *configuredAgentRunner) saveSubagentReport(ctx context.Context, name, result string, runErr error) (string, error) {
	if strings.TrimSpace(result) == "" || (len(result) < 4000 && runErr == nil && ctx.Err() == nil) {
		return "", nil
	}
	store := r.store
	if store == nil {
		if child := r.agents[name]; child != nil {
			store = child.Storage
		}
	}
	if _, ok := store.(storage.SessionFileStore); !ok || storage.SessionFromContext(ctx) == "" {
		return "", nil
	}
	status := "reported"
	if runErr != nil || ctx.Err() != nil || strings.HasPrefix(result, "STATUS: partial") {
		status = "partial"
	}
	id := "subagent:" + rand.Text()
	durable, cancel := durableObservationContext(ctx)
	defer cancel()
	if err := saveArtifact(durable, store, id, map[string]any{"kind": "subagent_report", "agent": name, "status": status, "result": result, "error": errorText(runErr)}); err != nil {
		return "", err
	}
	return id, nil
}
