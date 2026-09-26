package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/config"
	"github.com/spawn08/chronos-code/internal/execution"
	"github.com/spawn08/chronos-code/internal/orchestrator"
	"github.com/spawn08/chronos-code/internal/security"
	"github.com/spawn08/chronos/sdk/agent"
)

func deliveryRequest(handler http.Handler, method, path, key, body, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if key != "" {
		request.Header.Set("Idempotency-Key", key)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestDeliveryAdmissionPersistsWithoutWorkerAndIsScoped(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "deliveries.db")
	store, err := execution.OpenDeliveryStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	config := ServerConfig{AuthType: "api_key", APIKey: "secret", TenantID: "tenant-a", RepositoryID: "repo-a", DeliveryStore: store}
	handler := New(nil, config).Handler()
	body := `{"goal":"ship feature","requirements":[{"statement":"feature works","checks":["go test ./..."]}]}`
	response := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "key-1", body, "secret")
	if response.Code != http.StatusCreated {
		t.Fatalf("admission status = %d: %s", response.Code, response.Body.String())
	}
	var admitted deliveryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	if admitted.ID == "" || admitted.State != execution.DeliveryAdmitted || admitted.Goal.Actor != "api-key" ||
		len(admitted.Requirements) != 1 || len(admitted.Requirements[0].Checks) != 1 ||
		strings.Contains(response.Body.String(), "key-1") || response.Header().Get("Location") != "/v1/deliveries/"+string(admitted.ID) {
		t.Fatalf("admission response = %s", response.Body.String())
	}
	response = deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "key-1", body, "secret")
	if response.Code != http.StatusCreated {
		t.Fatalf("retry status = %d: %s", response.Code, response.Body.String())
	}
	var retried deliveryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &retried); err != nil || retried.ID != admitted.ID || retried.Version != admitted.Version {
		t.Fatalf("retry = %+v, error = %v", retried, err)
	}
	response = deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "key-1", `{"goal":"different goal"}`, "secret")
	if response.Code != http.StatusConflict {
		t.Fatalf("conflict status = %d: %s", response.Code, response.Body.String())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = execution.OpenDeliveryStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	config.DeliveryStore = store
	response = deliveryRequest(New(nil, config).Handler(), http.MethodGet, "/v1/deliveries/"+string(admitted.ID), "", "", "secret")
	if response.Code != http.StatusOK {
		t.Fatalf("inspect after restart = %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "key-1") {
		t.Fatalf("inspection exposed admission key: %s", response.Body.String())
	}
	response = deliveryRequest(New(nil, config).Handler(), http.MethodPost, "/v1/deliveries", "key-1", body, "secret")
	if response.Code != http.StatusCreated {
		t.Fatalf("retry after restart = %d: %s", response.Code, response.Body.String())
	}
	if attempts, err := store.Attempts(ctx, execution.DeliveryScope{TenantID: "tenant-a", RepositoryID: "repo-a"}, admitted.ID); err != nil || len(attempts) != 0 {
		t.Fatalf("worker attempts = %v, error = %v", attempts, err)
	}
	if _, err := store.Claim(ctx, "worker", time.Minute); err != execution.ErrNoRunnableDelivery {
		t.Fatalf("admitted delivery became runnable: %v", err)
	}
	config.TenantID = "tenant-b"
	response = deliveryRequest(New(nil, config).Handler(), http.MethodGet, "/v1/deliveries/"+string(admitted.ID), "", "", "secret")
	if response.Code != http.StatusNotFound {
		t.Fatalf("other tenant inspect = %d", response.Code)
	}
	config.TenantID, config.RepositoryID = "tenant-a", "repo-b"
	response = deliveryRequest(New(nil, config).Handler(), http.MethodGet, "/v1/deliveries/"+string(admitted.ID), "", "", "secret")
	if response.Code != http.StatusNotFound {
		t.Fatalf("other repository inspect = %d", response.Code)
	}
}

func TestDeliveryAdmissionRequiresAuthenticatedAuthorizedStorage(t *testing.T) {
	store, err := execution.OpenDeliveryStore(context.Background(), filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := ServerConfig{AuthType: "api_key", APIKey: "secret", TenantID: "tenant", RepositoryID: "repo", DeliveryStore: store}
	request := func(cfg ServerConfig, key, body, token string) int {
		return deliveryRequest(New(nil, cfg).Handler(), http.MethodPost, "/v1/deliveries", key, body, token).Code
	}
	if code := request(config, "key", `{"goal":"test"}`, "wrong"); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", code)
	}
	config.Authorizer = authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"agent.list": {}}}
	if code := request(config, "key", `{"goal":"test"}`, "secret"); code != http.StatusForbidden {
		t.Fatalf("unauthorized status = %d", code)
	}
	config.Authorizer = nil
	for _, body := range []string{`{"goal":""}`, `{"goal":"test","repository_id":"repo-b"}`, `{"goal":"test","requirements":[{"statement":""}]}`} {
		if code := request(config, "key", body, "secret"); code != http.StatusBadRequest {
			t.Fatalf("invalid body %s status = %d", body, code)
		}
	}
	if code := request(config, "", `{"goal":"test"}`, "secret"); code != http.StatusBadRequest {
		t.Fatalf("missing key status = %d", code)
	}
	config.DeliveryStore = nil
	if code := request(config, "key", `{"goal":"test"}`, "secret"); code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable store status = %d", code)
	}
	config.AuthType = "none"
	if code := request(config, "key", `{"goal":"test"}`, "secret"); code != http.StatusForbidden {
		t.Fatalf("local unauthenticated status = %d", code)
	}
}

