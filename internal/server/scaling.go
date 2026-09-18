package server

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// SessionRouter provides session-affinity tracking for horizontally scaled
// deployments. When multiple server instances share a PostgreSQL backend,
// SessionRouter records which instance owns each session so that routing
// layers (load balancers, reverse proxies) can make informed decisions.
type SessionRouter struct {
	instanceID string
	instances  []string
	mu         sync.RWMutex
	sessions   map[string]string // sessionID -> instanceID
}

// NewSessionRouter creates a router for the given instance. If instanceID
// is empty, one is generated from the hostname, PID, and a random suffix.
func NewSessionRouter(instanceID string, fleetInstances ...string) *SessionRouter {
	if instanceID == "" {
		instanceID = generateInstanceID()
	}
	instances := normalizeInstances(fleetInstances)
	if len(instances) == 0 {
		instances = []string{instanceID}
	}
	return &SessionRouter{
		instanceID: instanceID,
		instances:  instances,
		sessions:   make(map[string]string),
	}
}

// Claim marks a session as owned by this instance.
func (r *SessionRouter) Claim(sessionID string) {
	r.mu.Lock()
	owner := r.instanceID
	if r.MultiInstance() {
		owner = r.owner(sessionID)
	}
	r.sessions[sessionID] = owner
	r.mu.Unlock()
}

// Owner returns the instance ID that owns the session, or "" if unclaimed.
func (r *SessionRouter) Owner(sessionID string) string {
	r.mu.RLock()
	owner := r.sessions[sessionID]
	r.mu.RUnlock()
	if owner != "" {
		return owner
	}
	if r.MultiInstance() {
		return r.owner(sessionID)
	}
	return ""
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

// MultiInstance reports whether deterministic fleet affinity is enabled.
func (r *SessionRouter) MultiInstance() bool { return len(r.instances) > 1 }

func (r *SessionRouter) owner(sessionID string) string {
	if sessionID == "" || len(r.instances) == 0 {
		return ""
	}
	digest := sha256.Sum256([]byte(sessionID))
	index := (uint64(digest[0])<<56 | uint64(digest[1])<<48 | uint64(digest[2])<<40 | uint64(digest[3])<<32 |
		uint64(digest[4])<<24 | uint64(digest[5])<<16 | uint64(digest[6])<<8 | uint64(digest[7])) % uint64(len(r.instances))
	return r.instances[index]
}

func normalizeInstances(values []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
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
