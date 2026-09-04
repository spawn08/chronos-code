package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
)

var ErrStaleContextFingerprint = errors.New("stale restart context fingerprint")

// ContextEntry is one content-bearing item available when restarting a node.
type ContextEntry struct {
	ID      ContextID
	Content string
}

// RestartContext is the bounded, canonical context reconstructed for a restart.
type RestartContext struct {
	Entries     []ContextEntry
	Fingerprint string
	Truncated   bool
}

// BuildRestartContext returns a canonical context whose content does not exceed
// maxBytes. A non-empty expectedFingerprint must match the bounded payload.
func BuildRestartContext(entries []ContextEntry, maxBytes int, expectedFingerprint string) (RestartContext, error) {
	ordered := append([]ContextEntry(nil), entries...)
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].ID != ordered[j].ID {
			return ordered[i].ID < ordered[j].ID
		}
		return ordered[i].Content < ordered[j].Content
	})

	context := RestartContext{Entries: make([]ContextEntry, 0, len(ordered))}
	remaining := maxBytes
	for _, entry := range ordered {
		if len(entry.Content) > remaining {
			context.Truncated = true
			continue
		}
		context.Entries = append(context.Entries, entry)
		remaining -= len(entry.Content)
	}
	context.Fingerprint = contextFingerprint(context.Entries)
	if expectedFingerprint != "" && expectedFingerprint != context.Fingerprint {
		return RestartContext{}, ErrStaleContextFingerprint
	}
	return context, nil
}

func contextFingerprint(entries []ContextEntry) string {
	hash := sha256.New()
	for _, entry := range entries {
		hash.Write([]byte(entry.ID))
		hash.Write([]byte{0})
		hash.Write([]byte(entry.Content))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}
