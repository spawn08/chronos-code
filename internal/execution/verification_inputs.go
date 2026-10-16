package execution

import (
	"path/filepath"
	"strings"
)

// VerificationInputsOverlap includes Go package membership and module inputs.
// Exact file evidence still certifies only its named paths.
func VerificationInputsOverlap(check, write Event) bool {
	if eventScopesOverlap(check, write) {
		return true
	}
	if !strings.HasPrefix(check.CommandFamily, "go-") {
		return false
	}
	for _, changed := range write.Paths {
		for _, directory := range check.DirectInputDirs {
			if filepath.ToSlash(filepath.Dir(changed)) == directory {
				return true
			}
		}
		if changed == filepath.ToSlash(filepath.Join(check.CheckRoot, "go.mod")) || changed == filepath.ToSlash(filepath.Join(check.CheckRoot, "go.sum")) {
			return true
		}
		if filepath.Ext(changed) != ".go" {
			continue
		}
		for _, scope := range check.Scopes {
			if scope.Kind == ScopeExact && filepath.Ext(scope.Path) == ".go" && filepath.Dir(scope.Path) == filepath.Dir(changed) {
				return true
			}
		}
	}
	return false
}
