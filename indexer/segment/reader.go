package segment

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"unsafe"

	"github.com/cespare/xxhash/v2"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

// Segment is a validated, read-only view of one segment image.
type Segment struct {
	data   []byte
	unmap  func() error
	kind   Kind
	gen    uint64
	strs   []byte
	files  []byte
	syms   []byte
	sbf    []byte
	imps   []byte
	ibp    []byte
	refs   []byte
	rbe    []byte
	rbf    []byte
	stats  []byte
	docLen []byte
	terms  []byte
	posts  []byte
	names  []byte
	tris   []byte
	triPst []byte
	pkgFs  []byte
	inames []byte
	exps   []byte
	hints  []byte
	nFiles int
	nSyms  int
	nRefs  int
	nImps  int
	nDecls int // symbols other than embeds
}

// Open maps and validates the segment file at path.
func Open(path string) (*Segment, error) {
	data, unmap, err := mapFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(data)
	if err != nil {
		_ = unmap()
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.unmap = unmap
	return s, nil
}

// Close releases the mapping. The segment must not be used afterwards.
func (s *Segment) Close() error {
	if s.unmap == nil {
		return nil
	}
	err := s.unmap()
	s.unmap, s.data = nil, nil
	return err
}

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
}

// Parse validates an in-memory segment image. data must outlive the Segment.
func Parse(data []byte) (*Segment, error) {
	if len(data) < headerSize || !bytes.Equal(data[:8], Magic[:]) {
		return nil, corrupt("bad magic")
	}
	if xxhash.Sum64(data[:40]) != le.Uint64(data[40:]) {
		return nil, corrupt("header checksum")
	}
	if v := le.Uint16(data[8:]); v != Version {
		return nil, corrupt("unsupported version %d", v)
	}
	if n := le.Uint32(data[12:]); n != numSections {
		return nil, corrupt("section count %d", n)
	}
	tableOff := le.Uint64(data[24:])
	if tableOff > uint64(len(data)) || uint64(len(data))-tableOff != numSections*sectionEntry {
		return nil, corrupt("section table bounds")
	}
	table := data[tableOff:]
	if xxhash.Sum64(table) != le.Uint64(data[32:]) {
		return nil, corrupt("section table checksum")
	}
	s := &Segment{data: data, kind: Kind(le.Uint16(data[10:])), gen: le.Uint64(data[16:])}
	secs := make([][]byte, numSections)
	for i := range secs {
		e := table[i*sectionEntry:]
		off, n := le.Uint64(e[8:]), le.Uint64(e[16:])
		if le.Uint32(e) != uint32(i+1) || off < headerSize || off > tableOff || n > tableOff-off {
			return nil, corrupt("section %d bounds", i+1)
		}
		secs[i] = data[off : off+n : off+n]
		if xxhash.Sum64(secs[i]) != le.Uint64(e[24:]) {
			return nil, corrupt("section %d checksum", i+1)
		}
	}
	s.strs, s.files, s.syms, s.sbf = secs[secStrings-1], secs[secFiles-1], secs[secSymbols-1], secs[secSymByFile-1]
	s.imps, s.ibp, s.refs, s.rbe, s.rbf = secs[secImports-1], secs[secImportsByPath-1], secs[secRefs-1], secs[secRefsByEnclosing-1], secs[secRefsByFile-1]
	s.stats, s.docLen, s.terms, s.posts = secs[secSearchStats-1], secs[secDocLen-1], secs[secTerms-1], secs[secPostings-1]
	s.names, s.tris, s.triPst, s.pkgFs = secs[secNames-1], secs[secTrigrams-1], secs[secTriPost-1], secs[secPkgFiles-1]
	s.inames, s.exps, s.hints = secs[secImportNames-1], secs[secExports-1], secs[secHints-1]
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Segment) validate() error {
	for name, pair := range map[string][2]int{
		"files": {len(s.files), fileRecSize}, "symbols": {len(s.syms), symbolRecSize},
		"imports": {len(s.imps), importRecSize}, "refs": {len(s.refs), refRecSize},
		"symByFile": {len(s.sbf), 4}, "importsByPath": {len(s.ibp), 4},
		"refsByEnclosing": {len(s.rbe), 4}, "refsByFile": {len(s.rbf), 4},
		"importNames": {len(s.inames), impNameRecSize}, "exports": {len(s.exps), exportRecSize},
		"hints": {len(s.hints), hintRecSize},
	} {
		if pair[0]%pair[1] != 0 {
			return corrupt("%s size", name)
		}
	}
	s.nFiles, s.nSyms = len(s.files)/fileRecSize, len(s.syms)/symbolRecSize
	s.nImps, s.nRefs = len(s.imps)/importRecSize, len(s.refs)/refRecSize
	nImpNames, nExps, nHints := len(s.inames)/impNameRecSize, len(s.exps)/exportRecSize, len(s.hints)/hintRecSize
	nStr := uint64(len(s.strs))
	okRef := func(b []byte) bool {
		r := getRef(b)
		return uint64(r.off)+uint64(r.n) <= nStr
	}
	okRange := func(off, n uint32, limit int) bool { return uint64(off)+uint64(n) <= uint64(limit) }
	prev := ""
	for i := 0; i < s.nFiles; i++ {
		b := s.files[i*fileRecSize:]
		for o := 0; o < 40; o += strRefSize {
			if !okRef(b[o:]) {
				return corrupt("file %d string ref", i)
			}
		}
		if !okRange(le.Uint32(b[64:]), le.Uint32(b[68:]), len(s.sbf)/4) ||
			!okRange(le.Uint32(b[72:]), le.Uint32(b[76:]), s.nImps) ||
			!okRange(le.Uint32(b[80:]), le.Uint32(b[84:]), len(s.rbf)/4) ||
			!okRange(le.Uint32(b[96:]), le.Uint32(b[100:]), nExps) ||
			!okRange(le.Uint32(b[104:]), le.Uint32(b[108:]), nHints) {
			return corrupt("file %d ranges", i)
		}
		p := s.view(b[0:])
		if i > 0 && p <= prev {
			return corrupt("files not sorted")
		}
		prev = p
	}
	for i := 0; i < s.nSyms; i++ {
		b := s.syms[i*symbolRecSize:]
		for o := 0; o < 32; o += strRefSize {
			if !okRef(b[o:]) {
				return corrupt("symbol %d string ref", i)
			}
		}
		if int(le.Uint32(b[32:])) >= s.nFiles || int(b[44]) >= len(facts.Kinds) || b[46] >= facts.NumVisibility {
			return corrupt("symbol %d fields", i)
		}
		if !okRef(b[64:]) || b[45]&flagArity != 0 && (b[56] > facts.MaxArity || b[57] != varArgs && b[57] < b[56]) {
			return corrupt("symbol %d arity", i)
		}
	}
	if err := s.validateSearch(okRef); err != nil {
		return err
	}
	for i := 0; i < s.nSyms; i++ {
		b := s.syms[i*symbolRecSize:]
		if facts.Kinds[b[44]] != facts.KindEmbed {
			s.nDecls++
		}
		if c := le.Uint32(b[48:]); c != noCaller && (int(c) >= s.nSyms || int(c) == i ||
			le.Uint32(s.syms[int(c)*symbolRecSize+32:]) != le.Uint32(b[32:])) {
			return corrupt("symbol %d container", i)
		}
	}
	for i := 0; i < s.nImps; i++ {
		b := s.imps[i*importRecSize:]
		if !okRef(b[0:]) || !okRef(b[8:]) || int(le.Uint32(b[16:])) >= s.nFiles || b[24] >= facts.NumImportKinds ||
			!okRange(le.Uint32(b[28:]), le.Uint32(b[32:]), nImpNames) {
			return corrupt("import %d", i)
		}
	}
	for i := 0; i < nImpNames; i++ {
		if b := s.inames[i*impNameRecSize:]; !okRef(b[0:]) || !okRef(b[8:]) {
			return corrupt("import name %d", i)
		}
	}
	for i := 0; i < s.nRefs; i++ {
		b := s.refs[i*refRecSize:]
		c := le.Uint32(b[20:])
		f := le.Uint32(b[16:])
		if !okRef(b[0:]) || !okRef(b[8:]) || !okRef(b[36:]) || int(f) >= s.nFiles || b[31] >= facts.NumRefKinds ||
			(c != noCaller && (int(c) >= s.nSyms || le.Uint32(s.syms[int(c)*symbolRecSize+32:]) != f)) {
			return corrupt("ref %d", i)
		}
		if l := le.Uint32(b[44:]); l != noCaller && (int(l) >= s.nRefs || int(l) == i || le.Uint32(s.refs[int(l)*refRecSize+16:]) != f) {
			return corrupt("ref %d lambda", i)
		}
	}
	for i := 0; i < nExps; i++ {
		b := s.exps[i*exportRecSize:]
		if !okRef(b[0:]) || !okRef(b[8:]) || !okRef(b[16:]) || int(le.Uint32(b[24:])) >= s.nFiles {
			return corrupt("export %d", i)
		}
	}
	for i := 0; i < nHints; i++ {
		b := s.hints[i*hintRecSize:]
		f, scope := le.Uint32(b[16:]), le.Uint32(b[20:])
		if !okRef(b[0:]) || !okRef(b[8:]) || int(f) >= s.nFiles ||
			(scope != noCaller && (int(scope) >= s.nSyms || le.Uint32(s.syms[int(scope)*symbolRecSize+32:]) != f)) {
			return corrupt("hint %d", i)
		}
	}
	for _, idx := range []struct {
		b     []byte
		limit int
	}{{s.sbf, s.nSyms}, {s.ibp, s.nImps}, {s.rbe, s.nRefs}, {s.rbf, s.nRefs}} {
		for o := 0; o < len(idx.b); o += 4 {
			if int(le.Uint32(idx.b[o:])) >= idx.limit {
				return corrupt("index entry out of range")
			}
		}
	}
	return nil
}

