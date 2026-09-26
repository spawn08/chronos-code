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

	"github.com/spawn08/chronos-code/indexer/facts"
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
	fset       *token.FileSet
	f          *facts.File
	aliases    map[string]string // local package name -> import path
	typeParams map[string]bool   // type parameters of the declaration being walked
	buf        bytes.Buffer
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
			x.typeParams = fieldNames(d.Type.TypeParams)
			x.signature(d, idx)
			if d.Body != nil {
				x.calls(d.Body, idx)
			}
			x.typeParams = nil
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
			x.typeParams = fieldNames(s.TypeParams)
			switch t := s.Type.(type) {
			case *ast.InterfaceType:
				x.interfaceMembers(s.Name.Name, t, idx)
			case *ast.StructType:
				x.structEmbeds(t, idx)
				x.structFields(t, idx)
			default:
				x.typeRefs(s.Type, idx)
			}
			x.bodyCalls(s.Type, idx)
			x.typeParams = nil
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
			if s.Type != nil {
				x.typeRefs(s.Type, caller)
			}
			x.valueHints(s, facts.NoCaller)
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
			x.typeRefs(ft, len(x.f.Symbols)-1)
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
	x.typeRef(base, parent, facts.RefExtends)
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
		switch v := n.(type) {
		case *ast.CompositeLit:
			if v.Type != nil {
				x.typeRef(v.Type, caller, facts.RefInstantiate)
			}
			return true
		case *ast.TypeAssertExpr:
			if v.Type != nil {
				x.typeRefs(v.Type, caller)
			}
			return true
		case *ast.FuncLit:
			x.typeRefs(v.Type, caller)
			return true
		case *ast.AssignStmt:
			if v.Tok == token.DEFINE {
				x.assignHints(v, caller)
			}
			return true
		case *ast.DeclStmt:
			if g, ok := v.Decl.(*ast.GenDecl); ok && g.Tok == token.VAR {
				for _, spec := range g.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						if vs.Type != nil {
							x.typeRefs(vs.Type, caller)
						}
						x.valueHints(vs, caller)
					}
				}
			}
			return true
		}
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

// predeclaredTypes are Go's builtin types; naming one is not a reference
// into the workspace.
var predeclaredTypes = map[string]bool{
	"any": true, "bool": true, "byte": true, "comparable": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true, "uint": true, "uint8": true,
	"uint16": true, "uint32": true, "uint64": true, "uintptr": true,
}

func fieldNames(fl *ast.FieldList) map[string]bool {
	if fl == nil {
		return nil
	}
	m := map[string]bool{}
	for _, f := range fl.List {
		for _, n := range f.Names {
			m[n.Name] = true
		}
	}
	return m
}

// signature records the types a function's parameters and results name,
// and binding hints for its receiver and parameters (func (r *Repo) and
// func f(s store.Store) let r.M() and s.M() resolve by type).
func (x *extractor) signature(d *ast.FuncDecl, idx int) {
	if d.Recv != nil {
		for _, field := range d.Recv.List {
			for _, n := range field.Names {
				if t := facts.BaseType(x.render(field.Type)); t != "" && n.Name != "_" {
					x.hint(idx, n.Name, t, n.Pos())
				}
			}
		}
	}
	x.typeRefs(d.Type, idx)
	if d.Type.Params != nil {
		for _, field := range d.Type.Params.List {
			t := hintType(field.Type)
			for _, n := range field.Names {
				if t != "" && n.Name != "_" {
					x.hint(idx, n.Name, t, n.Pos())
				}
			}
		}
	}
}

// structFields records field types and a hint per named field, scoped to
// the struct: s.repo.Save() resolves through the type of repo.
func (x *extractor) structFields(t *ast.StructType, idx int) {
	if t.Fields == nil {
		return
	}
	for _, field := range t.Fields.List {
		if len(field.Names) == 0 {
			continue // embeds are recorded by structEmbeds
		}
		x.typeRefs(field.Type, idx)
		if ht := hintType(field.Type); ht != "" {
			for _, n := range field.Names {
				x.hint(idx, n.Name, ht, n.Pos())
			}
		}
	}
}

// valueHints records var x T and var x = T{} / f() as hints.
func (x *extractor) valueHints(s *ast.ValueSpec, scope int) {
	for i, n := range s.Names {
		if n.Name == "_" {
			continue
		}
		t := ""
		if s.Type != nil {
			t = hintType(s.Type)
		} else if i < len(s.Values) && len(s.Values) == len(s.Names) {
			t = x.valueType(s.Values[i])
		}
		if t != "" {
			x.hint(scope, n.Name, t, n.Pos())
		}
	}
}

// assignHints records x := T{}, x := &T{}, x := new(T), x := v.(T) and
// x := f() (as "f()", the result type of f) as hints.
func (x *extractor) assignHints(a *ast.AssignStmt, scope int) {
	for i, lhs := range a.Lhs {
		id, ok := lhs.(*ast.Ident)
		if !ok || id.Name == "_" {
			continue
		}
		var t string
		switch {
		case len(a.Rhs) == len(a.Lhs):
			t = x.valueType(a.Rhs[i])
		case i == 0 && len(a.Rhs) == 1:
			if call, ok := a.Rhs[0].(*ast.CallExpr); ok {
				t = x.valueType(call) // a, err := f()
			}
		}
		if t != "" {
			x.hint(scope, id.Name, t, id.Pos())
		}
	}
}

