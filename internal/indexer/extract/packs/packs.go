// Package packs holds the embedded language packs: one directory per pack
// with a pack.yaml naming the recorded language, the tree-sitter grammar and
// the file extensions it handles. Query files for extraction live next to it.
// Go is not a pack; it is extracted with go/parser.
package packs

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed */pack.yaml
var files embed.FS

// Pack describes one language pack.
type Pack struct {
	ID         string   `yaml:"-"`          // directory name
	Language   string   `yaml:"language"`   // recorded as facts.File.Lang
	Grammar    string   `yaml:"grammar"`    // grammar name in the tree-sitter registry
	Extensions []string `yaml:"extensions"` // lower-case, with the leading dot
}

// Registry maps file extensions to packs.
type Registry struct {
	packs []*Pack
	byExt map[string]*Pack
}

// Load parses and validates the embedded packs.
func Load() (*Registry, error) {
	return load(files)
}

func load(fsys fs.FS) (*Registry, error) {
	paths, err := fs.Glob(fsys, "*/pack.yaml")
	if err != nil {
		return nil, fmt.Errorf("list packs: %w", err)
	}
	r := &Registry{byExt: map[string]*Pack{}}
	for _, p := range paths {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, fmt.Errorf("read pack %s: %w", p, err)
		}
		pk := &Pack{ID: path.Dir(p)}
		if err := yaml.Unmarshal(data, pk); err != nil {
			return nil, fmt.Errorf("parse pack %s: %w", p, err)
		}
		if pk.Language == "" || pk.Grammar == "" || len(pk.Extensions) == 0 {
			return nil, fmt.Errorf("pack %s: language, grammar and extensions are required", pk.ID)
		}
		for _, ext := range pk.Extensions {
			if len(ext) < 2 || ext[0] != '.' || ext != strings.ToLower(ext) {
				return nil, fmt.Errorf("pack %s: extension %q must be lower-case with a leading dot", pk.ID, ext)
			}
			if other, dup := r.byExt[ext]; dup {
				return nil, fmt.Errorf("extension %s is claimed by packs %s and %s", ext, other.ID, pk.ID)
			}
			r.byExt[ext] = pk
		}
		r.packs = append(r.packs, pk)
	}
	return r, nil
}

// Packs returns the packs in ID order.
func (r *Registry) Packs() []*Pack { return r.packs }

// ForPath returns the pack handling a file path, or nil.
func (r *Registry) ForPath(p string) *Pack {
	ext := path.Ext(p)
	if ext == "" {
		return nil
	}
	return r.byExt[strings.ToLower(ext)]
}

// Extensions returns every handled extension, sorted.
func (r *Registry) Extensions() []string {
	out := make([]string, 0, len(r.byExt))
	for ext := range r.byExt {
		out = append(out, ext)
	}
	slices.Sort(out)
	return out
}