func (s *Segment) validateSearch(okRef func([]byte) bool) error {
	if len(s.stats) != 16 || len(s.docLen) != s.nSyms*4 || len(s.pkgFs) != s.nFiles*4 ||
		len(s.terms)%termRecSize != 0 || len(s.posts)%postingRecSize != 0 ||
		len(s.names)%nameRecSize != 0 || len(s.tris)%termRecSize != 0 || len(s.triPst)%4 != 0 {
		return corrupt("search section sizes")
	}
	nPosts, nNames, nTri := len(s.posts)/postingRecSize, len(s.names)/nameRecSize, len(s.triPst)/4
	for _, dict := range []struct {
		b     []byte
		limit int
	}{{s.terms, nPosts}, {s.tris, nTri}} {
		prev := ""
		for i := 0; i+termRecSize <= len(dict.b); i += termRecSize {
			b := dict.b[i:]
			if !okRef(b) || uint64(le.Uint32(b[8:]))+uint64(le.Uint32(b[12:])) > uint64(dict.limit) {
				return corrupt("search dictionary entry")
			}
			if t := s.view(b); i > 0 && t <= prev {
				return corrupt("search dictionary not sorted")
			} else {
				prev = t
			}
		}
	}
	for i := 0; i < nPosts; i++ {
		if int(le.Uint32(s.posts[i*postingRecSize:])) >= s.nSyms {
			return corrupt("posting out of range")
		}
	}
	for i := 0; i < nNames; i++ {
		if !okRef(s.names[i*nameRecSize:]) || !okRef(s.names[i*nameRecSize+8:]) {
			return corrupt("name entry")
		}
	}
	for o := 0; o < len(s.triPst); o += 4 {
		if int(le.Uint32(s.triPst[o:])) >= nNames {
			return corrupt("trigram posting out of range")
		}
	}
	for o := 0; o < len(s.pkgFs); o += 4 {
		if int(le.Uint32(s.pkgFs[o:])) >= s.nFiles {
			return corrupt("package file out of range")
		}
	}
	return nil
}

