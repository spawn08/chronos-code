package scip

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/cespare/xxhash/v2"

	"github.com/spawn08/chronos-code/indexer/precise"
)

// Version changes whenever imported facts change shape or meaning; stores
// written by another version are discarded.
const Version = "scip-1"

// Source is one SCIP index to import.
type Source struct {
	Index string // absolute path of the .scip file
	// Dir is the root-relative directory the index's document paths are
	// relative to, used when the index's project root is not inside the
	// workspace (an index built elsewhere, such as in CI). "" is the root.
	Dir string
	// Observed, for an index written by a run chronos watched, holds the
	// hash of every file that was the same before and after the run: a
	// document is imported only for a file listed with its current hash.
	// nil means the index came from elsewhere and each document is
	// checked against the file (see Import).
	Observed map[string]uint64
}

// Options configures Import.
type Options struct {
	Root string // absolute workspace root
	// Indexed returns the indexed hash of a root-relative file; documents
	// of files it does not know are skipped.
	Indexed func(path string) (uint64, bool)
	// Skip leaves out documents of files another tier covers (Go).
	Skip func(path string) bool
}

// Result is one import's outcome.
type Result struct {
	Dirs     []*precise.Dir    // facts by directory, sorted by path
	Docs     int               // documents imported
	Sites    int               // reference sites recorded
	Skipped  int               // documents of files that are not indexed, or skipped
	Rejected map[string]string // root-relative file -> why its document was not imported
}

// How a document is checked against the file it describes. SCIP records
// no content hash, so, in order of preference: the document's own text
// (rarely embedded), the hashes a watched run observed, or, for an index
// built elsewhere, the file's modification time (not after the index's)
// plus its occurrences of calls and types. A document is rejected when a
// definition's range spells another identifier, or any range splits an
// identifier, starts or ends in blank space or runs past its line (what a
// shifted range covers), or more than maxSoft references spell another
// whole identifier (aliases do, a few per file). Ranges covering an
// expression (Kotlin function types) and references to constructors and
// operators (spelled by syntax) are not checked. This is inferred, not
// exact; only sites spelling their symbol are imported either way.
func maxSoft(checked int) int { return max(3, checked/10) }

// Import reads sources and returns hash-gated facts for every document
// that describes the current, indexed content of its file. A document is
// recorded with the file's hash, so queries use its facts only while the
// file is unchanged. Nothing is written.
func Import(ctx context.Context, opts Options, sources []Source) (*Result, error) {
	im := &importer{
		opts: opts, res: &Result{Rejected: map[string]string{}},
		defs: map[string][]def{}, pkgs: map[string]bool{}, syms: map[string]symInfo{},
		files: map[string]*docFacts{},
	}
	var errs []error
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := im.read(ctx, src); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	im.build()
	return im.res, errors.Join(errs...)
}

type def struct {
	path string
	line int // 1-based
	text string
}

type symInfo struct {
	ok        bool
	local     bool
	callable  bool
	typ       bool
	macro     bool
	implicit  bool // references may be spelled by syntax (constructors, operators)
	pkg       string
	spellings []string
}

// site is one reference occurrence of an imported document.
type site struct {
	line, col int32 // 1-based line, 1-based byte column
	symbol    string
	text      string
}

type docFacts struct {
	path  string
	hash  uint64
	sites []site
}

type importer struct {
	opts  Options
	res   *Result
	defs  map[string][]def     // symbol -> definitions in imported documents
	pkgs  map[string]bool      // packages that define symbols: the workspace's
	syms  map[string]symInfo   // parsed symbols
	files map[string]*docFacts // imported documents by path
}

func (im *importer) symbol(s string) symInfo {
	if info, ok := im.syms[s]; ok {
		return info
	}
	sym, ok := ParseSymbol(s)
	info := symInfo{ok: ok}
	if ok {
		info.local = sym.Local
		if !sym.Local {
			info.callable, info.typ = sym.Callable(), sym.IsType()
			info.macro = sym.Last().Suffix == SuffixMacro
			info.implicit = sym.Implicit()
			info.pkg = sym.Scheme + " " + sym.Package
			info.spellings = sym.Spellings()
		}
	}
	im.syms[s] = info
	return info
}

