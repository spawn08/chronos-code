package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spawn08/chronos-code/internal/defaults"
	"github.com/spawn08/chronos-code/internal/security"
)

// init must never write a project policy that fails the "project may only
// narrow the embedded policy" rule, or startup stops.
func TestInitSecurityPolicyLoadsForEveryProjectType(t *testing.T) {
	floor, err := defaults.ReadFile("security.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, projType := range []ProjectType{ProjectGo, ProjectNode, ProjectPython, ProjectRust, ProjectJava, ProjectUnknown} {
		t.Run(string(projType), func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), ".chronos-code")
			if err := writeEmbeddedDefaults(dir); err != nil {
				t.Fatal(err)
			}
			if err := customizeForProject(dir, projType); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "security.yaml"))
			if err != nil {
				t.Fatal(err)
			}
			policy, err := security.ResolvePolicy(floor, security.Overlay{Source: "project", Data: data})
			if err != nil {
				t.Fatalf("init policy for %s fails to load: %v", projType, err)
			}
			if policy == nil {
				t.Fatal("nil policy")
			}
		})
	}
}