func TestPlanGenerationAdmissionRequiresExplicitPlanWorker(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := ServerConfig{AuthType: "api_key", APIKey: "secret", TenantID: "tenant", RepositoryID: "repo", DeliveryStore: store}
	worker, err := execution.NewWorker(store, parkedDeliveryExecutor{}, execution.WorkerConfig{OwnerID: "reader", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	config.DeliveryWorker = worker
	response := deliveryRequest(New(nil, config).Handler(), http.MethodPost, "/v1/deliveries", "plan-key", `{"goal":"add api","plan_generation":{}}`, "secret")
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured plan admission = %d: %s", response.Code, response.Body.String())
	}
	if deliveries, err := store.List(ctx, execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}); err != nil || len(deliveries) != 0 {
		t.Fatalf("unconfigured plan became runnable = %+v, error = %v", deliveries, err)
	}
}

func TestReadOnlyDeliveryAdmissionQueuesAtomicallyAndOutlivesRequest(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := ServerConfig{AuthType: "api_key", APIKey: "secret", TenantID: "tenant", RepositoryID: "repo", DeliveryStore: store}
	body := `{"goal":"inspect changes","run_read_only":true}`
	withoutWorker := deliveryRequest(New(nil, config).Handler(), http.MethodPost, "/v1/deliveries", "read-only-key", body, "secret")
	if withoutWorker.Code != http.StatusServiceUnavailable {
		t.Fatalf("admission without worker = %d", withoutWorker.Code)
	}
	if summaries, err := store.List(ctx, execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}); err != nil || len(summaries) != 0 {
		t.Fatalf("failed admission wrote records: %+v, error = %v", summaries, err)
	}
	worker, err := execution.NewWorker(store, parkedDeliveryExecutor{}, execution.WorkerConfig{OwnerID: "worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	config.DeliveryWorker = worker
	handler := New(nil, config).Handler()
	if capped := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "capped-read-only", `{"goal":"inspect changes","run_read_only":true,"max_cost_microdollars":50}`, "secret"); capped.Code != http.StatusServiceUnavailable {
		t.Fatalf("capped execution without model-call accounting = %d", capped.Code)
	}
	response := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "read-only-key", body, "secret")
	if response.Code != http.StatusAccepted {
		t.Fatalf("runnable admission = %d: %s", response.Code, response.Body.String())
	}
	var queued deliveryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &queued); err != nil || queued.State != execution.DeliveryQueued {
		t.Fatalf("admitted delivery = %+v, error = %v", queued, err)
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	response = deliveryRequest(handler, http.MethodGet, "/v1/deliveries/"+string(queued.ID), "", "", "secret")
	var waiting deliveryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &waiting); err != nil || response.Code != http.StatusOK || waiting.State != execution.DeliveryWaitingDecision {
		t.Fatalf("detached delivery after worker = %+v, status = %d, error = %v", waiting, response.Code, err)
	}
	if conflict := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "read-only-key", `{"goal":"inspect changes"}`, "secret"); conflict.Code != http.StatusConflict {
		t.Fatalf("same key with changed execution mode = %d", conflict.Code)
	}
}

type cappedParkedExecutor struct{ parkedDeliveryExecutor }

func (cappedParkedExecutor) SupportsCappedDelivery() bool { return true }