func (im *importer) read(ctx context.Context, src Source) error {
	st, err := os.Stat(src.Index)
	if err != nil {
		return fmt.Errorf("scip: %w", err)
	}
	prefix := path.Clean(filepath.ToSlash(src.Dir))
	if prefix == "" {
		prefix = "."
	}
	return ReadFile(src.Index, func(m Metadata) {
		if p, ok := projectDir(im.opts.Root, m.ProjectRoot); ok {
			prefix = p
		}
	}, func(d Document) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		im.document(src, st.ModTime(), prefix, d)
		return nil
	})
}

// projectDir returns the root-relative directory of a file:// project
// root inside root.
func projectDir(root, uri string) (string, bool) {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return "", false
	}
	rel, err := filepath.Rel(root, filepath.FromSlash(u.Path))
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

func (im *importer) document(src Source, indexTime time.Time, prefix string, d Document) {
	p := path.Clean(path.Join(prefix, filepath.ToSlash(d.Path)))
	if p == "." || strings.HasPrefix(p, "../") || path.IsAbs(p) {
		im.res.Skipped++
		return
	}
	for _, o := range d.Occurrences {
		if o.Roles&RoleDefinition != 0 {
			if info := im.symbol(o.Symbol); info.ok && !info.local {
				im.pkgs[info.pkg] = true
			}
		}
	}
	if im.opts.Skip != nil && im.opts.Skip(p) {
		im.res.Skipped++
		return
	}
	want, ok := im.opts.Indexed(p)
	if !ok || want == 0 {
		im.res.Skipped++
		return
	}
	if _, dup := im.files[p]; dup {
		return // an earlier source covered it
	}
	name := filepath.Join(im.opts.Root, filepath.FromSlash(p))
	fi, err := os.Stat(name)
	if err != nil {
		im.res.Rejected[p] = "unreadable"
		return
	}
	data, err := os.ReadFile(name)
	if err != nil {
		im.res.Rejected[p] = "unreadable"
		return
	}
	hash := xxhash.Sum64(data)
	if hash != want {
		im.res.Rejected[p] = "not indexed yet"
		return
	}
	lines := splitLines(data)
	check := false
	switch {
	case d.HasText:
		if xxhash.Sum64(d.Text) != hash {
			im.res.Rejected[p] = "changed since indexed (document text differs)"
			return
		}
	case src.Observed != nil:
		if h, ok := src.Observed[p]; !ok || h != hash {
			im.res.Rejected[p] = "changed while the indexer ran"
			return
		}
	default:
		if fi.ModTime().After(indexTime) {
			im.res.Rejected[p] = "modified after the index was written"
			return
		}
		check = true
	}
	df := &docFacts{path: p, hash: hash}
	expansions := map[[3]int]bool{} // ranges of macro occurrences (scip-clang)
	for _, o := range d.Occurrences {
		if info := im.symbol(o.Symbol); info.macro {
			expansions[[3]int{o.StartLine, o.StartChar, o.EndChar}] = true
		}
	}
	var defs []struct {
		symbol string
		d      def
	}
	seen := map[site]bool{}
	checked, soft := 0, 0
	for _, o := range d.Occurrences {
		info := im.symbol(o.Symbol)
		if !info.ok || info.local || !(info.callable || info.typ) || o.StartLine != o.EndLine {
			continue
		}
		if expansions[[3]int{o.StartLine, o.StartChar, o.EndChar}] {
			continue
		}
		if o.StartLine < 0 || o.StartLine >= len(lines) {
			if check {
				im.res.Rejected[p] = fmt.Sprintf("occurrence past the end of the file (line %d)", o.StartLine+1)
				return
			}
			continue
		}
		line := lines[o.StartLine]
		loc := locate(line, o, d.Encoding, info.spellings)
		start, text, whole := loc.start, loc.text, loc.whole
		matches := slices.Contains(info.spellings, text)
		isDef := o.Roles&RoleDefinition != 0
		if check && len(info.spellings) > 0 && (isDef || !info.implicit) {
			switch {
			case matches:
				checked++
			case isDef && isIdent(loc.raw), !loc.rawWhole, strings.TrimSpace(loc.raw) != loc.raw || loc.raw == "":
				// A definition now spells another name, or a range splits
				// an identifier, starts or ends in blank space or is past
				// the end of its line: the file changed since.
				im.res.Rejected[p] = fmt.Sprintf("line %d no longer spells %q", o.StartLine+1, info.spellings[0])
				return
			case isIdent(loc.raw):
				checked++
				soft++ // another name: an alias (import {a as b})
			}
			// Otherwise the range covers an expression (a Kotlin function
			// type such as (A) -> R for Function1, `companion object`):
			// not checkable.
		}
		if !matches || !whole {
			continue // only sites spelling their symbol are imported
		}
		if isDef {
			defs = append(defs, struct {
				symbol string
				d      def
			}{o.Symbol, def{p, o.StartLine + 1, text}})
			continue
		}
		if o.Roles&(RoleImport|RoleForwardDefinition) != 0 {
			continue
		}
		s := site{line: int32(o.StartLine + 1), col: int32(start + 1), symbol: o.Symbol, text: text}
		if seen[s] {
			continue
		}
		seen[s] = true
		df.sites = append(df.sites, s)
	}
	if check && soft > maxSoft(checked) {
		im.res.Rejected[p] = fmt.Sprintf("%d of %d references no longer spell their symbol", soft, checked)
		return
	}
	for _, x := range defs {
		im.defs[x.symbol] = append(im.defs[x.symbol], x.d)
	}
	im.files[p] = df
}