// view returns a zero-copy string over the mapping; never retain it.
func (s *Segment) view(b []byte) string {
	r := getRef(b)
	if r.n == 0 {
		return ""
	}
	return unsafe.String(&s.strs[r.off], int(r.n))
}

// str returns an owned copy of a string field.
func (s *Segment) str(b []byte) string { return strings.Clone(s.view(b)) }

// Kind returns the segment kind.
func (s *Segment) Kind() Kind { return s.kind }

// Generation returns the generation the segment was written for.
func (s *Segment) Generation() uint64 { return s.gen }

// Size returns the image size in bytes.
func (s *Segment) Size() int { return len(s.data) }

// NumFiles, NumSymbols and NumRefs return record counts.
func (s *Segment) NumFiles() int   { return s.nFiles }
func (s *Segment) NumSymbols() int { return s.nSyms }
func (s *Segment) NumRefs() int    { return s.nRefs }

// NumDecls returns the number of symbols that are not embeds.
func (s *Segment) NumDecls() int { return s.nDecls }

// SearchStats returns the BM25 document count and total document length.
func (s *Segment) SearchStats() (docs int, sumLen float64) {
	return int(le.Uint64(s.stats[0:])), math.Float64frombits(le.Uint64(s.stats[8:]))
}

// DocLen returns symbol i's BM25 document length (0 for embeds).
func (s *Segment) DocLen(i int) float32 { return math.Float32frombits(le.Uint32(s.docLen[i*4:])) }

