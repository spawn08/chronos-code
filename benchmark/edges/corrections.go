package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// Corrections of SCIP's gold answer (docs/chronos-indexer.md, "M7 precision
// targets"). The reported numbers use them; Result.Raw keeps the
// measurement against SCIP alone.
//
//   - Errata: reviewed references whose SCIP target cannot be the callee
//     (it takes another number of arguments, or needs a receiver the call
//     does not have). Each line of the errata file names the repository,
//     the reference and why; those references are not scored.
//   - Clang oracle (C and C++): every translation unit of
//     compile_commands.json is parsed by clang, and the declarations its
//     AST gives each call replace SCIP's definition (scip-clang can give a
//     call the symbol of another overload).
type corrections struct {
	errata map[siteKey]string
	oracle map[siteKey][]lineKey
}

// siteKey is a reference: file, 1-based line and name.
type siteKey struct {
	path string
	line int
	name string
}

func loadCorrections(name, root, errataPath, compdb, clang string) (*corrections, error) {
	if errataPath == "" && compdb == "" {
		return nil, nil
	}
	c := &corrections{errata: map[siteKey]string{}, oracle: map[siteKey][]lineKey{}}
	if errataPath != "" {
		if err := c.readErrata(name, errataPath); err != nil {
			return nil, err
		}
	}
	if compdb != "" {
		if _, err := exec.LookPath(clang); err != nil {
			fmt.Fprintf(os.Stderr, "edges: no %s, calls are not checked against clang\n", clang)
		} else if err := c.readOracle(root, compdb, clang); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// readErrata reads repository<TAB>file:line<TAB>name<TAB>reason lines;
// "#" starts a comment.
func (c *corrections) readErrata(repo, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("errata: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 4 || strings.TrimSpace(cols[3]) == "" {
			return fmt.Errorf("errata %s:%d: want repository, file:line, name and reason", path, n)
		}
		if cols[0] != repo {
			continue
		}
		file, lineNo, ok := strings.Cut(cols[1], ":")
		l, err := strconv.Atoi(lineNo)
		if !ok || err != nil {
			return fmt.Errorf("errata %s:%d: bad location %q", path, n, cols[1])
		}
		c.errata[siteKey{file, l, cols[2]}] = cols[3]
	}
	return sc.Err()
}

type compileCommand struct {
	Directory string   `json:"directory"`
	File      string   `json:"file"`
	Command   string   `json:"command"`
	Arguments []string `json:"arguments"`
}

// readOracle parses every translation unit with clang and records, for
// each call inside the repository, the lines of all declarations of its
// callee (a declaration in a header and its definition elsewhere are one
// group, merged across translation units).
func (c *corrections) readOracle(root, compdb, clang string) error {
	data, err := os.ReadFile(compdb)
	if err != nil {
		return fmt.Errorf("compile commands: %w", err)
	}
	var cmds []compileCommand
	if err := json.Unmarshal(data, &cmds); err != nil {
		return fmt.Errorf("compile commands: %w", err)
	}
	o := &oracleBuilder{root: root, parent: map[lineKey]lineKey{}, sites: map[siteKey][]lineKey{}}
	for _, cmd := range cmds {
		if err := o.parseTU(cmd, clang); err != nil {
			fmt.Fprintf(os.Stderr, "edges: clang %s: %v\n", cmd.File, err)
		}
	}
	for site, decls := range o.sites {
		var out []lineKey
		for _, d := range decls {
			root := o.find(d)
			for k := range o.parent {
				if o.find(k) == root && !slices.Contains(out, k) {
					out = append(out, k)
				}
			}
			if !slices.Contains(out, d) {
				out = append(out, d)
			}
		}
		slices.SortFunc(out, func(a, b lineKey) int {
			if a.path != b.path {
				return strings.Compare(a.path, b.path)
			}
			return a.line - b.line
		})
		c.oracle[site] = out
	}
	return nil
}

type oracleBuilder struct {
	root   string
	parent map[lineKey]lineKey // union-find over declaration locations
	sites  map[siteKey][]lineKey
}

func (o *oracleBuilder) find(k lineKey) lineKey {
	for {
		p, ok := o.parent[k]
		if !ok || p == k {
			return k
		}
		k = p
	}
}

func (o *oracleBuilder) union(a, b lineKey) {
	for _, k := range []lineKey{a, b} {
		if _, ok := o.parent[k]; !ok {
			o.parent[k] = k
		}
	}
	if ra, rb := o.find(a), o.find(b); ra != rb {
		o.parent[ra] = rb
	}
}

type astLoc struct {
	Offset    *int    `json:"offset"`
	File      string  `json:"file"`
	Line      int     `json:"line"`
	Spelling  *astLoc `json:"spellingLoc"`
	Expansion *astLoc `json:"expansionLoc"`
}

type astNode struct {
	ID    string  `json:"id"`
	Kind  string  `json:"kind"`
	Loc   *astLoc `json:"loc"`
	Range *struct {
		Begin *astLoc `json:"begin"`
		End   *astLoc `json:"end"`
	} `json:"range"`
	Name           string `json:"name"`
	PreviousDecl   string `json:"previousDecl"`
	ReferencedDecl *struct {
		ID   string `json:"id"`
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"referencedDecl"`
	ReferencedMemberDecl string     `json:"referencedMemberDecl"`
	Inner                []*astNode `json:"inner"`
}

// tu is the state of one translation unit's walk. clang's JSON omits a
// location's file and line when they equal the previous location's, so
// locations are resolved in output order.
type tu struct {
	o         *oracleBuilder
	dir       string
	file      string
	line      int
	decls     map[string]lineKey // declaration id -> location
	prev      map[string]string  // declaration id -> previous declaration id
	callSites []struct {
		site siteKey
		decl string
	}
}

func (o *oracleBuilder) parseTU(cmd compileCommand, clang string) error {
	args := cmd.Arguments
	if len(args) == 0 {
		args = splitCommand(cmd.Command)
	}
	if len(args) == 0 {
		return fmt.Errorf("empty command")
	}
	abs := func(p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(cmd.Directory, p)
		}
		return filepath.Clean(p)
	}
	source := abs(cmd.File)
	var flags []string
	for i := 1; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-c" || a == "-MD" || a == "-MMD":
		case a == "-o" || a == "-MF" || a == "-MT" || a == "-MQ":
			i++
		case abs(a) == source: // the source file, added last
		default:
			flags = append(flags, a)
		}
	}
	bin := clang
	if ext := strings.ToLower(filepath.Ext(cmd.File)); ext != ".c" && ext != ".m" && filepath.Base(clang) == "clang" {
		bin = clang + "++"
	}
	flags = append(flags, "-fsyntax-only", "-w", "-Xclang", "-ast-dump=json", source)
	c := exec.Command(bin, flags...)
	c.Dir = cmd.Directory
	var stdout bytes.Buffer
	c.Stdout = &stdout
	if err := c.Run(); err != nil && stdout.Len() == 0 {
		return err
	}
	var root astNode
	if err := json.Unmarshal(stdout.Bytes(), &root); err != nil {
		return err
	}
	t := &tu{o: o, dir: cmd.Directory, decls: map[string]lineKey{}, prev: map[string]string{}}
	t.walk(&root)
	for id, p := range t.prev {
		if a, ok := t.decls[id]; ok {
			if b, ok := t.decls[p]; ok {
				o.union(a, b)
			}
		}
	}
	for _, d := range t.decls {
		o.union(d, d)
	}
	for _, cs := range t.callSites {
		if d, ok := t.decls[cs.decl]; ok && !slices.Contains(o.sites[cs.site], d) {
			o.sites[cs.site] = append(o.sites[cs.site], d)
		}
	}
	return nil
}

// track resolves a location (the expansion location of a macro).
func (t *tu) track(l *astLoc) (lineKey, bool) {
	if l == nil {
		return lineKey{}, false
	}
	if l.Spelling != nil || l.Expansion != nil {
		t.track(l.Spelling)
		return t.track(l.Expansion)
	}
	if l.File != "" {
		t.file = l.File
	}
	if l.Line != 0 {
		t.line = l.Line
	}
	if l.Offset == nil || t.file == "" {
		return lineKey{}, false
	}
	return lineKey{t.rel(t.file), t.line}, true
}

// rel makes a clang path root-relative; "" outside the repository.
func (t *tu) rel(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.dir, p)
	}
	if c, err := filepath.EvalSymlinks(p); err == nil {
		p = c
	}
	r, err := filepath.Rel(t.o.root, p)
	if err != nil || strings.HasPrefix(r, "..") {
		return ""
	}
	return filepath.ToSlash(r)
}

var declKinds = map[string]bool{"FunctionDecl": true, "CXXMethodDecl": true, "CXXConstructorDecl": true, "CXXDestructorDecl": true}

func (t *tu) walk(n *astNode) {
	loc, hasLoc := t.track(n.Loc)
	var begin, end lineKey
	var hasBegin, hasEnd bool
	if n.Range != nil {
		begin, hasBegin = t.track(n.Range.Begin)
		end, hasEnd = t.track(n.Range.End)
	}
	switch {
	case declKinds[n.Kind] && hasLoc && loc.path != "":
		t.decls[n.ID] = loc
		if n.PreviousDecl != "" {
			t.prev[n.ID] = n.PreviousDecl
		}
	case n.Kind == "DeclRefExpr" && hasBegin && begin.path != "" && n.ReferencedDecl != nil && declKinds[n.ReferencedDecl.Kind]:
		t.addSite(begin, n.ReferencedDecl.Name, n.ReferencedDecl.ID)
	case n.Kind == "MemberExpr" && hasEnd && end.path != "" && n.ReferencedMemberDecl != "":
		t.addSite(end, n.Name, n.ReferencedMemberDecl)
	}
	for _, c := range n.Inner {
		t.walk(c)
	}
}

func (t *tu) addSite(at lineKey, name, decl string) {
	name = strings.TrimLeftFunc(name, func(r rune) bool { return r == '~' || unicode.IsSpace(r) })
	if name == "" {
		return
	}
	t.callSites = append(t.callSites, struct {
		site siteKey
		decl string
	}{siteKey{at.path, at.line, name}, decl})
}

// splitCommand splits a compile command line, honoring quotes and
// backslash escapes.
func splitCommand(s string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	in := false
	for i := 0; i < len(s); i++ {
		c := rune(s[i])
		switch {
		case c == '\\' && i+1 < len(s) && quote != '\'':
			i++
			cur.WriteByte(s[i])
			in = true
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote, in = c, true
		case quote == 0 && unicode.IsSpace(c):
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}