func TestCappedReadOnlyAdmissionNeedsAccountingCapableWorker(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := ServerConfig{AuthType: "api_key", APIKey: "secret", TenantID: "tenant", RepositoryID: "repo", DeliveryStore: store}
	worker, err := execution.NewWorker(store, cappedParkedExecutor{}, execution.WorkerConfig{OwnerID: "worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	config.DeliveryWorker = worker
	response := deliveryRequest(New(nil, config).Handler(), http.MethodPost, "/v1/deliveries", "capped", `{"goal":"inspect","run_read_only":true,"max_cost_microdollars":20000}`, "secret")
	if response.Code != http.StatusAccepted {
		t.Fatalf("capped admission = %d: %s", response.Code, response.Body.String())
	}
	if err := worker.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAuthenticatedTeamAdmissionBindsConfiguredCheckpointedWorker(t *testing.T) {
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	root := t.TempDir()
	indexOnStart := false
	agentConfig := func(id string) agent.AgentConfig {
		return agent.AgentConfig{ID: id, Name: id, Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test-key"}}
	}
	configured := &config.Config{FileConfig: agent.FileConfig{
		Defaults: &agent.AgentConfig{Storage: agent.StorageConfig{Backend: "sqlite", DSN: filepath.Join(root, "sessions.db")}},
		Agents:   []agent.AgentConfig{agentConfig("reader"), agentConfig("reviewer")},
		Teams: []agent.TeamConfig{{ID: "pair", Name: "Pair", Strategy: "sequential", Agents: []string{"reader", "reviewer"}},
			{ID: "fan", Name: "Fan", Strategy: "parallel", Agents: []string{"reader", "reviewer"}},
			{ID: "route", Name: "Route", Strategy: "router", Agents: []string{"reader", "reviewer"}}},
	}, Workspace: config.WorkspaceConfig{Root: root, IndexOnStart: &indexOnStart}}
	orch, err := orchestrator.New(context.Background(), configured, "")
	if err != nil {
		t.Fatal(err)
	}
	defer orch.Close()
	store, err := execution.OpenDeliveryStore(context.Background(), filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	executor, err := orchestrator.NewReadOnlyDeliveryExecutor(orch, authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, executor, execution.WorkerConfig{OwnerID: "worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil || !worker.CanRunCheckpointedTeam() {
		t.Fatalf("team worker = %+v, error = %v", worker, err)
	}
	handler := New(orch, ServerConfig{AuthType: "api_key", APIKey: "secret", TenantID: "tenant", RepositoryID: "repo", DeliveryStore: store, DeliveryWorker: worker}).Handler()
	response := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "team-key", `{"goal":"inspect together","run_read_only":true,"team_id":"pair"}`, "secret")
	if response.Code != http.StatusAccepted {
		t.Fatalf("team admission = %d: %s", response.Code, response.Body.String())
	}
	var admitted deliveryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}, admitted.ID)
	if err != nil || loaded.PolicyReference != execution.ReadOnlyTeamPolicyPrefix+"pair" || loaded.State != execution.DeliveryQueued {
		t.Fatalf("trusted team route = %+v, error = %v", loaded, err)
	}
	invalid := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "unknown-team", `{"goal":"inspect","run_read_only":true,"team_id":"missing"}`, "secret")
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("unknown team admission = %d", invalid.Code)
	}
	if parallel := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "parallel-team", `{"goal":"inspect","run_read_only":true,"team_id":"fan"}`, "secret"); parallel.Code != http.StatusAccepted {
		t.Fatalf("parallel team admission = %d: %s", parallel.Code, parallel.Body.String())
	}
	if router := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "router-team", `{"goal":"inspect","run_read_only":true,"team_id":"route"}`, "secret"); router.Code != http.StatusBadRequest {
		t.Fatalf("router team without member checkpoints admitted = %d", router.Code)
	}
}

func TestDeliveryCostAuthorityIsScopedAndImmutableAtAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := execution.OpenDeliveryStore(ctx, filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	handler := New(nil, ServerConfig{AuthType: "api_key", APIKey: "secret", TenantID: "tenant", RepositoryID: "repo", DeliveryStore: store}).Handler()
	request := `{"goal":"build feature","max_cost_microdollars":100}`
	response := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "budget-key", request, "secret")
	var admitted deliveryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &admitted); err != nil || response.Code != http.StatusCreated || admitted.MaxCostMicrodollars != 100 || !admitted.Usage.CostKnown {
		t.Fatalf("capped admission = %+v, status = %d, error = %v", admitted, response.Code, err)
	}
	if changed := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "budget-key", `{"goal":"build feature","max_cost_microdollars":200}`, "secret"); changed.Code != http.StatusConflict {
		t.Fatalf("changed authority under same key = %d", changed.Code)
	}
	if invalid := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "invalid", `{"goal":"build feature","max_cost_microdollars":-1}`, "secret"); invalid.Code != http.StatusBadRequest {
		t.Fatalf("negative authority accepted: %d", invalid.Code)
	}
}

