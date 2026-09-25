package segment

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/cespare/xxhash/v2"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

type encoder struct {
	strs    []byte
	interns map[string]strRef
}

func (e *encoder) ref(s string) strRef {
	if s == "" {
		return strRef{}
	}
	if r, ok := e.interns[s]; ok {
		return r
	}
	r := strRef{uint32(len(e.strs)), uint32(len(s))}
	e.strs = append(e.strs, s...)
	e.interns[s] = r
	return r
}

type symEntry struct {
	file, local int
	sym         *facts.Symbol
}

type callEntry struct {
	file   int
	caller uint32
	call   *facts.Call
}

// Encode serializes files (in any order; duplicates by path are an error)
// into one segment image.
func Encode(files []*facts.File, kind Kind, generation uint64) ([]byte, error) {
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b *facts.File) int { return cmp.Compare(a.Path, b.Path) })
	for i := 1; i < len(sorted); i++ {
		if sorted[i].Path == sorted[i-1].Path {
			return nil, fmt.Errorf("encode segment: duplicate path %q", sorted[i].Path)
		}
	}
	kinds := make(map[string]uint8, len(facts.Kinds))
	for i, k := range facts.Kinds {
		kinds[k] = uint8(i)
	}
	e := &encoder{interns: make(map[string]strRef, 4096)}

	// Symbols: global order (name, file, line); remember local -> global.
	var syms []symEntry
	for fi, f := range sorted {
		for li := range f.Symbols {
			syms = append(syms, symEntry{fi, li, &f.Symbols[li]})
		}
	}
	slices.SortFunc(syms, func(a, b symEntry) int {
		if c := cmp.Compare(a.sym.Name, b.sym.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(a.file, b.file); c != 0 {
			return c
		}
		return cmp.Compare(a.sym.Line, b.sym.Line)
	})
	globalSym := make([][]uint32, len(sorted))
	for fi, f := range sorted {
		globalSym[fi] = make([]uint32, len(f.Symbols))
	}
	for gi, s := range syms {
		globalSym[s.file][s.local] = uint32(gi)
	}

	symSec := make([]byte, len(syms)*symbolRecSize)
	for gi, s := range syms {
		b := symSec[gi*symbolRecSize:]
		putRef(b[0:], e.ref(s.sym.Name))
		putRef(b[8:], e.ref(s.sym.Receiver))
		putRef(b[16:], e.ref(s.sym.Signature))
		putRef(b[24:], e.ref(s.sym.Doc))
		le.PutUint32(b[32:], uint32(s.file))
		le.PutUint32(b[36:], uint32(s.sym.Line))
		le.PutUint32(b[40:], uint32(s.sym.EndLine))
		k, ok := kinds[s.sym.Kind]
		if !ok {
			return nil, fmt.Errorf("encode segment: unknown symbol kind %q", s.sym.Kind)
		}
		b[44] = k
		if s.sym.Exported {
			b[45] = flagExported
		}
	}

	// Calls: global order (callee, qualifier, file, line).
	var calls []callEntry
	for fi, f := range sorted {
		for ci := range f.Calls {
			c := &f.Calls[ci]
			caller := noCaller
			if c.Caller >= 0 && c.Caller < len(f.Symbols) {
				caller = globalSym[fi][c.Caller]
			}
			calls = append(calls, callEntry{fi, caller, c})
		}
	}
	slices.SortFunc(calls, func(a, b callEntry) int {
		if c := cmp.Compare(a.call.Callee, b.call.Callee); c != 0 {
			return c
		}
		if c := cmp.Compare(a.call.Qualifier, b.call.Qualifier); c != 0 {
			return c
		}
		if c := cmp.Compare(a.file, b.file); c != 0 {
			return c
		}
		return cmp.Compare(a.call.Line, b.call.Line)
	})
	callSec := make([]byte, len(calls)*callRecSize)
	byCaller := make([]uint32, 0, len(calls))
	byFileCalls := make([][]uint32, len(sorted))
	for gi, c := range calls {
		b := callSec[gi*callRecSize:]
		putRef(b[0:], e.ref(c.call.Callee))
		putRef(b[8:], e.ref(c.call.Qualifier))
		le.PutUint32(b[16:], uint32(c.file))
		le.PutUint32(b[20:], c.caller)
		le.PutUint32(b[24:], uint32(c.call.Line))
		le.PutUint16(b[28:], uint16(min(c.call.Col, 0xFFFF)))
		b[30] = c.call.QualKind
		if c.caller != noCaller {
			byCaller = append(byCaller, uint32(gi))
		}
		byFileCalls[c.file] = append(byFileCalls[c.file], uint32(gi))
	}
	slices.SortFunc(byCaller, func(a, b uint32) int {
		ca, cb := calls[a], calls[b]
		if c := cmp.Compare(ca.caller, cb.caller); c != 0 {
			return c
		}
		return cmp.Compare(ca.call.Line, cb.call.Line)
	})

	// Imports: per-file contiguous, sorted by path within a file.
	var impSec []byte
	type impKey struct {
		path string
		idx  uint32
	}
	var impByPath []impKey
	impOff := make([]uint32, len(sorted))
	for fi, f := range sorted {
		imps := slices.Clone(f.Imports)
		slices.SortFunc(imps, func(a, b facts.Import) int { return cmp.Compare(a.Path, b.Path) })
		impOff[fi] = uint32(len(impSec) / importRecSize)
		for _, im := range imps {
			var rec [importRecSize]byte
			putRef(rec[0:], e.ref(im.Path))
			putRef(rec[8:], e.ref(im.Name))
			le.PutUint32(rec[16:], uint32(fi))
			le.PutUint32(rec[20:], uint32(im.Line))
			impByPath = append(impByPath, impKey{im.Path, uint32(len(impSec) / importRecSize)})
			impSec = append(impSec, rec[:]...)
		}
	}
	slices.SortStableFunc(impByPath, func(a, b impKey) int { return cmp.Compare(a.path, b.path) })

	// Files, with their per-file index ranges.
	fileSec := make([]byte, len(sorted)*fileRecSize)
	var symByFile, callsByFile []uint32
	for fi, f := range sorted {
		b := fileSec[fi*fileRecSize:]
		putRef(b[0:], e.ref(f.Path))
		putRef(b[8:], e.ref(f.Package))
		putRef(b[16:], e.ref(f.PkgName))
		putRef(b[24:], e.ref(f.Lang))
		putRef(b[32:], e.ref(f.ParseErr))
		le.PutUint64(b[40:], f.Hash)
		le.PutUint64(b[48:], uint64(f.Size))
		le.PutUint64(b[56:], uint64(f.MtimeNS))

		local := slices.Clone(globalSym[fi])
		slices.SortFunc(local, func(a, b uint32) int {
			if c := cmp.Compare(syms[a].sym.Line, syms[b].sym.Line); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
		le.PutUint32(b[64:], uint32(len(symByFile)))
		le.PutUint32(b[68:], uint32(len(local)))
		symByFile = append(symByFile, local...)

		le.PutUint32(b[72:], impOff[fi])
		le.PutUint32(b[76:], uint32(len(f.Imports)))

		fc := byFileCalls[fi]
		slices.SortFunc(fc, func(a, b uint32) int {
			if c := cmp.Compare(calls[a].call.Line, calls[b].call.Line); c != 0 {
				return c
			}
			return cmp.Compare(calls[a].call.Col, calls[b].call.Col)
		})
		le.PutUint32(b[80:], uint32(len(callsByFile)))
		le.PutUint32(b[84:], uint32(len(fc)))
		callsByFile = append(callsByFile, fc...)
		if f.Deleted {
			le.PutUint32(b[88:], flagDeleted)
		}
	}
	if len(e.strs) >= maxStringBytes {
		return nil, fmt.Errorf("encode segment: string table exceeds %d bytes", maxStringBytes)
	}

	impIdx := make([]uint32, len(impByPath))
	for i, k := range impByPath {
		impIdx[i] = k.idx
	}
	sections := [numSections][]byte{
		secStrings - 1:       e.strs,
		secFiles - 1:         fileSec,
		secSymbols - 1:       symSec,
		secSymByFile - 1:     u32s(symByFile),
		secImports - 1:       impSec,
		secImportsByPath - 1: u32s(impIdx),
		secCalls - 1:         callSec,
		secCallsByCaller - 1: u32s(byCaller),
		secCallsByFile - 1:   u32s(callsByFile),
	}
	return assemble(sections, kind, generation), nil
}

func u32s(v []uint32) []byte {
	b := make([]byte, len(v)*4)
	for i, x := range v {
		le.PutUint32(b[i*4:], x)
	}
	return b
}

func align8(n int) int { return (n + 7) &^ 7 }

func assemble(sections [numSections][]byte, kind Kind, generation uint64) []byte {
	size := headerSize
	offs := make([]int, numSections)
	for i, s := range sections {
		offs[i] = size
		size = align8(size + len(s))
	}
	tableOff := size
	size += numSections * sectionEntry
	out := make([]byte, size)
	copy(out, Magic[:])
	le.PutUint16(out[8:], Version)
	le.PutUint16(out[10:], uint16(kind))
	le.PutUint32(out[12:], numSections)
	le.PutUint64(out[16:], generation)
	le.PutUint64(out[24:], uint64(tableOff))
	for i, s := range sections {
		copy(out[offs[i]:], s)
		e := out[tableOff+i*sectionEntry:]
		le.PutUint32(e[0:], uint32(i+1))
		le.PutUint64(e[8:], uint64(offs[i]))
		le.PutUint64(e[16:], uint64(len(s)))
		le.PutUint64(e[24:], xxhash.Sum64(s))
	}
	le.PutUint64(out[32:], xxhash.Sum64(out[tableOff:]))
	le.PutUint64(out[40:], xxhash.Sum64(out[:40]))
	return out
}
