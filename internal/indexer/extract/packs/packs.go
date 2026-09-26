// Package packs holds the embedded language packs: one directory per pack
// with a pack.yaml (recorded language, tree-sitter grammar, file extensions
// and the language's conventions) and a tags.scm query that captures
// definitions, imports, exports and references. Go is not a pack; it is
// extracted with go/parser.
//
// # Query captures
//
// The generic extractor (extract/generic) interprets these capture names:
//
//	@def.<kind>        a definition node; <kind> is a facts kind (func, method,
//	                   constructor, class, interface, trait, protocol, struct,
//	                   enum, enum_member, field, property, var, const,
//	                   type_alias, module, namespace, macro). A ".decl" suffix
//	                   (@def.func.decl) marks a declaration without a body.
//	@name              the definition's or reference's name
//	@receiver          explicit owner of a definition (C++ Foo::bar)
//	@doc               doc node (Python docstring); otherwise the comments
//	                   right before the definition are used
//	@body              where the signature ends; default: the "body" field
//	@scope             a container that is not a symbol (Rust impl, Swift
//	                   extension); @scope.name names the owner of its members
//	@import            one import (@include for #include/#import), with
//	                   @import.path, @import.alias, @import.name (+ alias),
//	                   @import.default and @import.wildcard
//	@export            an export statement, with @export.name (+ @export.alias),
//	                   @export.default, @export.all and @export.source
//	@ref.<kind>        a reference: call, type, extends, implements,
//	                   instantiate or decorator, with @name and an optional
//	                   @ref.qualifier (the receiver or scope expression)
//	@package           the declared package or namespace
//
// Captures starting with "_" are free for predicates.
package packs

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed */pack.yaml */tags.scm
var files embed.FS

// Pack describes one language pack.
type Pack struct {
	ID         string   `yaml:"-"`          // directory name
	Language   string   `yaml:"language"`   // recorded as facts.File.Lang
	Grammar    string   `yaml:"grammar"`    // grammar name in the tree-sitter registry
	Extensions []string `yaml:"extensions"` // lower-case, with the leading dot

	Visibility Visibility `yaml:"visibility"`
	// Modifiers maps words in a definition's header (the tokens before its
	// name, including decorators and annotations) to modifiers: static,
	// abstract, async, override, deprecated.
	Modifiers map[string]string `yaml:"modifiers"`
	// Wrappers are node types that wrap a definition without being one
	// (decorated_definition, export_statement). Doc comments precede the
	// wrapper, and its tokens count as the definition's header.
	Wrappers []string `yaml:"wrappers"`
	// DocSkip are node types allowed between a doc comment and the
	// definition (Rust attribute items).
	DocSkip []string `yaml:"doc_skip"`
	// MemberOf lists container kinds whose functions are methods. Default:
	// class, struct, interface, trait, protocol, enum.
	MemberOf []string `yaml:"member_of"`
	Imports  Imports  `yaml:"imports"`
	Tests    Tests    `yaml:"tests"`
	// QueryFrom names another pack whose tags.scm comes before this pack's
	// own (tsx adds JSX patterns to typescript's).
	QueryFrom string `yaml:"query_from"`

	// Query is the tags.scm source; empty means file-level indexing only.
	Query string `yaml:"-"`
}

// Visibility describes how a language decides a symbol's visibility. The
// first rule that applies wins: header keywords, the private name prefix,
// access labels, the export wrapper, the member default of the container
// kind, then Default.
type Visibility struct {
	Default string `yaml:"default"` // top-level declarations
	// Members maps a container kind to the default visibility of its
	// members; "*" applies to any other container.
	Members map[string]string `yaml:"members"`
	// Keywords maps header words to visibilities; the last one found wins
	// (Rust "pub(crate)" is pub then crate).
	Keywords map[string]string `yaml:"keywords"`
	// PrivatePrefix marks names starting with it as private (Python and
	// Dart "_"); Python dunder names (__x__) are exempt.
	PrivatePrefix string `yaml:"private_prefix"`
	// ExportWrapper is the node type that makes a declaration public
	// (export_statement); without it declarations get the defaults.
	ExportWrapper string `yaml:"export_wrapper"`
	// Labels are node types that set the visibility of the members after
	// them (C++ access_specifier, a Ruby "private" line); their text is
	// looked up in Keywords.
	Labels []string `yaml:"labels"`
}

