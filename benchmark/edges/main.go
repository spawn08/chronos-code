// Command edges measures the chronos indexer's reference resolution
// against a SCIP index of the same checkout (docs/chronos-indexer.md,
// "Evaluation"). SCIP's definitions are the gold answer: for every
// reference the indexer records at a position where SCIP has a reference
// to an in-repository declaration, the resolved target is correct when it
// is the declaration SCIP defines.
//
// Two groups are reported: calls (calls and instantiations, against SCIP
// references to functions and methods) and types (type uses, extends and
// implements, against references to types). For each, per resolution
// label: how many references carry it and how often the first target
// (top1) or any target (any) is correct; coverage is the share of SCIP's
// references at which the indexer recorded one, and recall the share of
// matched references resolved to the right declaration first. Results are
// merged into -out by -name; run.sh drives it over pinned repositories.
//
// With -errata and -compdb the gold answer is corrected (corrections.go):
// reviewed references whose SCIP target cannot be the callee are not
// scored, and C/C++ calls are checked against clang's AST. The numbers
// against SCIP alone are kept under "raw".
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/spawn08/chronos-code/internal/indexer"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/query"
)

// LabelStats counts references with one resolution label.
type LabelStats struct {
	N    int `json:"n"`
	Top1 int `json:"top1"`
	Any  int `json:"any"`
}

// Group is the measurement of one kind of reference: calls (calls and
// instantiations, against SCIP references to functions and methods) or
// types (type uses, extends and implements, against references to types).
type Group struct {
	Refs     int                    `json:"refs"`     // indexer references of the group's kinds
	Gold     int                    `json:"gold"`     // SCIP references (not imports) to in-repo declarations of the group
	Matched  int                    `json:"matched"`  // indexer references at a gold position
	Coverage float64                `json:"coverage"` // matched gold / gold
	Recall   float64                `json:"recall"`   // top1-correct / matched
	Labels   map[string]*LabelStats `json:"labels"`   // by resolution label; "unresolved" when none
	// Corrections of the gold answer (see corrections); raw SCIP numbers
	// are in Result.Raw.
	Errata    int `json:"errata,omitempty"`    // matched references excluded by the errata file
	Corrected int `json:"corrected,omitempty"` // matched references scored against clang where SCIP disagrees
}

// Groups are the two measurements, for Result.Raw.
type Groups struct {
	Calls Group `json:"calls"`
	Types Group `json:"types"`
}

// Result is one repository's measurement.
type Result struct {
	Name    string `json:"name"`
	Lang    string `json:"lang"`
	Repo    string `json:"repo,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Indexer string `json:"indexer,omitempty"`
	Calls   Group  `json:"calls"`
	Types   Group  `json:"types"`
	// Raw is the measurement against SCIP alone, when corrections applied.
	Raw      *Groups `json:"raw,omitempty"`
	Measured string  `json:"measured"` // date
	Seconds  float64 `json:"seconds"`  // indexing plus resolution time
}

// Baseline is the checked-in file of results.
type Baseline struct {
	Results []Result `json:"results"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "edges:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		name    = flag.String("name", "", "result name (unique in -out)")
		lang    = flag.String("lang", "", "language label for the report")
		repo    = flag.String("repo", "", "checkout to evaluate")
		scip    = flag.String("scip", "", "SCIP index of the checkout")
		out     = flag.String("out", "", "baseline JSON to merge the result into (stdout when empty)")
		origin  = flag.String("origin", "", "repository URL, for the record")
		commit  = flag.String("commit", "", "commit, for the record")
		tool    = flag.String("indexer", "", "SCIP indexer and version, for the record")
		verbose = flag.Bool("v", false, "print every incorrect or unresolved reference")
		errata  = flag.String("errata", "", "errata file: gold targets SCIP gets wrong, excluded from scoring")
		compdb  = flag.String("compdb", "", "compile_commands.json: check C/C++ calls against clang's AST")
		clang   = flag.String("clang", "clang", "clang for -compdb")
	)
	flag.Parse()
	if *name == "" || *repo == "" || *scip == "" {
		return errors.New("-name, -repo and -scip are required")
	}
	idx, err := readSCIP(*scip)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(*repo)
	if err != nil {
		return fmt.Errorf("resolve repo: %w", err)
	}
	if c, err := filepath.EvalSymlinks(root); err == nil {
		root = c
	}
	start := time.Now()
	corr, err := loadCorrections(*name, root, *errata, *compdb, *clang)
	if err != nil {
		return err
	}
	res, err := evaluate(root, idx, *verbose, corr)
	if err != nil {
		return err
	}
	res.Name, res.Lang, res.Repo, res.Commit, res.Indexer = *name, *lang, *origin, *commit, *tool
	res.Measured = time.Now().UTC().Format("2006-01-02")
	res.Seconds = time.Since(start).Seconds()
	return write(*out, res)
}

