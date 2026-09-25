package segment

import (
	"bytes"
	"fmt"
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
	calls  []byte
	cbc    []byte
	cbf    []byte
	nFiles int
	nSyms  int
	nCalls int
	nImps  int
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
	s.imps, s.ibp, s.calls, s.cbc, s.cbf = secs[secImports-1], secs[secImportsByPath-1], secs[secCalls-1], secs[secCallsByCaller-1], secs[secCallsByFile-1]
	if err := s.validate(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Segment) validate() error {
	for name, pair := range map[string][2]int{
		"files": {len(s.files), fileRecSize}, "symbols": {len(s.syms), symbolRecSize},
		"imports": {len(s.imps), importRecSize}, "calls": {len(s.calls), callRecSize},
		"symByFile": {len(s.sbf), 4}, "importsByPath": {len(s.ibp), 4},
		"callsByCaller": {len(s.cbc), 4}, "callsByFile": {len(s.cbf), 4},
	} {
		if pair[0]%pair[1] != 0 {
			return corrupt("%s size", name)
		}
	}
	s.nFiles, s.nSyms = len(s.files)/fileRecSize, len(s.syms)/symbolRecSize
	s.nImps, s.nCalls = len(s.imps)/importRecSize, len(s.calls)/callRecSize
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
			!okRange(le.Uint32(b[80:]), le.Uint32(b[84:]), len(s.cbf)/4) {
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
		if int(le.Uint32(b[32:])) >= s.nFiles || int(b[44]) >= len(facts.Kinds) {
			return corrupt("symbol %d fields", i)
		}
	}
	for i := 0; i < s.nImps; i++ {
		b := s.imps[i*importRecSize:]
		if !okRef(b[0:]) || !okRef(b[8:]) || int(le.Uint32(b[16:])) >= s.nFiles {
			return corrupt("import %d", i)
		}
	}
	for i := 0; i < s.nCalls; i++ {
		b := s.calls[i*callRecSize:]
		c := le.Uint32(b[20:])
		if !okRef(b[0:]) || !okRef(b[8:]) || int(le.Uint32(b[16:])) >= s.nFiles || (c != noCaller && int(c) >= s.nSyms) {
			return corrupt("call %d", i)
		}
	}
	for _, idx := range []struct {
		b     []byte
		limit int
	}{{s.sbf, s.nSyms}, {s.ibp, s.nImps}, {s.cbc, s.nCalls}, {s.cbf, s.nCalls}} {
		for o := 0; o < len(idx.b); o += 4 {
			if int(le.Uint32(idx.b[o:])) >= idx.limit {
				return corrupt("index entry out of range")
			}
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

// NumFiles, NumSymbols and NumCalls return record counts.
func (s *Segment) NumFiles() int   { return s.nFiles }
func (s *Segment) NumSymbols() int { return s.nSyms }
func (s *Segment) NumCalls() int   { return s.nCalls }

// FileMeta is a file record without its facts.
type FileMeta struct {
	Path, Package, PkgName, Lang, ParseErr string
	Hash                                   uint64
	Size, MtimeNS                          int64
	Deleted                                bool
}

func (s *Segment) fileRec(i int) []byte { return s.files[i*fileRecSize : (i+1)*fileRecSize] }

// FilePath returns the path of file i.
func (s *Segment) FilePath(i int) string { return s.str(s.fileRec(i)) }

// FileMeta returns file i's metadata.
func (s *Segment) FileMeta(i int) FileMeta {
	b := s.fileRec(i)
	return FileMeta{
		Path: s.str(b[0:]), Package: s.str(b[8:]), PkgName: s.str(b[16:]), Lang: s.str(b[24:]),
		ParseErr: s.str(b[32:]), Hash: le.Uint64(b[40:]), Size: int64(le.Uint64(b[48:])),
		MtimeNS: int64(le.Uint64(b[56:])), Deleted: le.Uint32(b[88:])&flagDeleted != 0,
	}
}

// FindFile returns the index of path, if present.
func (s *Segment) FindFile(path string) (int, bool) {
	i := sort.Search(s.nFiles, func(i int) bool { return s.view(s.fileRec(i)) >= path })
	return i, i < s.nFiles && s.view(s.fileRec(i)) == path
}

// SymbolRec is a symbol plus the segment file index that declares it.
type SymbolRec struct {
	facts.Symbol
	File int
}

func (s *Segment) symRec(i int) []byte { return s.syms[i*symbolRecSize : (i+1)*symbolRecSize] }

// Symbol returns symbol i.
func (s *Segment) Symbol(i int) SymbolRec {
	b := s.symRec(i)
	return SymbolRec{
		Symbol: facts.Symbol{
			Name: s.str(b[0:]), Receiver: s.str(b[8:]), Signature: s.str(b[16:]), Doc: s.str(b[24:]),
			Line: int(le.Uint32(b[36:])), EndLine: int(le.Uint32(b[40:])), Kind: facts.Kinds[b[44]],
			Exported: b[45]&flagExported != 0,
		},
		File: int(le.Uint32(b[32:])),
	}
}

// SymbolFile returns the file index of symbol i without decoding it.
func (s *Segment) SymbolFile(i int) int { return int(le.Uint32(s.symRec(i)[32:])) }

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

// CallRec is a call site. Caller is a segment symbol index or -1.
type CallRec struct {
	Callee, Qualifier string
	QualKind          uint8
	File, Caller      int
	Line, Col         int
}

func (s *Segment) callRec(i int) []byte { return s.calls[i*callRecSize : (i+1)*callRecSize] }

// Call returns call i.
func (s *Segment) Call(i int) CallRec {
	b := s.callRec(i)
	caller := -1
	if c := le.Uint32(b[20:]); c != noCaller {
		caller = int(c)
	}
	return CallRec{
		Callee: s.str(b[0:]), Qualifier: s.str(b[8:]), QualKind: b[30],
		File: int(le.Uint32(b[16:])), Caller: caller, Line: int(le.Uint32(b[24:])), Col: int(le.Uint16(b[28:])),
	}
}

// CallFile returns the file index of call i without decoding it.
func (s *Segment) CallFile(i int) int { return int(le.Uint32(s.callRec(i)[16:])) }

// CallsTo returns the half-open range of call indexes whose callee is name.
func (s *Segment) CallsTo(name string) (lo, hi int) {
	lo = sort.Search(s.nCalls, func(i int) bool { return s.view(s.callRec(i)) >= name })
	hi = lo + sort.Search(s.nCalls-lo, func(i int) bool { return s.view(s.callRec(lo+i)) > name })
	return lo, hi
}

// CallsFrom returns the call indexes made by symbol sym, in line order.
func (s *Segment) CallsFrom(sym int) []int {
	n := len(s.cbc) / 4
	at := func(k int) uint32 { return le.Uint32(s.callRec(int(le.Uint32(s.cbc[k*4:])))[20:]) }
	lo := sort.Search(n, func(k int) bool { return at(k) >= uint32(sym) })
	hi := lo + sort.Search(n-lo, func(k int) bool { return at(lo+k) > uint32(sym) })
	return s.u32Range(s.cbc, uint32(lo), uint32(hi-lo))
}

// CallsInFile returns file i's call indexes in source order.
func (s *Segment) CallsInFile(i int) []int {
	b := s.fileRec(i)
	return s.u32Range(s.cbf, le.Uint32(b[80:]), le.Uint32(b[84:]))
}

// ImportRec is one import of a file.
type ImportRec struct {
	facts.Import
	File int
}

// Import returns import i.
func (s *Segment) Import(i int) ImportRec {
	b := s.imps[i*importRecSize:]
	return ImportRec{
		Import: facts.Import{Path: s.str(b[0:]), Name: s.str(b[8:]), Line: int(le.Uint32(b[20:]))},
		File:   int(le.Uint32(b[16:])),
	}
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

// File decodes file i with all of its facts. Call.Caller indexes File.Symbols.
func (s *Segment) File(i int) *facts.File {
	m := s.FileMeta(i)
	f := &facts.File{
		Path: m.Path, Package: m.Package, PkgName: m.PkgName, Lang: m.Lang, ParseErr: m.ParseErr,
		Hash: m.Hash, Size: m.Size, MtimeNS: m.MtimeNS, Deleted: m.Deleted,
	}
	local := map[int]int{}
	for _, si := range s.SymbolsInFile(i) {
		local[si] = len(f.Symbols)
		f.Symbols = append(f.Symbols, s.Symbol(si).Symbol)
	}
	for _, ii := range s.ImportsInFile(i) {
		f.Imports = append(f.Imports, s.Import(ii).Import)
	}
	for _, ci := range s.CallsInFile(i) {
		c := s.Call(ci)
		caller := facts.NoCaller
		if c.Caller >= 0 {
			caller = local[c.Caller]
		}
		f.Calls = append(f.Calls, facts.Call{
			Caller: caller, Callee: c.Callee, Qualifier: c.Qualifier, QualKind: c.QualKind, Line: c.Line, Col: c.Col,
		})
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