func dictFind(s *Segment, dict []byte, key string) (off, n int) {
	cnt := len(dict) / termRecSize
	i := sort.Search(cnt, func(i int) bool { return s.view(dict[i*termRecSize:]) >= key })
	if i == cnt || s.view(dict[i*termRecSize:]) != key {
		return 0, 0
	}
	b := dict[i*termRecSize:]
	return int(le.Uint32(b[8:])), int(le.Uint32(b[12:]))
}

// Postings returns the range of posting indexes for term.
func (s *Segment) Postings(term string) (lo, hi int) {
	off, n := dictFind(s, s.terms, term)
	return off, off + n
}

// Posting returns posting k: a symbol index and its term frequency.
func (s *Segment) Posting(k int) (sym int, tf float32) {
	b := s.posts[k*postingRecSize:]
	return int(le.Uint32(b)), math.Float32frombits(le.Uint32(b[4:]))
}

// NumNames returns the number of distinct declaration names.
func (s *Segment) NumNames() int { return len(s.names) / nameRecSize }

// Name returns distinct name i and its lower-cased form (zero-copy views).
func (s *Segment) Name(i int) (name, lower string) {
	return s.view(s.names[i*nameRecSize:]), s.view(s.names[i*nameRecSize+8:])
}

// TrigramNames returns the name indexes containing trigram g, ascending.
func (s *Segment) TrigramNames(g string) []int {
	off, n := dictFind(s, s.tris, g)
	return s.u32Range(s.triPst, uint32(off), uint32(n))
}

// NumPackageFiles returns the length of the package-ordered file list.
func (s *Segment) NumPackageFiles() int { return len(s.pkgFs) / 4 }

// PackageFile returns entry k of the file list ordered by (package, path).
func (s *Segment) PackageFile(k int) int { return int(le.Uint32(s.pkgFs[k*4:])) }

// FilePackage returns a zero-copy view of file i's package.
func (s *Segment) FilePackage(i int) string { return s.view(s.fileRec(i)[8:]) }

// PackageFiles returns the indexes of the files of pkg, in path order.
func (s *Segment) PackageFiles(pkg string) []int {
	n := s.NumPackageFiles()
	lo := sort.Search(n, func(k int) bool { return s.FilePackage(s.PackageFile(k)) >= pkg })
	hi := lo + sort.Search(n-lo, func(k int) bool { return s.FilePackage(s.PackageFile(lo+k)) > pkg })
	return s.u32Range(s.pkgFs, uint32(lo), uint32(hi-lo))
}

// FileDeleted reports whether file i is a tombstone.
func (s *Segment) FileDeleted(i int) bool { return le.Uint32(s.fileRec(i)[88:])&flagDeleted != 0 }

// FileLangView returns a zero-copy view of file i's language.
func (s *Segment) FileLangView(i int) string { return s.view(s.fileRec(i)[24:]) }

// FilePkgNameView returns a zero-copy view of file i's declared package.
func (s *Segment) FilePkgNameView(i int) string { return s.view(s.fileRec(i)[16:]) }

// FilePathView returns a zero-copy view of file i's path.
func (s *Segment) FilePathView(i int) string { return s.view(s.fileRec(i)) }

// FileMeta is a file record without its facts.
type FileMeta struct {
	Path, Package, PkgName, Lang, ParseErr string
	Hash                                   uint64
	Size, MtimeNS                          int64
	Deleted, Generated, Vendored, Test     bool
}

func (s *Segment) fileRec(i int) []byte { return s.files[i*fileRecSize : (i+1)*fileRecSize] }

// FilePath returns the path of file i.
func (s *Segment) FilePath(i int) string { return s.str(s.fileRec(i)) }