// gold is a SCIP reference whose definition is in the repository.
type gold struct {
	start, end int // byte columns on the line
	text       string
	defPath    string
	defLine    int  // 1-based
	callable   bool // a function or method (else a type)
	// Corrections: errata excludes the reference; oracle holds clang's
	// callee declarations, which replace SCIP's definition.
	errata bool
	oracle []lineKey
}

type lineKey struct {
	path string
	line int // 1-based
}

// occKey identifies an occurrence within a document.
type occKey struct {
	line, start, end int // SCIP positions
	symbol           string
}

func evaluate(root string, idx *scipIndex, verbose bool, corr *corrections) (Result, error) {
	defs := map[string][]lineKey{}
	for _, d := range idx.Documents {
		for _, o := range d.Occurrences {
			if o.Roles&roleDefinition != 0 && !strings.HasPrefix(o.Symbol, "local ") {
				defs[o.Symbol] = append(defs[o.Symbol], lineKey{filepath.ToSlash(d.Path), o.StartLine + 1})
			}
		}
	}
	golds := map[lineKey][]gold{}
	errataGold := map[bool]int{} // by callable
	var res Result
	skipped := map[string]int{} // occurrences not scored, by reason
	for _, d := range idx.Documents {
		lines, err := readLines(filepath.Join(root, filepath.FromSlash(d.Path)))
		if err != nil {
			continue
		}
		path := filepath.ToSlash(d.Path)
		// scip-clang attributes the references inside a macro expansion to
		// the macro name (CJSON_PUBLIC(cJSON *) references cJSON# at
		// "CJSON_PUBLIC"); such an occurrence shares its range with the
		// macro's (descriptor suffix "!") and is not scored.
		expansions := map[occKey]bool{}
		for _, o := range d.Occurrences {
			if strings.HasSuffix(o.Symbol, "!") {
				expansions[occKey{o.StartLine, o.StartChar, o.EndChar, ""}] = true
			}
		}
		seen := map[occKey]bool{}
		for _, o := range d.Occurrences {
			ds, ok := defs[o.Symbol]
			if !ok || o.Roles&(roleDefinition|roleImport) != 0 || !evaluated(o.Symbol) || o.StartLine >= len(lines) {
				continue
			}
			if expansions[occKey{o.StartLine, o.StartChar, o.EndChar, ""}] {
				skipped["macro expansion"]++
				continue
			}
			// scip-clang can emit the same occurrence twice.
			key := occKey{o.StartLine, o.StartChar, o.EndChar, o.Symbol}
			if seen[key] {
				skipped["duplicate"]++
				continue
			}
			seen[key] = true
			if strings.HasPrefix(o.Symbol, "semanticdb ") && strings.HasPrefix(strings.TrimSpace(lines[o.StartLine]), "import ") {
				skipped["import"]++ // scip-java does not give imports the import role
				continue
			}
			sc, ec := o.StartChar, o.EndChar
			if strings.HasPrefix(o.Symbol, "semanticdb ") && strings.HasSuffix(path, ".java") {
				// scip-java takes some columns from javac, which expands
				// tabs to multiples of 8, and others from the text.
				l := lines[o.StartLine]
				if _, _, t := slice(l, sc, ec, d.Encoding); t != symbolName(o.Symbol) {
					if _, _, t := slice(l, untab(l, sc), untab(l, ec), d.Encoding); t == symbolName(o.Symbol) {
						sc, ec = untab(l, sc), untab(l, ec)
					}
				}
			}
			start, end, text := slice(lines[o.StartLine], sc, ec, d.Encoding)
			if m := typeExpr.FindStringSubmatch(text); m != nil {
				// scip-java's Kotlin plugin covers a whole type expression
				// (KArgumentCaptor<T>, UseConstructor?); score its name.
				text, end = m[1], start+len(m[1])
			}
			if text == "" {
				continue
			}
			callable := strings.HasSuffix(o.Symbol, ").")
			if callable && strings.HasPrefix(o.Symbol, "semanticdb ") && strings.HasSuffix(path, ".kt") && text != symbolName(o.Symbol) {
				// scip-java's Kotlin plugin adds the accessor (getFirst().)
				// to a property's references; the property is not scored.
				skipped["property accessor"]++
				continue
			}
			if strings.HasPrefix(o.Symbol, "cxx ") && callable {
				if reason := clangNotReference(o.Symbol, text, lines, o.StartLine, start, end); reason != "" {
					skipped[reason]++
					continue
				}
			}
			def := definition(ds, path, o.StartLine+1)
			g := gold{start: start, end: end, text: text, defPath: def.path, defLine: def.line, callable: callable}
			k := lineKey{path, o.StartLine + 1}
			if corr != nil {
				site := siteKey{path, o.StartLine + 1, text}
				_, g.errata = corr.errata[site]
				if callable {
					g.oracle = corr.oracle[site]
				}
			}
			golds[k] = append(golds[k], g)
			if g.callable {
				res.Calls.Gold++
			} else {
				res.Types.Gold++
			}
			if g.errata {
				errataGold[g.callable]++
			}
		}
	}
	if verbose && len(skipped) > 0 {
		fmt.Fprintf(os.Stderr, "not scored: %v\n", skipped)
	}

	dir, err := os.MkdirTemp("", "chronos-edges-*")
	if err != nil {
		return Result{}, fmt.Errorf("temp index dir: %w", err)
	}
	defer os.RemoveAll(dir)
	eng, err := indexer.Open(indexer.Options{Root: root, Dir: dir, ProgressiveFiles: -1})
	if err != nil {
		return Result{}, err
	}
	defer eng.Close()
	if _, err := eng.Reconcile(context.Background()); err != nil {
		return Result{}, fmt.Errorf("index %s: %w", root, err)
	}
	sn := eng.Snapshot()
	defer sn.Release()
	v := query.NewView(sn, query.NewCache())

	hit := map[lineKey]map[int]bool{} // matched gold starts, both groups
	measure := func(grp *Group, callable bool, kinds []uint8, corrected, verbose bool) {
		grp.Labels = map[string]*LabelStats{}
		correct := 0
		counted := map[lineKey]map[int]bool{} // this group's gold, for coverage
		v.EachReference(kinds, func(r query.ResolvedRef) {
			grp.Refs++
			k := lineKey{r.File, r.Line}
			g, ok := match(golds[k], r)
			if !ok {
				return
			}
			if corrected && g.errata {
				grp.Errata++
				return
			}
			grp.Matched++
			if hit[k] == nil {
				hit[k] = map[int]bool{}
			}
			hit[k][g.start] = true
			if g.callable == callable {
				if counted[k] == nil {
					counted[k] = map[int]bool{}
				}
				counted[k][g.start] = true
			}
			label := r.Resolution
			if label == "" {
				label = "unresolved"
			}
			ls := grp.Labels[label]
			if ls == nil {
				ls = &LabelStats{}
				grp.Labels[label] = ls
			}
			ls.N++
			want := []lineKey{{g.defPath, g.defLine}}
			if corrected && len(g.oracle) > 0 {
				if !slices.Contains(g.oracle, want[0]) {
					grp.Corrected++
				}
				want = g.oracle
			}
			top1, any := false, false
			for i, t := range r.Targets {
				for _, w := range want {
					if t.File == w.path && w.line >= t.Line && w.line <= max(t.Line, t.EndLine) {
						any = true
						top1 = top1 || i == 0
					}
				}
			}
			if top1 {
				ls.Top1++
				correct++
			}
			if any {
				ls.Any++
			}
			if verbose && !top1 {
				var got []string
				for _, t := range r.Targets {
					got = append(got, fmt.Sprintf("%s:%d", t.File, t.Line))
				}
				note := "" // matched by text only: SCIP's reference is elsewhere on the line
				if r.Col-1 < g.start || r.Col-1 >= g.end {
					note = " (other column)"
				}
				fmt.Fprintf(os.Stderr, "%s:%d %s [%s] want %s:%d got %v%s\n", r.File, r.Line, r.Name, label, want[0].path, want[0].line, got, note)
			}
		})
		matchedGold := 0
		for _, starts := range counted {
			matchedGold += len(starts)
		}
		if grp.Gold > 0 {
			grp.Coverage = round(float64(matchedGold) / float64(grp.Gold))
		}
		if grp.Matched > 0 {
			grp.Recall = round(float64(correct) / float64(grp.Matched))
		}
	}
	calls, types := []uint8{facts.RefCall, facts.RefInstantiate}, []uint8{facts.RefTypeUse, facts.RefExtends, facts.RefImplements}
	if corr != nil {
		res.Raw = &Groups{Calls: Group{Gold: res.Calls.Gold}, Types: Group{Gold: res.Types.Gold}}
		measure(&res.Raw.Calls, true, calls, false, false)
		measure(&res.Raw.Types, false, types, false, false)
		hit = map[lineKey]map[int]bool{}
		res.Calls.Gold -= errataGold[true]
		res.Types.Gold -= errataGold[false]
	}
	measure(&res.Calls, true, calls, corr != nil, verbose)
	measure(&res.Types, false, types, corr != nil, verbose)
	if verbose { // every gold reference without an indexer reference, in file order
		keys := slices.Collect(maps.Keys(golds))
		slices.SortFunc(keys, func(a, b lineKey) int {
			return cmp.Or(strings.Compare(a.path, b.path), cmp.Compare(a.line, b.line))
		})
		for _, k := range keys {
			for _, g := range golds[k] {
				if !hit[k][g.start] {
					kind := "type"
					if g.callable {
						kind = "call"
					}
					fmt.Fprintf(os.Stderr, "unmatched gold %s:%d %q %s -> %s:%d\n", k.path, k.line, g.text, kind, g.defPath, g.defLine)
				}
			}
		}
	}
	return res, nil
}

