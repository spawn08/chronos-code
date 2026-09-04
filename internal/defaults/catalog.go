package defaults

import (
	"fmt"
	"io/fs"
	"sort"

	"gopkg.in/yaml.v3"
)

// Activation states whether an embedded artifact participates in the default
// runtime or is shipped only for `chronos-code init` and user opt-in.
type Activation string

const (
	RuntimeActive Activation = "runtime-active"
	ExportOnly    Activation = "export-only"
)

// Artifact records an embedded file's activation status. Export-only entries
// must state why they are not an enabled runtime capability.
type Artifact struct {
	Path       string
	Activation Activation
	Rationale  string
}

var catalog = map[string]Artifact{
	"config.yaml":                {Path: "config.yaml", Activation: RuntimeActive, Rationale: "loaded as the zero-config base"},
	"security.yaml":              {Path: "security.yaml", Activation: RuntimeActive, Rationale: "loaded into every agent security hook"},
	"routing.yaml":               {Path: "routing.yaml", Activation: RuntimeActive, Rationale: "loaded by the message router"},
	"learning.yaml":              {Path: "learning.yaml", Activation: RuntimeActive, Rationale: "loaded by the learn command"},
	"guardrails/default.yaml":    {Path: "guardrails/default.yaml", Activation: RuntimeActive, Rationale: "loaded into every agent guardrail engine"},
	"skills/default-skills.yaml": {Path: "skills/default-skills.yaml", Activation: RuntimeActive, Rationale: "loaded into the bundled skill catalog"},
	"agents/architect.yaml":      {Path: "agents/architect.yaml", Activation: RuntimeActive, Rationale: "supplies the default architect prompt"},
	"agents/chronos-code.yaml":   {Path: "agents/chronos-code.yaml", Activation: RuntimeActive, Rationale: "supplies the primary Chronos Code prompt"},
	"agents/coder.yaml":          {Path: "agents/coder.yaml", Activation: RuntimeActive, Rationale: "supplies the default coder prompt"},
	"agents/debugger.yaml":       {Path: "agents/debugger.yaml", Activation: RuntimeActive, Rationale: "supplies the default debugger prompt"},
	"agents/explainer.yaml":      {Path: "agents/explainer.yaml", Activation: RuntimeActive, Rationale: "supplies the default explainer prompt"},
	"agents/planner.yaml":        {Path: "agents/planner.yaml", Activation: RuntimeActive, Rationale: "supplies the default planner prompt"},
	"agents/researcher.yaml":     {Path: "agents/researcher.yaml", Activation: RuntimeActive, Rationale: "supplies the default researcher prompt"},
	"agents/reviewer.yaml":       {Path: "agents/reviewer.yaml", Activation: RuntimeActive, Rationale: "supplies the default reviewer prompt"},
	"agents/ppd-planner.yaml":    {Path: "agents/ppd-planner.yaml", Activation: RuntimeActive, Rationale: "supplies the default PPD specialist"},
	"mcp-servers.yaml":           {Path: "mcp-servers.yaml", Activation: ExportOnly, Rationale: "MCP clients are discovered from project configuration, not this manifest"},
	"tools.yaml":                 {Path: "tools.yaml", Activation: ExportOnly, Rationale: "documents tool shapes; runtime tools are registered in Go"},
	"teams/code-review.yaml":     {Path: "teams/code-review.yaml", Activation: ExportOnly, Rationale: "teams require explicit configuration before they are built"},
	"teams/debug.yaml":           {Path: "teams/debug.yaml", Activation: ExportOnly, Rationale: "teams require explicit configuration before they are built"},
}

// Catalog returns the complete, sorted inventory of embedded artifacts.
func Catalog() ([]Artifact, error) {
	artifacts := make([]Artifact, 0, len(catalog))
	seen := make(map[string]struct{}, len(catalog))
	err := fs.WalkDir(FS, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		artifact, ok := catalog[path]
		if !ok {
			return fmt.Errorf("embedded artifact %q has no catalog entry", path)
		}
		seen[path] = struct{}{}
		artifacts = append(artifacts, artifact)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk embedded artifacts: %w", err)
	}
	for path := range catalog {
		if _, ok := seen[path]; !ok {
			return nil, fmt.Errorf("catalog artifact %q is not embedded", path)
		}
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return artifacts, nil
}

// ValidateCatalog verifies catalog coverage, valid YAML, and unambiguous
// activation labels. It is intentionally diagnostic-only: it never registers
// an export-only artifact as a runtime capability.
func ValidateCatalog() error {
	artifacts, err := Catalog()
	if err != nil {
		return err
	}
	for _, artifact := range artifacts {
		if artifact.Activation != RuntimeActive && artifact.Activation != ExportOnly {
			return fmt.Errorf("catalog artifact %q has invalid activation %q", artifact.Path, artifact.Activation)
		}
		if artifact.Rationale == "" {
			return fmt.Errorf("catalog artifact %q has no rationale", artifact.Path)
		}
		data, err := ReadFile(artifact.Path)
		if err != nil {
			return fmt.Errorf("read catalog artifact %q: %w", artifact.Path, err)
		}
		var document yaml.Node
		if err := yaml.Unmarshal(data, &document); err != nil {
			return fmt.Errorf("parse catalog artifact %q: %w", artifact.Path, err)
		}
	}
	return nil
}