// location is where an occurrence's name is on its line.
type location struct {
	start    int    // byte offset of text
	text     string // the name found, or the range's text
	whole    bool   // text is an identifier, not part of a longer one
	raw      string // the range's own text
	rawWhole bool   // raw is not part of a longer identifier
}

// locate finds an occurrence's name on line. When the range's text is not
// one of the symbol's spellings, columns counting a tab as advancing to
// the next multiple of 8 (scip-java takes some columns from javac) are
// tried, and then a spelling standing as a word inside the range (Kotlin
// type expressions such as Outer.T<A> or `when`, Python's `a as b`, Java's
// T::new).
func locate(line string, o Occurrence, enc int, spellings []string) location {
	s, e, text := slice(line, o.StartChar, o.EndChar, enc)
	loc := location{raw: text, rawWhole: !identByte(line, s-1) && !identByte(line, e)}
	if !slices.Contains(spellings, text) && strings.Contains(line, "\t") {
		if s2, e2, t2 := slice(line, untab(line, o.StartChar), untab(line, o.EndChar), EncodingUTF16); slices.Contains(spellings, t2) {
			s, e, text = s2, e2, t2
		}
	}
	if !slices.Contains(spellings, text) && len(text) > 0 {
		for _, sp := range spellings {
			if i := wordIndex(line[s:e], sp); i >= 0 {
				s, e, text = s+i, s+i+len(sp), sp
				break
			}
		}
	}
	loc.start, loc.text = s, text
	loc.whole = text != "" && isIdent(text) && !identByte(line, s-1) && !identByte(line, e)
	return loc
}

// wordIndex returns the first index of word in s where it is not part of
// a longer identifier, or -1.
func wordIndex(s, word string) int {
	for off := 0; off+len(word) <= len(s); {
		i := strings.Index(s[off:], word)
		if i < 0 {
			return -1
		}
		i += off
		if !identByte(s, i-1) && !identByte(s, i+len(word)) {
			return i
		}
		off = i + 1
	}
	return -1
}

func identByte(line string, i int) bool {
	if i < 0 || i >= len(line) {
		return false
	}
	c := line[i]
	return c == '_' || c == '$' || c >= 0x80 || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9'
}