// evaluated reports SCIP symbols of callables and types: method and
// function descriptors end in ")." and type descriptors in "#".
func evaluated(symbol string) bool {
	return strings.HasSuffix(symbol, ").") || strings.HasSuffix(symbol, "#")
}

// definition picks the definition a reference at path:line is scored
// against when its symbol has several: the nearest preceding one in the
// same document, else the nearest following one there, else the first.
// scip-clang gives same-named static functions in different translation
// units one symbol, and scip-python a function defined twice in one file.
func definition(defs []lineKey, path string, line int) lineKey {
	best, found := defs[0], false
	for _, d := range defs {
		if d.path != path {
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

// match finds the gold reference on the ref's line with the same text,
// preferring the one starting at the ref's column.
func match(cands []gold, r query.ResolvedRef) (gold, bool) {
	var found []gold
	for _, g := range cands {
		if g.text == r.Name {
			found = append(found, g)
		}
	}
	if len(found) == 0 {
		return gold{}, false
	}
	for _, g := range found {
		if r.Col-1 >= g.start && r.Col-1 < g.end {
			return g, true
		}
	}
	return found[0], true
}

// typeExpr matches a type name with type arguments or a nullable mark.
var typeExpr = regexp.MustCompile(`^([\pL_][\pL\pN_]*)(?:<.*>)?\??$`)

// untab converts a column that counts a tab as advancing to the next
// multiple of 8 into a UTF-16 offset on line.
func untab(line string, col int) int {
	if !strings.Contains(line, "\t") {
		return col
	}
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
// the document's position encoding (UTF-16 when unspecified).
func slice(line string, startChar, endChar, enc int) (int, int, string) {
	toByte := func(ch int) int {
		switch enc {
		case encodingUTF8:
			return min(ch, len(line))
		case encodingUTF32:
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

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	return lines, sc.Err()
}

func round(f float64) float64 { return float64(int(f*1000+0.5)) / 1000 }

func write(out string, res Result) error {
	if out == "" {
		data, err := json.MarshalIndent(res, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		return nil
	}
	var b Baseline
	if data, err := os.ReadFile(out); err == nil {
		if err := json.Unmarshal(data, &b); err != nil {
			return fmt.Errorf("parse %s: %w", out, err)
		}
	}
	b.Results = slices.DeleteFunc(b.Results, func(r Result) bool { return r.Name == res.Name })
	b.Results = append(b.Results, res)
	slices.SortFunc(b.Results, func(x, y Result) int { return strings.Compare(x.Name, y.Name) })
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(out, append(data, '\n'), 0o644)
}
