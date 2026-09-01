package graph

import (
	"context"
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RenderCodeMap renders the indexed files, imports, and symbols for pkg as
// deterministic Markdown. Only paths contained by workspaceRoot become links.
func RenderCodeMap(ctx context.Context, store *Store, workspaceRoot, pkg string) (string, error) {
	files, err := store.FilesInPackage(ctx, pkg)
	if err != nil {
		return "", fmt.Errorf("render code map files: %w", err)
	}
	symbols, err := store.SymbolsInPackage(ctx, pkg)
	if err != nil {
		return "", fmt.Errorf("render code map symbols: %w", err)
	}
	imports, err := store.PackageImports(ctx, pkg)
	if err != nil {
		return "", fmt.Errorf("render code map imports: %w", err)
	}

	type renderedFile struct {
		key      string
		display  string
		link     string
		external bool
		symbols  []Symbol
	}

	byKey := make(map[string]*renderedFile, len(files))
	addFile := func(path string) *renderedFile {
		key, display, link, external := codeMapPath(workspaceRoot, path)
		if file := byKey[key]; file != nil {
			return file
		}
		file := &renderedFile{key: key, display: display, link: link, external: external}
		byKey[key] = file
		return file
	}
	for _, file := range files {
		addFile(file.Path)
	}
	for _, symbol := range symbols {
		file := addFile(symbol.File)
		file.symbols = append(file.symbols, symbol)
	}

	orderedFiles := make([]*renderedFile, 0, len(byKey))
	for _, file := range byKey {
		file.symbols = sortedUniqueSymbols(file.symbols)
		orderedFiles = append(orderedFiles, file)
	}
	sort.Slice(orderedFiles, func(i, j int) bool {
		if orderedFiles[i].display != orderedFiles[j].display {
			return orderedFiles[i].display < orderedFiles[j].display
		}
		return orderedFiles[i].key < orderedFiles[j].key
	})

	var out strings.Builder
	fmt.Fprintf(&out, "# Package %s\n\n## Imports\n", markdownCode(pkg))
	importList := sortedUniqueImports(imports)
	if len(importList) == 0 {
		out.WriteString("_None._\n")
	} else {
		for _, importPath := range importList {
			fmt.Fprintf(&out, "- %s\n", markdownCode(importPath))
		}
	}
	out.WriteString("\n## Files\n")
	if len(orderedFiles) == 0 {
		out.WriteString("_None._\n")
		return out.String(), nil
	}

	for i, file := range orderedFiles {
		if i > 0 {
			out.WriteByte('\n')
		}
		if file.external {
			fmt.Fprintf(&out, "### %s (outside workspace)\n", markdownCode(file.display))
		} else {
			fmt.Fprintf(&out, "### [%s](%s)\n", markdownCode(file.display), file.link)
		}
		if len(file.symbols) == 0 {
			out.WriteString("_No indexed symbols._\n")
			continue
		}
		for _, symbol := range file.symbols {
			signature := symbol.Signature
			if signature == "" {
				signature = strings.TrimSpace(string(symbol.Kind) + " " + symbol.Name)
			}
			fmt.Fprintf(&out, "- %s (%s, line %d)\n", markdownCode(signature), symbol.Kind, symbol.Line)
		}
	}
	return out.String(), nil
}

// RenderCodeMapIndex renders the deterministic list of indexed packages.
func RenderCodeMapIndex(ctx context.Context, store *Store) (string, error) {
	packages, err := store.Packages(ctx)
	if err != nil {
		return "", fmt.Errorf("render code map index: %w", err)
	}
	packages = sortedUniqueStrings(packages)

	var out strings.Builder
	out.WriteString("# Code Map\n\n## Packages\n")
	if len(packages) == 0 {
		out.WriteString("_No indexed packages._\n")
		return out.String(), nil
	}
	for _, pkg := range packages {
		fmt.Fprintf(&out, "- %s\n", markdownCode(pkg))
	}
	return out.String(), nil
}

func codeMapPath(workspaceRoot, path string) (key, display, link string, external bool) {
	cleanPath := filepath.Clean(path)
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "outside:" + cleanPath, filepath.Base(cleanPath), "", true
	}

	var rel string
	if filepath.IsAbs(cleanPath) {
		rel, err = filepath.Rel(filepath.Clean(root), cleanPath)
	} else {
		rel = cleanPath
	}
	if err != nil || rel == "." || !filepath.IsLocal(rel) {
		return "outside:" + cleanPath, filepath.Base(cleanPath), "", true
	}

	rel = filepath.ToSlash(filepath.Clean(rel))
	return "inside:" + rel, rel, (&url.URL{Path: rel}).EscapedPath(), false
}

func sortedUniqueImports(imports string) []string {
	if imports == "" {
		return nil
	}
	parts := strings.Split(imports, ",")
	trimmed := parts[:0]
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			trimmed = append(trimmed, part)
		}
	}
	return sortedUniqueStrings(trimmed)
}

func sortedUniqueStrings(values []string) []string {
	sort.Strings(values)
	out := values[:0]
	for _, value := range values {
		if len(out) == 0 || out[len(out)-1] != value {
			out = append(out, value)
		}
	}
	return out
}

func sortedUniqueSymbols(symbols []Symbol) []Symbol {
	sort.Slice(symbols, func(i, j int) bool {
		left, right := symbols[i], symbols[j]
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		if left.Signature != right.Signature {
			return left.Signature < right.Signature
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		return left.Name < right.Name
	})
	out := symbols[:0]
	for _, symbol := range symbols {
		if len(out) == 0 || symbolRenderKey(out[len(out)-1]) != symbolRenderKey(symbol) {
			out = append(out, symbol)
		}
	}
	return out
}

func symbolRenderKey(symbol Symbol) string {
	return strconv.Itoa(symbol.Line) + "\x00" + symbol.Signature + "\x00" + string(symbol.Kind) + "\x00" + symbol.Name
}

func markdownCode(value string) string {
	delimiter := "`"
	for strings.Contains(value, delimiter) {
		delimiter += "`"
	}
	if strings.HasPrefix(value, "`") || strings.HasSuffix(value, "`") {
		value = " " + value + " "
	}
	return delimiter + value + delimiter
}