// FileMeta returns file i's metadata.
func (s *Segment) FileMeta(i int) FileMeta {
	b := s.fileRec(i)
	flags := le.Uint32(b[88:])
	return FileMeta{
		Path: s.str(b[0:]), Package: s.str(b[8:]), PkgName: s.str(b[16:]), Lang: s.str(b[24:]),
		ParseErr: s.str(b[32:]), Hash: le.Uint64(b[40:]), Size: int64(le.Uint64(b[48:])),
		MtimeNS: int64(le.Uint64(b[56:])), Deleted: flags&flagDeleted != 0,
		Generated: flags&flagGenerated != 0, Vendored: flags&flagVendored != 0, Test: flags&flagTest != 0,
	}
}

// FindFile returns the index of path, if present.
func (s *Segment) FindFile(path string) (int, bool) {
	i := sort.Search(s.nFiles, func(i int) bool { return s.view(s.fileRec(i)) >= path })
	return i, i < s.nFiles && s.view(s.fileRec(i)) == path
}

// SymbolRec is a symbol plus the segment file index that declares it.
// Symbol.Container is always 0 here; Parent holds the enclosing symbol's
// segment index, or -1.
type SymbolRec struct {
	facts.Symbol
	File   int
	Parent int
}

func (s *Segment) symRec(i int) []byte { return s.syms[i*symbolRecSize : (i+1)*symbolRecSize] }

// Symbol returns symbol i.
func (s *Segment) Symbol(i int) SymbolRec {
	b := s.symRec(i)
	return SymbolRec{
		Symbol: facts.Symbol{
			Name: s.str(b[0:]), Receiver: s.str(b[8:]), Signature: s.str(b[16:]), Doc: s.str(b[24:]),
			Line: int(le.Uint32(b[36:])), EndLine: int(le.Uint32(b[40:])), Kind: facts.Kinds[b[44]],
			Exported: b[45]&flagExported != 0, Visibility: b[46], Modifiers: le.Uint32(b[52:]),
			Params: arity(b), ParamList: s.str(b[64:]),
		},
		File:   int(le.Uint32(b[32:])),
		Parent: s.SymbolParent(i),
	}
}

func arity(b []byte) facts.Arity {
	if b[45]&flagArity == 0 {
		return facts.Arity{}
	}
	a := facts.Arity{Min: int(b[56]), Max: int(b[57]), Known: true}
	if b[57] == varArgs {
		a.Max = facts.VarArgs
	}
	return a
}

// SymbolFile returns the file index of symbol i without decoding it.
func (s *Segment) SymbolFile(i int) int { return int(le.Uint32(s.symRec(i)[32:])) }

// SymbolKind returns the kind of symbol i without decoding it.
func (s *Segment) SymbolKind(i int) string { return facts.Kinds[s.symRec(i)[44]] }

// SymbolName returns a zero-copy view of symbol i's name. It is valid only
// while the segment is open; clone it to retain it.
func (s *Segment) SymbolName(i int) string { return s.view(s.symRec(i)) }

// SymbolReceiver returns a zero-copy view of symbol i's receiver, valid only
// while the segment is open.
func (s *Segment) SymbolReceiver(i int) string { return s.view(s.symRec(i)[8:]) }

// SymbolParent returns the segment index of symbol i's enclosing symbol, or -1.
func (s *Segment) SymbolParent(i int) int {
	if c := le.Uint32(s.symRec(i)[48:]); c != noCaller {
		return int(c)
	}
	return -1
}

// SymbolsNamed returns the half-open range of symbol indexes named name.
func (s *Segment) SymbolsNamed(name string) (lo, hi int) {
	lo = sort.Search(s.nSyms, func(i int) bool { return s.view(s.symRec(i)) >= name })
	hi = lo + sort.Search(s.nSyms-lo, func(i int) bool { return s.view(s.symRec(lo+i)) > name })
	return lo, hi
}

// SymbolsInFile returns file i's symbol indexes in line order.
func (s *Segment) SymbolsInFile(i int) []int {
	b := s.fileRec(i)
	return s.u32Range(s.sbf, le.Uint32(b[64:]), le.Uint32(b[68:]))
}

func (s *Segment) u32Range(sec []byte, off, n uint32) []int {
	out := make([]int, n)
	for k := range out {
		out[k] = int(le.Uint32(sec[(int(off)+k)*4:]))
	}
	return out
}

