// Package golang extracts syntactic facts from one Go source file using
// go/parser only: no type-checking, no go toolchain, no other files.
package golang

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

// Lang is the language identifier recorded for Go files.
const Lang = "go"

const maxDocBytes = 1024

// generatedRE is the Go convention for generated files (go help generate).
var generatedRE = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// Extract parses src and fills f.PkgName, f.Symbols, f.Imports, f.Refs and
// the Test and Generated flags. A syntax error is recorded in f.ParseErr;
// whatever parsed is still kept.
func Extract(f *facts.File, src []byte) {
	f.Test = strings.HasSuffix(f.Path, "_test.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, f.Path, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		f.ParseErr = err.Error()
	}
	if file == nil {
		return
	}
	x := extractor{fset: fset, f: f, aliases: map[string]string{}}
	x.run(file)
}

type extractor struct {
	fset    *token.FileSet
	f       *facts.File
	aliases map[string]string // local package name -> import path
	buf     bytes.Buffer
}

func (x *extractor) line(p token.Pos) int { return x.fset.Position(p).Line }

func (x *extractor) run(file *ast.File) {
	if file.Name != nil {
		x.f.PkgName = file.Name.Name
	}
	x.f.Generated = isGenerated(file)
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		imp := facts.Import{Path: path, Line: x.line(spec.Pos())}
		local := defaultImportName(path)
		if spec.Name != nil {
			imp.Name = spec.Name.Name
			local = spec.Name.Name
			if local == "." {
				imp.Kind = facts.ImportWildcard
			}
		}
		x.f.Imports = append(x.f.Imports, imp)
		if local != "_" && local != "." {
			x.aliases[local] = path
		}
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			idx := x.funcDecl(d)
			if d.Body != nil {
				x.calls(d.Body, idx)
			}
		case *ast.GenDecl:
			x.genDecl(d)
		}
	}
	for i := range x.f.Symbols {
		setVisibility(&x.f.Symbols[i])
	}
}

// isGenerated reports a "Code generated … DO NOT EDIT." line comment before
// the package clause.
func isGenerated(file *ast.File) bool {
	for _, g := range file.Comments {
		if g.Pos() >= file.Package {
			return false
		}
		for _, c := range g.List {
			if generatedRE.MatchString(c.Text) {
				return true
			}
		}
	}
	return false
}

// setVisibility derives Visibility from Exported and marks deprecated
// declarations (a "Deprecated: " paragraph in the doc comment). Embeds are
// structure, not declarations, and keep VisUnknown.
func setVisibility(s *facts.Symbol) {
	if s.Kind == facts.KindEmbed {
		return
	}
	s.Visibility = facts.VisPackage
	if s.Exported {
		s.Visibility = facts.VisPublic
	}
	if strings.HasPrefix(s.Doc, "Deprecated: ") || strings.Contains(s.Doc, "\n\nDeprecated: ") {
		s.Modifiers |= facts.ModDeprecated
	}
}

