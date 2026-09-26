package graph

import (
	"context"
	"strings"
)

// memBackend is an in-memory Backend for testing rendering and tool
// plumbing with exact control over the data (duplicates, unsafe paths,
// insertion order). Methods a test does not need are left to the embedded
// nil interface and panic if called.
type memBackend struct {
	Backend
	packages []string          // in insertion order
	imports  map[string]string // package -> comma-joined imports
	files    []FileRecord
	symbols  []Symbol
}

func newMemBackend() *memBackend { return &memBackend{imports: map[string]string{}} }

func (m *memBackend) addPackage(name, imports string) {
	if _, ok := m.imports[name]; !ok {
		m.packages = append(m.packages, name)
	}
	m.imports[name] = imports
}

func (m *memBackend) addFile(path, pkg string) {
	m.files = append(m.files, FileRecord{Path: path, Package: pkg})
}

func (m *memBackend) addSymbol(s Symbol) {
	s.ID = int64(len(m.symbols) + 1)
	m.symbols = append(m.symbols, s)
}

func (m *memBackend) FindSymbols(_ context.Context, name, kind string) ([]Symbol, error) {
	var out []Symbol
	for _, s := range m.symbols {
		if s.Name == name && (kind == "" || string(s.Kind) == kind) {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memBackend) FindSymbolsFuzzy(_ context.Context, substr string) ([]Symbol, error) {
	var out []Symbol
	for _, s := range m.symbols {
		if strings.Contains(strings.ToLower(s.Name), strings.ToLower(substr)) {
			out = append(out, s)
		}
	}
	return out, nil
}

// Search matches the query case-insensitively in names and docs.
func (m *memBackend) Search(_ context.Context, query string, topK int) ([]SearchResult, error) {
	q := strings.ToLower(query)
	var out []SearchResult
	for _, s := range m.symbols {
		if strings.Contains(strings.ToLower(s.Name), q) || strings.Contains(strings.ToLower(s.Doc), q) {
			out = append(out, SearchResult{Symbol: s, Rank: float64(-len(out))})
		}
		if topK > 0 && len(out) == topK {
			break
		}
	}
	return out, nil
}

func (m *memBackend) SymbolsInPackage(_ context.Context, pkg string) ([]Symbol, error) {
	var out []Symbol
	for _, s := range m.symbols {
		if s.Package == pkg {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *memBackend) FilesInPackage(_ context.Context, pkg string) ([]FileRecord, error) {
	var out []FileRecord
	for _, f := range m.files {
		if f.Package == pkg {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *memBackend) PackageImports(_ context.Context, pkg string) (string, error) {
	return m.imports[pkg], nil
}

func (m *memBackend) Packages(context.Context) ([]string, error) {
	return append([]string(nil), m.packages...), nil
}