// RefRec is a reference. Enclosing is a segment symbol index or -1.
type RefRec struct {
	Kind            uint8
	Name, Qualifier string
	QualKind        uint8
	File, Enclosing int
	Line, Col       int
	Args            uint8  // facts.Ref.Args: 1 + the argument count, 0 unknown
	ArgTypes        string // facts.Ref.ArgTypes
	Lambda          int    // segment ref index of the call whose lambda contains it, or -1
}

// NArgs returns the reference's argument count, if it was counted.
func (r RefRec) NArgs() (int, bool) { return int(r.Args) - 1, r.Args > 0 }

func (s *Segment) refRec(i int) []byte { return s.refs[i*refRecSize : (i+1)*refRecSize] }

// Ref returns reference i.
func (s *Segment) Ref(i int) RefRec {
	b := s.refRec(i)
	enclosing := -1
	if c := le.Uint32(b[20:]); c != noCaller {
		enclosing = int(c)
	}
	rec := RefRec{
		Kind: b[31], Name: s.str(b[0:]), Qualifier: s.str(b[8:]), QualKind: b[30],
		File: int(le.Uint32(b[16:])), Enclosing: enclosing, Line: int(le.Uint32(b[24:])), Col: int(le.Uint16(b[28:])),
		Args: b[32], ArgTypes: s.str(b[36:]), Lambda: -1,
	}
	if l := le.Uint32(b[44:]); l != noCaller {
		rec.Lambda = int(l)
	}
	return rec
}

// RefKind returns the kind of reference i without decoding it.
func (s *Segment) RefKind(i int) uint8 { return s.refRec(i)[31] }

// RefFile returns the file index of reference i without decoding it.
func (s *Segment) RefFile(i int) int { return int(le.Uint32(s.refRec(i)[16:])) }

// RefEnclosing returns the enclosing symbol index of reference i, or -1.
func (s *Segment) RefEnclosing(i int) int {
	if c := le.Uint32(s.refRec(i)[20:]); c != noCaller {
		return int(c)
	}
	return -1
}

// RefsTo returns the half-open range of reference indexes named name, of
// every kind.
func (s *Segment) RefsTo(name string) (lo, hi int) {
	lo = sort.Search(s.nRefs, func(i int) bool { return s.view(s.refRec(i)) >= name })
	hi = lo + sort.Search(s.nRefs-lo, func(i int) bool { return s.view(s.refRec(lo+i)) > name })
	return lo, hi
}

// RefsFrom returns the reference indexes inside symbol sym, in line order.
func (s *Segment) RefsFrom(sym int) []int {
	n := len(s.rbe) / 4
	at := func(k int) uint32 { return le.Uint32(s.refRec(int(le.Uint32(s.rbe[k*4:])))[20:]) }
	lo := sort.Search(n, func(k int) bool { return at(k) >= uint32(sym) })
	hi := lo + sort.Search(n-lo, func(k int) bool { return at(lo+k) > uint32(sym) })
	return s.u32Range(s.rbe, uint32(lo), uint32(hi-lo))
}

// RefsInFile returns file i's reference indexes in source order.
func (s *Segment) RefsInFile(i int) []int {
	b := s.fileRec(i)
	return s.u32Range(s.rbf, le.Uint32(b[80:]), le.Uint32(b[84:]))
}

// ImportRec is one import of a file.
type ImportRec struct {
	facts.Import
	File int
}

// Import returns import i with its listed names.
func (s *Segment) Import(i int) ImportRec {
	b := s.imps[i*importRecSize:]
	rec := ImportRec{
		Import: facts.Import{Path: s.str(b[0:]), Name: s.str(b[8:]), Line: int(le.Uint32(b[20:])), Kind: b[24]},
		File:   int(le.Uint32(b[16:])),
	}
	off, n := int(le.Uint32(b[28:])), int(le.Uint32(b[32:]))
	for k := off; k < off+n; k++ {
		nb := s.inames[k*impNameRecSize:]
		rec.Names = append(rec.Names, facts.ImportedName{Name: s.str(nb[0:]), Alias: s.str(nb[8:])})
	}
	return rec
}

// ImportsInFile returns file i's import indexes.
func (s *Segment) ImportsInFile(i int) []int {
	b := s.fileRec(i)
	off, n := int(le.Uint32(b[72:])), int(le.Uint32(b[76:]))
	out := make([]int, n)
	for k := range out {
		out[k] = off + k
	}
	return out
}