// isTestFunc reports Go test, benchmark, example and fuzz functions.
func isTestFunc(name string) bool {
	for _, p := range []string{"Test", "Benchmark", "Example", "Fuzz"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// defaultImportName guesses the package name from an import path: the last
// element, ignoring a major-version suffix and gopkg.in's .vN.
func defaultImportName(path string) string {
	elems := strings.Split(path, "/")
	name := elems[len(elems)-1]
	if len(elems) > 1 && len(name) > 1 && name[0] == 'v' && isDigits(name[1:]) {
		name = elems[len(elems)-2]
	}
	if i := strings.Index(name, ".v"); i > 0 && isDigits(name[i+2:]) {
		name = name[:i]
	}
	return strings.ReplaceAll(name, "-", "_")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

func (x *extractor) funcDecl(d *ast.FuncDecl) int {
	sym := facts.Symbol{
		Name:     d.Name.Name,
		Kind:     facts.KindFunc,
		Line:     x.line(d.Pos()),
		EndLine:  x.line(d.End()),
		Doc:      docText(d.Doc),
		Exported: d.Name.IsExported(),
	}
	if d.Recv != nil && len(d.Recv.List) > 0 {
		sym.Kind = facts.KindMethod
		sym.Receiver = x.render(d.Recv.List[0].Type)
	} else if x.f.Test && isTestFunc(d.Name.Name) {
		sym.Modifiers |= facts.ModTest
	}
	if d.Body == nil {
		sym.Modifiers |= facts.ModDecl // implemented in assembly or linked
	}
	header := *d
	header.Doc, header.Body = nil, nil
	sym.Signature = x.render(&header)
	x.f.Symbols = append(x.f.Symbols, sym)
	return len(x.f.Symbols) - 1
}

func (x *extractor) genDecl(d *ast.GenDecl) {
	for _, spec := range d.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			kind, sig := facts.KindType, "type "+s.Name.Name
			switch s.Type.(type) {
			case *ast.StructType:
				kind, sig = facts.KindStruct, sig+" struct"
			case *ast.InterfaceType:
				kind, sig = facts.KindInterface, sig+" interface"
			default:
				if s.Assign.IsValid() {
					sig += " ="
				}
				sig += " " + x.render(s.Type)
			}
			doc := s.Doc
			if doc == nil && len(d.Specs) == 1 {
				doc = d.Doc
			}
			x.f.Symbols = append(x.f.Symbols, facts.Symbol{
				Name: s.Name.Name, Kind: kind, Signature: sig, Doc: docText(doc),
				Line: x.line(s.Pos()), EndLine: x.line(s.End()), Exported: s.Name.IsExported(),
			})
			idx := len(x.f.Symbols) - 1
			switch t := s.Type.(type) {
			case *ast.InterfaceType:
				x.interfaceMembers(s.Name.Name, t, idx)
			case *ast.StructType:
				x.structEmbeds(t, idx)
			}
			x.bodyCalls(s.Type, idx)
		case *ast.ValueSpec:
			kind := facts.KindVar
			if d.Tok == token.CONST {
				kind = facts.KindConst
			}
			typ := ""
			if s.Type != nil {
				typ = " " + x.render(s.Type)
			}
			doc := s.Doc
			if doc == nil && len(d.Specs) == 1 {
				doc = d.Doc
			}
			first := len(x.f.Symbols)
			for _, name := range s.Names {
				if name.Name == "_" {
					continue
				}
				x.f.Symbols = append(x.f.Symbols, facts.Symbol{
					Name: name.Name, Kind: kind, Signature: d.Tok.String() + " " + name.Name + typ,
					Doc: docText(doc), Line: x.line(name.Pos()), EndLine: x.line(s.End()), Exported: name.IsExported(),
				})
			}
			caller := facts.NoCaller
			if len(x.f.Symbols) > first {
				caller = first
			}
			for _, v := range s.Values {
				x.calls(v, caller)
			}
		}
	}
}

// interfaceMembers records an interface's method specs (as methods whose
// receiver is the interface) and its embedded types. Type-set terms of
// constraint interfaces (~int | string) are not recorded.
func (x *extractor) interfaceMembers(iface string, t *ast.InterfaceType, parent int) {
	if t.Methods == nil {
		return
	}
	for _, field := range t.Methods.List {
		if len(field.Names) == 0 {
			x.embed(field.Type, parent)
			continue
		}
		ft, ok := field.Type.(*ast.FuncType)
		if !ok {
			continue
		}
		for _, name := range field.Names {
			x.f.Symbols = append(x.f.Symbols, facts.Symbol{
				Name: name.Name, Kind: facts.KindMethod, Receiver: iface,
				Signature: name.Name + strings.TrimPrefix(x.render(ft), "func"),
				Doc:       docText(field.Doc), Line: x.line(name.Pos()), EndLine: x.line(field.End()),
				Exported: name.IsExported(), Container: parent + 1, Modifiers: facts.ModDecl,
			})
		}
	}
}

// structEmbeds records a struct's embedded fields, whose methods are promoted.
func (x *extractor) structEmbeds(t *ast.StructType, parent int) {
	if t.Fields == nil {
		return
	}
	for _, field := range t.Fields.List {
		if len(field.Names) == 0 {
			x.embed(field.Type, parent)
		}
	}
}

func (x *extractor) embed(e ast.Expr, parent int) {
	base := e
	if star, ok := base.(*ast.StarExpr); ok {
		base = star.X
	}
	name := ""
	switch v := unwrapIndex(base).(type) {
	case *ast.Ident:
		name = v.Name
	case *ast.SelectorExpr:
		name = v.Sel.Name
	default:
		return
	}
	x.f.Symbols = append(x.f.Symbols, facts.Symbol{
		Name: name, Kind: facts.KindEmbed, Signature: x.render(e),
		Line: x.line(e.Pos()), EndLine: x.line(e.End()), Container: parent + 1,
	})
}

// bodyCalls records calls inside type expressions (e.g. func literals in
// struct tags are impossible, but array length expressions may call).
func (x *extractor) bodyCalls(n ast.Node, caller int) {
	if n != nil {
		x.calls(n, caller)
	}
}

func (x *extractor) calls(root ast.Node, caller int) {
	ast.Inspect(root, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := unwrapIndex(call.Fun)
		c := facts.Ref{Kind: facts.RefCall, Enclosing: caller, Line: x.line(call.Lparen)}
		c.Col = x.fset.Position(call.Lparen).Column
		switch fn := fun.(type) {
		case *ast.Ident:
			c.Name = fn.Name
		case *ast.SelectorExpr:
			c.Name = fn.Sel.Name
			if id, ok := fn.X.(*ast.Ident); ok {
				if path, ok := x.aliases[id.Name]; ok {
					c.QualKind, c.Qualifier = facts.QualPackage, path
					break
				}
			}
			c.QualKind, c.Qualifier = facts.QualExpr, x.qualifierText(fn.X)
		default:
			return true // calls of func values, conversions of complex types, etc.
		}
		x.f.Refs = append(x.f.Refs, c)
		return true
	})
}

func unwrapIndex(e ast.Expr) ast.Expr {
	for {
		switch v := e.(type) {
		case *ast.IndexExpr:
			e = v.X
		case *ast.IndexListExpr:
			e = v.X
		case *ast.ParenExpr:
			e = v.X
		default:
			return e
		}
	}
}

// qualifierText renders short receiver expressions (x, x.y, x.y()) and
// abbreviates anything longer to keep records small.
func (x *extractor) qualifierText(e ast.Expr) string {
	s := x.render(e)
	if len(s) > 64 {
		s = s[:61] + "..."
	}
	return s
}

func (x *extractor) render(n ast.Node) string {
	x.buf.Reset()
	if err := (&printer.Config{Mode: printer.RawFormat}).Fprint(&x.buf, x.fset, n); err != nil {
		return ""
	}
	return collapseSpace(x.buf.String())
}

func collapseSpace(s string) string {
	if !strings.ContainsAny(s, "\n\t") && !strings.Contains(s, "  ") {
		return s
	}
	return strings.Join(strings.Fields(s), " ")
}

func docText(g *ast.CommentGroup) string {
	if g == nil {
		return ""
	}
	s := strings.TrimSpace(g.Text())
	if len(s) > maxDocBytes {
		s = s[:maxDocBytes]
	}
	return s
}
