// Package modelid normalizes provider-specific model ID spellings so price
// and context-window lookups agree on which catalog entry a model maps to.
package modelid

import (
	"regexp"
	"strings"
)

var (
	// Bedrock model IDs carry a region/vendor prefix and a version suffix:
	// "us.anthropic.claude-sonnet-4-5-20250929-v1:0".
	vendorPrefix  = regexp.MustCompile(`^(?:[a-z]+\.)+`)
	bedrockSuffix = regexp.MustCompile(`-v\d+(?::\d+)?$`)
	// Release-date suffixes: "-20250929", "@20250929" (Vertex), "-2024-08-06".
	datedSuffix = regexp.MustCompile(`(?:[-@]\d{8}|-\d{4}-\d{2}-\d{2})$`)
)

// Candidates returns lookup keys for modelID, most specific first, so an
// exact entry (including a dated or provider-prefixed one) always wins over a
// normalized one. Keys are lowercase. It handles provider prefixes
// ("anthropic/claude-sonnet-4-5"), Bedrock and Vertex forms, trailing release
// dates, and OpenRouter's dotted versions ("claude-sonnet-4.5").
func Candidates(modelID string) []string {
	id := strings.ToLower(strings.TrimSpace(modelID))
	keys := []string{id}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		id = id[i+1:]
		keys = append(keys, id)
	}
	base := datedSuffix.ReplaceAllString(vendorPrefix.ReplaceAllString(bedrockSuffix.ReplaceAllString(id, ""), ""), "")
	keys = append(keys, base)
	if dashed := strings.ReplaceAll(base, ".", "-"); dashed != base {
		keys = append(keys, dashed)
	}
	return keys
}