// Imports describes the local name an import binds when it has no alias
// and lists no names.
type Imports struct {
	// Binds is "last" (default: Java a.b.C binds C), "first" (Python
	// import os.path binds os) or "none" (the import binds no name).
	Binds string `yaml:"binds"`
	// Separators split an import spec into segments; default ".".
	Separators string `yaml:"separators"`
}

// Tests describes the language's test conventions.
type Tests struct {
	Files   []string `yaml:"files"`   // base-name globs (test_*.py)
	Dirs    []string `yaml:"dirs"`    // directory names (tests)
	Symbols []string `yaml:"symbols"` // name globs of test functions in test files
	Markers []string `yaml:"markers"` // header words marking a test (Test, it)
}

// Visibility names accepted in pack.yaml.
var visibilityNames = []string{"public", "protected", "internal", "private", "package"}

// Modifier names accepted in pack.yaml.
var modifierNames = []string{"static", "abstract", "async", "override", "deprecated"}

// Registry maps file extensions to packs.
type Registry struct {
	packs []*Pack
	byExt map[string]*Pack
}

var (
	defaultOnce sync.Once
	defaultReg  *Registry
)

// Default returns the embedded packs, loaded once. The embedded packs are
// validated by tests, so a load error is a build defect and panics.
func Default() *Registry {
	defaultOnce.Do(func() {
		r, err := Load()
		if err != nil {
			panic(fmt.Sprintf("packs: embedded packs are invalid: %v", err))
		}
		defaultReg = r
	})
	return defaultReg
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
		if err := pk.validate(); err != nil {
			return nil, err
		}
		query, err := fs.ReadFile(fsys, path.Join(pk.ID, "tags.scm"))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("read pack %s query: %w", pk.ID, err)
		}
		pk.Query = string(query)
		if pk.QueryFrom != "" {
			base, err := fs.ReadFile(fsys, path.Join(pk.QueryFrom, "tags.scm"))
			if err != nil {
				return nil, fmt.Errorf("pack %s: query_from %s: %w", pk.ID, pk.QueryFrom, err)
			}
			pk.Query = string(base) + "\n" + pk.Query
		}
		for _, ext := range pk.Extensions {
			if other, dup := r.byExt[ext]; dup {
				return nil, fmt.Errorf("extension %s is claimed by packs %s and %s", ext, other.ID, pk.ID)
			}
			r.byExt[ext] = pk
		}
		r.packs = append(r.packs, pk)
	}
	return r, nil
}

func (pk *Pack) validate() error {
	if pk.Language == "" || pk.Grammar == "" || len(pk.Extensions) == 0 {
		return fmt.Errorf("pack %s: language, grammar and extensions are required", pk.ID)
	}
	for _, ext := range pk.Extensions {
		if len(ext) < 2 || ext[0] != '.' || ext != strings.ToLower(ext) {
			return fmt.Errorf("pack %s: extension %q must be lower-case with a leading dot", pk.ID, ext)
		}
	}
	v := pk.Visibility
	check := func(what, name string) error {
		if name != "" && !slices.Contains(visibilityNames, name) {
			return fmt.Errorf("pack %s: %s: unknown visibility %q", pk.ID, what, name)
		}
		return nil
	}
	if err := check("visibility.default", v.Default); err != nil {
		return err
	}
	for kind, name := range v.Members {
		if err := check("visibility.members."+kind, name); err != nil {
			return err
		}
	}
	for word, name := range v.Keywords {
		if err := check("visibility.keywords."+word, name); err != nil {
			return err
		}
	}
	for word, name := range pk.Modifiers {
		if !slices.Contains(modifierNames, name) {
			return fmt.Errorf("pack %s: modifiers.%s: unknown modifier %q", pk.ID, word, name)
		}
	}
	switch pk.Imports.Binds {
	case "", "last", "first", "none":
	default:
		return fmt.Errorf("pack %s: imports.binds: want last, first or none, got %q", pk.ID, pk.Imports.Binds)
	}
	for _, g := range append(slices.Clone(pk.Tests.Files), pk.Tests.Symbols...) {
		if _, err := path.Match(g, ""); err != nil {
			return fmt.Errorf("pack %s: bad test glob %q: %w", pk.ID, g, err)
		}
	}
	return nil
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
