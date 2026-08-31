package server

import (
	"crypto/rand"
	"fmt"
	"os"
	"sync"
)

// SessionRouter provides session-affinity tracking for horizontally scaled
// deployments. When multiple server instances share a PostgreSQL backend,
// SessionRouter records which instance owns each session so that routing
// layers (load balancers, reverse proxies) can make informed decisions.
type SessionRouter struct {
	instanceID string
	mu         sync.RWMutex
	sessions   map[string]string // sessionID -> instanceID
}

// NewSessionRouter creates a router for the given instance. If instanceID
// is empty, one is generated from the hostname, PID, and a random suffix.
func NewSessionRouter(instanceID string) *SessionRouter {
	if instanceID == "" {
		instanceID = generateInstanceID()
	}
	return &SessionRouter{
		instanceID: instanceID,
		sessions:   make(map[string]string),
	}
}

// Claim marks a session as owned by this instance.
func (r *SessionRouter) Claim(sessionID string) {
	r.mu.Lock()
	r.sessions[sessionID] = r.instanceID
	r.mu.Unlock()
}

// Owner returns the instance ID that owns the session, or "" if unclaimed.
func (r *SessionRouter) Owner(sessionID string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.sessions[sessionID]
}

// IsLocal returns true if the session is owned by this instance.
func (r *SessionRouter) IsLocal(sessionID string) bool {
	return r.Owner(sessionID) == r.instanceID
}

// Release removes ownership of a session.
func (r *SessionRouter) Release(sessionID string) {
	r.mu.Lock()
	delete(r.sessions, sessionID)
	r.mu.Unlock()
}

// InstanceID returns this server's unique instance ID.
func (r *SessionRouter) InstanceID() string {
	return r.instanceID
}

func generateInstanceID() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	buf := make([]byte, 4)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("%s-%d-%x", host, os.Getpid(), buf)
}