// TestCandidatePlanAdmissionFromProductionStartup exercises orchestrator.New,
// the candidate plan executor and the routed worker without the interactive
// closed-loop PPD capability. The worker is not run: it would call a provider.
func TestCandidatePlanAdmissionFromProductionStartup(t *testing.T) {
	t.Setenv("CHRONOS_CODE_DATA_HOME", t.TempDir())
	root := t.TempDir()
	indexOnStart := false
	configured := &config.Config{FileConfig: agent.FileConfig{
		Defaults: &agent.AgentConfig{Storage: agent.StorageConfig{Backend: "sqlite", DSN: filepath.Join(root, "sessions.db")}},
		Agents:   []agent.AgentConfig{{ID: "reader", Name: "reader", Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o-mini", APIKey: "test-key"}}},
	}, Workspace: config.WorkspaceConfig{Root: root, IndexOnStart: &indexOnStart}}
	orch, err := orchestrator.New(context.Background(), configured, "")
	if err != nil {
		t.Fatal(err)
	}
	defer orch.Close()
	store, err := execution.OpenDeliveryStore(context.Background(), filepath.Join(t.TempDir(), "deliveries.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	authorizer := authorization.RepositoryAuthorizer{RepositoryID: "repo", AllowedActions: map[string]struct{}{"delivery.execute": {}}}
	readOnly, err := orchestrator.NewReadOnlyDeliveryExecutor(orch, authorizer, security.SandboxPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orchestrator.NewPlanDeliveryExecutor(orch, authorizer, security.SandboxPolicy{}); err == nil {
		t.Fatal("write plan executor started without the closed-loop PPD capability")
	}
	candidate, err := orchestrator.NewCandidatePlanDeliveryExecutor(orch, authorizer, security.SandboxPolicy{})
	if err != nil {
		t.Fatalf("candidate plan executor from production startup: %v", err)
	}
	routed, err := orchestrator.NewRoutedDeliveryExecutor(readOnly, candidate)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := execution.NewWorker(store, routed, execution.WorkerConfig{OwnerID: "worker", Concurrency: 1, LeaseDuration: time.Minute, HeartbeatEvery: time.Second, PollEvery: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	handler := New(orch, ServerConfig{AuthType: "api_key", APIKey: "secret", TenantID: "tenant", RepositoryID: "repo", DeliveryStore: store, DeliveryWorker: worker}).Handler()
	proposal := `{"source_request_ref":"source","classifier_ref":"classifier","nodes":[{"id":"a","kind":"implement","objective":"add api","depends_on":[],"scope":"api.go","context_refs":[],"expected_artifacts":["api.go"],"assumptions":[],"invalidation_triggers":[],"recovery_class":"replan","risks":["compatibility"],"verification":"go test ./..."}]}`
	body := `{"goal":"add api","plan_generation":` + proposal + `}`
	if capped := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "capped-plan", `{"goal":"add api","max_cost_microdollars":10,"plan_generation":`+proposal+`}`, "secret"); capped.Code != http.StatusServiceUnavailable {
		t.Fatalf("capped candidate plan admission = %d: %s", capped.Code, capped.Body.String())
	}
	response := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "plan-key", body, "secret")
	if response.Code != http.StatusAccepted {
		t.Fatalf("candidate plan admission = %d: %s", response.Code, response.Body.String())
	}
	var admitted deliveryResponse
	if err := json.Unmarshal(response.Body.Bytes(), &admitted); err != nil {
		t.Fatal(err)
	}
	scope := execution.DeliveryScope{TenantID: "tenant", RepositoryID: "repo"}
	loaded, err := store.Load(context.Background(), scope, admitted.ID)
	if err != nil || loaded.PolicyReference != execution.CandidatePlanPolicyReference || loaded.State != execution.DeliveryQueued {
		t.Fatalf("candidate plan route = %+v, error = %v", loaded, err)
	}
	if retry := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "plan-key", body, "secret"); retry.Code != http.StatusAccepted {
		t.Fatalf("retried candidate plan admission = %d: %s", retry.Code, retry.Body.String())
	}
	changed := strings.Replace(body, "add api", "remove api", 2)
	if conflict := deliveryRequest(handler, http.MethodPost, "/v1/deliveries", "plan-key", changed, "secret"); conflict.Code != http.StatusConflict {
		t.Fatalf("changed proposal under same key = %d: %s", conflict.Code, conflict.Body.String())
	}
}