// build turns the imported documents into directory facts.
func (im *importer) build() {
	byDir := map[string]*precise.Dir{}
	for _, df := range im.files {
		f := precise.File{Hash: df.hash}
		d := byDir[path.Dir(df.path)]
		if d == nil {
			d = &precise.Dir{Path: path.Dir(df.path), Files: map[string]precise.File{}, Defs: map[string]uint64{}}
			byDir[d.Path] = d
		}
		for _, s := range df.sites {
			info := im.symbol(s.symbol)
			c := precise.Call{Line: s.line, Col: s.col, Name: s.text}
			if ds := im.defs[s.symbol]; len(ds) > 0 {
				t := nearest(ds, df.path, int(s.line))
				c.File, c.Name, c.DefLine = t.path, t.text, int32(t.line)
				d.Defs[t.path] = im.files[t.path].hash
			} else if im.pkgs[info.pkg] {
				continue // the workspace's, but its definition was not imported
			}
			if info.callable {
				f.Calls = append(f.Calls, c)
			} else {
				f.TypeRefs = append(f.TypeRefs, c)
			}
		}
		byPos := func(a, b precise.Call) int {
			if a.Line != b.Line {
				return int(a.Line - b.Line)
			}
			return int(a.Col - b.Col)
		}
		f.Calls = dedupe(f.Calls, byPos)
		f.TypeRefs = dedupe(f.TypeRefs, byPos)
		im.res.Sites += len(f.Calls) + len(f.TypeRefs)
		d.Files[df.path] = f
		im.res.Docs++
	}
	for _, d := range byDir {
		im.res.Dirs = append(im.res.Dirs, d)
	}
	slices.SortFunc(im.res.Dirs, func(a, b *precise.Dir) int { return strings.Compare(a.Path, b.Path) })
}

// dedupe sorts sites by position and drops positions with conflicting
// targets (several symbols at one range, such as a Kotlin property and its
// accessor): an ambiguous site keeps its syntactic answer.
func dedupe(cs []precise.Call, cmp func(a, b precise.Call) int) []precise.Call {
	slices.SortStableFunc(cs, cmp)
	out := cs[:0]
	for i := 0; i < len(cs); {
		j := i + 1
		same := true
		for j < len(cs) && cmp(cs[i], cs[j]) == 0 {
			same = same && cs[j].File == cs[i].File && cs[j].Name == cs[i].Name && cs[j].DefLine == cs[i].DefLine
			j++
		}
		if same {
			out = append(out, cs[i])
		}
		i = j
	}
	return out
}

// nearest picks the definition a reference at file:line points to when
// its symbol has several: the nearest preceding one in the same file, else
// the nearest following one there, else the first. (scip-clang gives
// same-named static functions in different translation units one symbol,
// and scip-python a function defined twice in one file.)
func nearest(defs []def, file string, line int) def {
	best, found := defs[0], false
	for _, d := range defs {
		if d.path != file {
			continue
		}
		switch {
		case !found:
			best, found = d, true
		case d.line <= line && (best.line > line || d.line > best.line):
			best = d
		case d.line > line && best.line > line && d.line < best.line:
			best = d
		}
	}
	return best
}

func splitLines(data []byte) []string {
	s := string(data)
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// untab converts a column that counts a tab as advancing to the next
// multiple of 8 into a UTF-16 offset on line.
func untab(line string, col int) int {
	c, u := 0, 0 // expanded column, UTF-16 offset
	for _, r := range line {
		if c >= col {
			break
		}
		if r == '\t' {
			c = (c/8 + 1) * 8
		} else {
			c++
		}
		u += len(utf16.Encode([]rune{r}))
	}
	return u
}

// slice returns the byte range and text of [startChar, endChar) on line in
// the given position encoding (UTF-16 when unspecified).
func slice(line string, startChar, endChar, enc int) (int, int, string) {
	toByte := func(ch int) int {
		switch enc {
		case EncodingUTF8:
			return min(max(ch, 0), len(line))
		case EncodingUTF32:
			b, n := 0, 0
			for b < len(line) && n < ch {
				_, w := utf8.DecodeRuneInString(line[b:])
				b, n = b+w, n+1
			}
			return b
		default: // UTF-16
			b, n := 0, 0
			for b < len(line) && n < ch {
				r, w := utf8.DecodeRuneInString(line[b:])
				b, n = b+w, n+len(utf16.Encode([]rune{r}))
			}
			return b
		}
	}
	s, e := toByte(startChar), toByte(endChar)
	if s >= e || e > len(line) {
		return 0, 0, ""
	}
	return s, e, line[s:e]
}
