package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/execution"
)

type deliveryAdmissionRequest struct {
	Goal                string `json:"goal"`
	RunReadOnly         bool   `json:"run_read_only,omitempty"`
	MaxCostMicrodollars int64  `json:"max_cost_microdollars,omitempty"`
	Requirements []struct {
		Statement string   `json:"statement"`
		Checks    []string `json:"checks"`
	} `json:"requirements,omitempty"`
}

type deliveryResponse struct {
	ID                  execution.DeliveryID    `json:"id"`
	State               execution.DeliveryState `json:"state"`
	Version             int64                   `json:"version"`
	CurrentGoalRevision execution.GoalRevision  `json:"current_goal_revision"`
	MaxCostMicrodollars int64                   `json:"max_cost_microdollars,omitempty"`
	Usage               execution.CumulativeUsage `json:"usage"`
	Goal                execution.Goal          `json:"goal"`
	Requirements        []execution.Requirement `json:"requirements"`
	CreatedAt           time.Time               `json:"created_at"`
}

func responseForDelivery(delivery execution.Delivery) deliveryResponse {
	return deliveryResponse{
		ID: delivery.ID, State: delivery.State, Version: delivery.Version,
		CurrentGoalRevision: delivery.CurrentGoalRevision, MaxCostMicrodollars: delivery.MaxCostMicrodollars,
		Goal: delivery.Goals[len(delivery.Goals)-1],
		Requirements: delivery.Requirements, CreatedAt: delivery.CreatedAt,
	}
}

// Admission persists a goal for later scheduling. Until a production executor
// is configured, it remains admitted rather than being placed on the queue.
func (s *Server) handleAdmitDelivery(w http.ResponseWriter, r *http.Request) {
	if !s.deliveryAvailable(w) {
		return
	}
	authority, ok := authorization.FromContext(r.Context())
	if !ok || authority.Action != "delivery.admit" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "delivery admission is not authorized"})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" || len(key) > 256 || strings.TrimSpace(key) != key {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Idempotency-Key is required (maximum 256 bytes)"})
		return
	}
	var request deliveryAdmissionRequest
	if err := decodeJSONBody(r, &request); err != nil {
		writeJSON(w, jsonErrorStatus(err), map[string]string{"error": "invalid JSON: " + err.Error()})
		return
	}
	if strings.TrimSpace(request.Goal) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "goal is required"})
		return
	}
	if request.MaxCostMicrodollars < 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "max_cost_microdollars must be non-negative"})
		return
	}
	if request.RunReadOnly && request.MaxCostMicrodollars > 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delivery spending caps require durable model-call admission"})
		return
	}
	if request.RunReadOnly && s.cfg.DeliveryWorker == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "read-only delivery worker is unavailable"})
		return
	}
	scope := execution.DeliveryScope{TenantID: execution.TenantID(authority.TenantID), RepositoryID: execution.RepositoryID(authority.RepositoryID)}
	digest := sha256.Sum256([]byte(authority.TenantID + "\x00" + authority.RepositoryID + "\x00" + key))
	id := execution.DeliveryID(hex.EncodeToString(digest[:]))
	requirements := make([]execution.Requirement, 0, len(request.Requirements))
	for i, item := range request.Requirements {
		if strings.TrimSpace(item.Statement) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "requirement statement is required"})
			return
		}
		requirement := execution.Requirement{ID: execution.RequirementID(fmt.Sprintf("requirement-%d", i+1)), Statement: item.Statement, Status: execution.RequirementAccepted}
		for j, check := range item.Checks {
			if strings.TrimSpace(check) == "" {
				writeJSON(w, http.StatusBadRequest, map[string]string{"error": "acceptance check statement is required"})
				return
			}
			requirement.Checks = append(requirement.Checks, execution.AcceptanceCheck{ID: execution.AcceptanceCheckID(fmt.Sprintf("check-%d", j+1)), Statement: check})
		}
		requirements = append(requirements, requirement)
	}
	if len(requirements) == 0 {
		requirements = append(requirements, execution.Requirement{ID: "requirement-1", Statement: request.Goal, Status: execution.RequirementAccepted})
	}
	policyReference := "admission-v1"
	if request.RunReadOnly {
		policyReference = "admission-readonly-v1"
	}
	admission := execution.Admission{
		Scope: scope, DeliveryID: id, AdmissionKey: execution.AdmissionKey(key),
		Goal:         execution.Goal{Statement: request.Goal, Actor: authority.PrincipalID},
		Requirements: requirements, PolicyReference: policyReference, MaxCostMicrodollars: request.MaxCostMicrodollars,
		Event: execution.EventIdentity{ID: execution.DeliveryEventID("admit:" + string(id)), IdempotencyKey: execution.DeliveryIdempotencyKey("admit:" + string(id))},
	}
	var delivery execution.Delivery
	var err error
	if request.RunReadOnly {
		delivery, err = s.cfg.DeliveryStore.AdmitRunnable(r.Context(), admission)
	} else {
		delivery, err = s.cfg.DeliveryStore.Admit(r.Context(), admission)
	}
	if err != nil {
		if errors.Is(err, execution.ErrAdmissionConflict) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "Idempotency-Key was used for another admission"})
			return
		}
		s.logger.Error("delivery_admission_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delivery admission failed"})
		return
	}
	w.Header().Set("Location", "/v1/deliveries/"+string(delivery.ID))
	usage, err := s.cfg.DeliveryStore.Usage(r.Context(), scope, delivery.ID)
	if err != nil {
		s.logger.Error("delivery_usage_inspection_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delivery admission persisted; inspection unavailable"})
		return
	}
	status := http.StatusCreated
	if request.RunReadOnly {
		status = http.StatusAccepted
	}
	response := responseForDelivery(delivery)
	response.Usage = usage
	writeJSON(w, status, response)
}

func (s *Server) handleInspectDelivery(w http.ResponseWriter, r *http.Request) {
	if !s.deliveryAvailable(w) {
		return
	}
	authority, ok := authorization.FromContext(r.Context())
	if !ok || authority.Action != "delivery.inspect" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "delivery inspection is not authorized"})
		return
	}
	id := r.PathValue("id")
	if len(id) != 64 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "delivery not found"})
		return
	}
	delivery, err := s.cfg.DeliveryStore.Load(r.Context(), execution.DeliveryScope{
		TenantID: execution.TenantID(authority.TenantID), RepositoryID: execution.RepositoryID(authority.RepositoryID),
	}, execution.DeliveryID(id))
	if errors.Is(err, execution.ErrDeliveryNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "delivery not found"})
		return
	}
	if err != nil {
		s.logger.Error("delivery_inspection_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delivery inspection failed"})
		return
	}
	usage, err := s.cfg.DeliveryStore.Usage(r.Context(), delivery.DeliveryScope, delivery.ID)
	if err != nil {
		s.logger.Error("delivery_usage_inspection_failed", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "delivery usage inspection failed"})
		return
	}
	response := responseForDelivery(delivery)
	response.Usage = usage
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) deliveryAvailable(w http.ResponseWriter) bool {
	if s.cfg.AuthType == "none" {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "delivery admission requires authentication"})
		return false
	}
	if s.cfg.DeliveryStore == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "delivery storage is unavailable"})
		return false
	}
	return true
}
