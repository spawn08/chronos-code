package server

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/spawn08/chronos-code/internal/authorization"
	"github.com/spawn08/chronos-code/internal/execution"
)

type deliveryAdmissionRequest struct {
	Goal         string `json:"goal"`
	Requirements []struct {
		Statement string   `json:"statement"`
		Checks    []string `json:"checks"`
	} `json:"requirements,omitempty"`
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
	delivery, err := s.cfg.DeliveryStore.Admit(r.Context(), execution.Admission{
		Scope: scope, DeliveryID: id, AdmissionKey: execution.AdmissionKey(key),
		Goal: execution.Goal{Statement: request.Goal, Actor: authority.PrincipalID},
		Requirements: requirements, PolicyReference: "admission-v1",
		Event: execution.EventIdentity{ID: execution.DeliveryEventID("admit:" + string(id)), IdempotencyKey: execution.DeliveryIdempotencyKey("admit:" + string(id))},
	})
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
	writeJSON(w, http.StatusCreated, delivery)
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
	writeJSON(w, http.StatusOK, delivery)
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