// valueType returns the type an expression evidently has, or "f()" for a
// call whose result type the resolver can look up.
func (x *extractor) valueType(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.CompositeLit:
		return hintType(v.Type)
	case *ast.UnaryExpr:
		if v.Op == token.AND {
			if lit, ok := v.X.(*ast.CompositeLit); ok {
				return hintType(lit.Type)
			}
		}
	case *ast.TypeAssertExpr:
		return hintType(v.Type)
	case *ast.CallExpr:
		if id, ok := v.Fun.(*ast.Ident); ok && id.Name == "new" && len(v.Args) == 1 {
			return hintType(v.Args[0])
		}
		switch fn := unwrapIndex(v.Fun).(type) {
		case *ast.Ident:
			return fn.Name + "()"
		case *ast.SelectorExpr:
			if id, ok := fn.X.(*ast.Ident); ok {
				return id.Name + "." + fn.Sel.Name + "()"
			}
		}
	}
	return ""
}

// hintType renders a named type (T, *T, pkg.T, T[int]) without pointer or
// type arguments; other types (slices, maps, funcs) give "".
func hintType(e ast.Expr) string {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	switch v := unwrapIndex(e).(type) {
	case *ast.Ident:
		if !predeclaredTypes[v.Name] {
			return v.Name
		}
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name + "." + v.Sel.Name
		}
	}
	return ""
}

func (x *extractor) hint(scope int, name, typ string, pos token.Pos) {
	x.f.Hints = append(x.f.Hints, facts.BindingHint{Scope: scope, Name: name, Type: typ, Line: x.line(pos)})
}

// typeRefs records a type use for every named type in a type expression,
// walking type positions only (parameter names and array lengths are not
// types).
func (x *extractor) typeRefs(e ast.Node, enclosing int) {
	switch v := e.(type) {
	case nil:
	case *ast.Ident, *ast.SelectorExpr, *ast.IndexExpr, *ast.IndexListExpr:
		x.typeRef(v.(ast.Expr), enclosing, facts.RefTypeUse)
	case *ast.StarExpr:
		x.typeRefs(v.X, enclosing)
	case *ast.ParenExpr:
		x.typeRefs(v.X, enclosing)
	case *ast.ArrayType:
		x.typeRefs(v.Elt, enclosing)
	case *ast.Ellipsis:
		x.typeRefs(v.Elt, enclosing)
	case *ast.MapType:
		x.typeRefs(v.Key, enclosing)
		x.typeRefs(v.Value, enclosing)
	case *ast.ChanType:
		x.typeRefs(v.Value, enclosing)
	case *ast.UnaryExpr: // ~T in a constraint
		x.typeRefs(v.X, enclosing)
	case *ast.BinaryExpr: // A | B in a constraint
		x.typeRefs(v.X, enclosing)
		x.typeRefs(v.Y, enclosing)
	case *ast.FuncType:
		x.fieldTypes(v.TypeParams, enclosing)
		x.fieldTypes(v.Params, enclosing)
		x.fieldTypes(v.Results, enclosing)
	case *ast.StructType:
		x.fieldTypes(v.Fields, enclosing)
	case *ast.InterfaceType:
		x.fieldTypes(v.Methods, enclosing)
	}
}

func (x *extractor) fieldTypes(fl *ast.FieldList, enclosing int) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		x.typeRefs(f.Type, enclosing)
	}
}

// typeRef records one reference of kind to the named type e (T, *T,
// pkg.T, T[int]); other expressions are ignored.
func (x *extractor) typeRef(e ast.Expr, enclosing int, kind uint8) {
	if star, ok := e.(*ast.StarExpr); ok {
		e = star.X
	}
	if idx, ok := e.(*ast.IndexExpr); ok {
		x.typeRefs(idx.Index, enclosing)
	} else if idx, ok := e.(*ast.IndexListExpr); ok {
		for _, ix := range idx.Indices {
			x.typeRefs(ix, enclosing)
		}
	}
	r := facts.Ref{Kind: kind, Enclosing: enclosing}
	switch v := unwrapIndex(e).(type) {
	case *ast.Ident:
		if predeclaredTypes[v.Name] || x.typeParams[v.Name] || v.Name == "_" {
			return
		}
		r.Name = v.Name
		r.Line, r.Col = x.line(v.Pos()), x.fset.Position(v.Pos()).Column
	case *ast.SelectorExpr:
		id, ok := v.X.(*ast.Ident)
		if !ok {
			return
		}
		r.Name = v.Sel.Name
		r.Line, r.Col = x.line(v.Sel.Pos()), x.fset.Position(v.Sel.Pos()).Column
		if path, ok := x.aliases[id.Name]; ok {
			r.QualKind, r.Qualifier = facts.QualPackage, path
		} else {
			r.QualKind, r.Qualifier = facts.QualExpr, id.Name
		}
	default:
		return
	}
	x.f.Refs = append(x.f.Refs, r)
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
