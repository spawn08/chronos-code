// Package golang extracts syntactic facts from one Go source file using
// go/parser only: no type-checking, no go toolchain, no other files.
package golang

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"strconv"
	"strings"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

// Lang is the language identifier recorded for Go files.
const Lang = "go"

const maxDocBytes = 1024

// Extract parses src and fills f.PkgName, f.Symbols, f.Imports and f.Calls.
// A syntax error is recorded in f.ParseErr; whatever parsed is still kept.
func Extract(f *facts.File, src []byte) {
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
			x.bodyCalls(s.Type, len(x.f.Symbols)-1)
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
		c := facts.Call{Caller: caller, Line: x.line(call.Lparen)}
		c.Col = x.fset.Position(call.Lparen).Column
		switch fn := fun.(type) {
		case *ast.Ident:
			c.Callee = fn.Name
		case *ast.SelectorExpr:
			c.Callee = fn.Sel.Name
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
		x.f.Calls = append(x.f.Calls, c)
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