// Importers returns import indexes whose path equals importPath.
func (s *Segment) Importers(importPath string) []int {
	n := len(s.ibp) / 4
	at := func(k int) string { return s.view(s.imps[int(le.Uint32(s.ibp[k*4:]))*importRecSize:]) }
	lo := sort.Search(n, func(k int) bool { return at(k) >= importPath })
	hi := lo + sort.Search(n-lo, func(k int) bool { return at(lo+k) > importPath })
	return s.u32Range(s.ibp, uint32(lo), uint32(hi-lo))
}

// Exports returns file i's exports in line order.
func (s *Segment) Exports(i int) []facts.Export {
	b := s.fileRec(i)
	off, n := int(le.Uint32(b[96:])), int(le.Uint32(b[100:]))
	out := make([]facts.Export, 0, n)
	for k := off; k < off+n; k++ {
		r := s.exps[k*exportRecSize:]
		out = append(out, facts.Export{Name: s.str(r[0:]), Source: s.str(r[8:]), SourceName: s.str(r[16:]), Line: int(le.Uint32(r[28:]))})
	}
	return out
}

// HintRec is a binding hint. Scope is a segment symbol index or -1.
type HintRec struct {
	Name, Type string
	File       int
	Scope      int
	Line       int
}

// Hints returns file i's binding hints in line order.
func (s *Segment) Hints(i int) []HintRec {
	b := s.fileRec(i)
	off, n := int(le.Uint32(b[104:])), int(le.Uint32(b[108:]))
	out := make([]HintRec, 0, n)
	for k := off; k < off+n; k++ {
		r := s.hints[k*hintRecSize:]
		scope := -1
		if c := le.Uint32(r[20:]); c != noCaller {
			scope = int(c)
		}
		out = append(out, HintRec{Name: s.str(r[0:]), Type: s.str(r[8:]), File: int(le.Uint32(r[16:])), Scope: scope, Line: int(le.Uint32(r[24:]))})
	}
	return out
}

// File decodes file i with all of its facts. Ref.Enclosing and
// BindingHint.Scope index File.Symbols.
func (s *Segment) File(i int) *facts.File {
	m := s.FileMeta(i)
	f := &facts.File{
		Path: m.Path, Package: m.Package, PkgName: m.PkgName, Lang: m.Lang, ParseErr: m.ParseErr,
		Hash: m.Hash, Size: m.Size, MtimeNS: m.MtimeNS, Deleted: m.Deleted,
		Generated: m.Generated, Vendored: m.Vendored, Test: m.Test,
	}
	local := map[int]int{}
	parents := map[int]int{}
	for _, si := range s.SymbolsInFile(i) {
		rec := s.Symbol(si)
		local[si] = len(f.Symbols)
		if rec.Parent >= 0 {
			parents[len(f.Symbols)] = rec.Parent
		}
		f.Symbols = append(f.Symbols, rec.Symbol)
	}
	for li, parent := range parents {
		if p, ok := local[parent]; ok {
			f.Symbols[li].Container = p + 1
		}
	}
	for _, ii := range s.ImportsInFile(i) {
		f.Imports = append(f.Imports, s.Import(ii).Import)
	}
	localOf := func(seg int) int {
		if l, ok := local[seg]; ok {
			return l
		}
		return facts.NoCaller
	}
	refIdx := s.RefsInFile(i)
	localRef := make(map[int]int, len(refIdx))
	for k, ri := range refIdx {
		localRef[ri] = k
	}
	for _, ri := range refIdx {
		r := s.Ref(ri)
		ref := facts.Ref{
			Kind: r.Kind, Enclosing: localOf(r.Enclosing), Name: r.Name, Qualifier: r.Qualifier,
			QualKind: r.QualKind, Line: r.Line, Col: r.Col, Args: r.Args, ArgTypes: r.ArgTypes,
		}
		if k, ok := localRef[r.Lambda]; ok && r.Lambda >= 0 {
			ref.Lambda = k + 1
		}
		f.Refs = append(f.Refs, ref)
	}
	f.Exports = s.Exports(i)
	for _, h := range s.Hints(i) {
		f.Hints = append(f.Hints, facts.BindingHint{Scope: localOf(h.Scope), Name: h.Name, Type: h.Type, Line: h.Line})
	}
	if len(f.Exports) == 0 {
		f.Exports = nil
	}
	return f
}

func readWhole(path string) ([]byte, func() error, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return data, func() error { return nil }, nil
}
