package segment

import (
	"cmp"
	"fmt"
	"slices"

	"math"
	"strings"

	"github.com/cespare/xxhash/v2"

	"github.com/spawn08/chronos-code/internal/indexer/facts"
	"github.com/spawn08/chronos-code/internal/indexer/terms"
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

type refEntry struct {
	file, local int
	enclosing   uint32
	ref         *facts.Ref
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
		if s.sym.Visibility >= facts.NumVisibility {
			return nil, fmt.Errorf("encode segment: unknown visibility %d", s.sym.Visibility)
		}
		b[46] = s.sym.Visibility
		container := noCaller
		if p, ok := s.sym.Parent(); ok && p < len(sorted[s.file].Symbols) && p != s.local {
			container = globalSym[s.file][p]
		}
		le.PutUint32(b[48:], container)
		le.PutUint32(b[52:], s.sym.Modifiers)
		if a := s.sym.Params; a.Known {
			if a.Min < 0 || a.Min > facts.MaxArity || a.Max > facts.MaxArity || (a.Max != facts.VarArgs && a.Max < a.Min) {
				return nil, fmt.Errorf("encode segment: invalid arity %d..%d", a.Min, a.Max)
			}
			putRef(b[64:], e.ref(s.sym.ParamList))
			b[45] |= flagArity
			b[56] = uint8(a.Min)
			b[57] = varArgs
			if a.Max != facts.VarArgs {
				b[57] = uint8(a.Max)
			}
		}
	}
	localSym := func(fi, i int) uint32 {
		if i >= 0 && i < len(sorted[fi].Symbols) {
			return globalSym[fi][i]
		}
		return noCaller
	}

	// References: global order (name, qualifier, file, line, col, kind).
	var refs []refEntry
	for fi, f := range sorted {
		for ri := range f.Refs {
			r := &f.Refs[ri]
			if r.Kind >= facts.NumRefKinds {
				return nil, fmt.Errorf("encode segment: unknown reference kind %d", r.Kind)
			}
			refs = append(refs, refEntry{fi, ri, localSym(fi, r.Enclosing), r})
		}
	}
	slices.SortFunc(refs, func(a, b refEntry) int {
		if c := cmp.Compare(a.ref.Name, b.ref.Name); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ref.Qualifier, b.ref.Qualifier); c != 0 {
			return c
		}
		if c := cmp.Compare(a.file, b.file); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ref.Line, b.ref.Line); c != 0 {
			return c
		}
		if c := cmp.Compare(a.ref.Col, b.ref.Col); c != 0 {
			return c
		}
		return cmp.Compare(a.ref.Kind, b.ref.Kind)
	})
	globalRef := make([][]uint32, len(sorted))
	for fi, f := range sorted {
		globalRef[fi] = make([]uint32, len(f.Refs))
	}
	for gi, r := range refs {
		globalRef[r.file][r.local] = uint32(gi)
	}
	refSec := make([]byte, len(refs)*refRecSize)
	byEnclosing := make([]uint32, 0, len(refs))
	byFileRefs := make([][]uint32, len(sorted))
	for gi, r := range refs {
		b := refSec[gi*refRecSize:]
		putRef(b[0:], e.ref(r.ref.Name))
		putRef(b[8:], e.ref(r.ref.Qualifier))
		le.PutUint32(b[16:], uint32(r.file))
		le.PutUint32(b[20:], r.enclosing)
		le.PutUint32(b[24:], uint32(r.ref.Line))
		le.PutUint16(b[28:], uint16(min(r.ref.Col, 0xFFFF)))
		b[30] = r.ref.QualKind
		b[31] = r.ref.Kind
		b[32] = r.ref.Args
		putRef(b[36:], e.ref(r.ref.ArgTypes))
		lambda := noCaller
		if l := r.ref.Lambda - 1; l >= 0 && l < len(globalRef[r.file]) && l != r.local {
			lambda = globalRef[r.file][l]
		}
		le.PutUint32(b[44:], lambda)
		if r.enclosing != noCaller {
			byEnclosing = append(byEnclosing, uint32(gi))
		}
		byFileRefs[r.file] = append(byFileRefs[r.file], uint32(gi))
	}
	// Ties break on the record index: a total order keeps the bytes
	// deterministic without a (slower) stable sort.
	slices.SortFunc(byEnclosing, func(a, b uint32) int {
		ra, rb := refs[a], refs[b]
		if c := cmp.Compare(ra.enclosing, rb.enclosing); c != 0 {
			return c
		}
		if c := cmp.Compare(ra.ref.Line, rb.ref.Line); c != 0 {
			return c
		}
		return cmp.Compare(a, b)
	})

	// Imports: per-file contiguous, sorted by path within a file; their
	// listed names follow in importNames.
	var impSec, impNameSec []byte
	type impKey struct {
		path string
		idx  uint32
	}
	var impByPath []impKey
	impOff := make([]uint32, len(sorted))
	for fi, f := range sorted {
		imps := slices.Clone(f.Imports)
		slices.SortStableFunc(imps, func(a, b facts.Import) int {
			if c := cmp.Compare(a.Path, b.Path); c != 0 {
				return c
			}
			return cmp.Compare(a.Line, b.Line)
		})
		impOff[fi] = uint32(len(impSec) / importRecSize)
		for _, im := range imps {
			if im.Kind >= facts.NumImportKinds {
				return nil, fmt.Errorf("encode segment: unknown import kind %d", im.Kind)
			}
			var rec [importRecSize]byte
			putRef(rec[0:], e.ref(im.Path))
			putRef(rec[8:], e.ref(im.Name))
			le.PutUint32(rec[16:], uint32(fi))
			le.PutUint32(rec[20:], uint32(im.Line))
			rec[24] = im.Kind
			le.PutUint32(rec[28:], uint32(len(impNameSec)/impNameRecSize))
			le.PutUint32(rec[32:], uint32(len(im.Names)))
			for _, n := range im.Names {
				var nr [impNameRecSize]byte
				putRef(nr[0:], e.ref(n.Name))
				putRef(nr[8:], e.ref(n.Alias))
				impNameSec = append(impNameSec, nr[:]...)
			}
			impByPath = append(impByPath, impKey{im.Path, uint32(len(impSec) / importRecSize)})
			impSec = append(impSec, rec[:]...)
		}
	}
	slices.SortStableFunc(impByPath, func(a, b impKey) int { return cmp.Compare(a.path, b.path) })

	// Exports and binding hints: per-file contiguous, in line order.
	var expSec, hintSec []byte
	expOff := make([]uint32, len(sorted))
	hintOff := make([]uint32, len(sorted))
	for fi, f := range sorted {
		exps := slices.Clone(f.Exports)
		slices.SortStableFunc(exps, func(a, b facts.Export) int { return cmp.Compare(a.Line, b.Line) })
		expOff[fi] = uint32(len(expSec) / exportRecSize)
		for _, x := range exps {
			var rec [exportRecSize]byte
			putRef(rec[0:], e.ref(x.Name))
			putRef(rec[8:], e.ref(x.Source))
			putRef(rec[16:], e.ref(x.SourceName))
			le.PutUint32(rec[24:], uint32(fi))
			le.PutUint32(rec[28:], uint32(x.Line))
			expSec = append(expSec, rec[:]...)
		}
		hints := slices.Clone(f.Hints)
		slices.SortStableFunc(hints, func(a, b facts.BindingHint) int { return cmp.Compare(a.Line, b.Line) })
		hintOff[fi] = uint32(len(hintSec) / hintRecSize)
		for _, h := range hints {
			var rec [hintRecSize]byte
			putRef(rec[0:], e.ref(h.Name))
			putRef(rec[8:], e.ref(h.Type))
			le.PutUint32(rec[16:], uint32(fi))
			le.PutUint32(rec[20:], localSym(fi, h.Scope))
			le.PutUint32(rec[24:], uint32(h.Line))
			hintSec = append(hintSec, rec[:]...)
		}
	}

	// Files, with their per-file index ranges.
	fileSec := make([]byte, len(sorted)*fileRecSize)
	var symByFile, refsByFile []uint32
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

		fr := byFileRefs[fi]
		slices.SortFunc(fr, func(a, b uint32) int {
			if c := cmp.Compare(refs[a].ref.Line, refs[b].ref.Line); c != 0 {
				return c
			}
			if c := cmp.Compare(refs[a].ref.Col, refs[b].ref.Col); c != 0 {
				return c
			}
			return cmp.Compare(a, b)
		})
		le.PutUint32(b[80:], uint32(len(refsByFile)))
		le.PutUint32(b[84:], uint32(len(fr)))
		refsByFile = append(refsByFile, fr...)
		var flags uint32
		for _, fl := range []struct {
			on  bool
			bit uint32
		}{{f.Deleted, flagDeleted}, {f.Generated, flagGenerated}, {f.Vendored, flagVendored}, {f.Test, flagTest}} {
			if fl.on {
				flags |= fl.bit
			}
		}
		le.PutUint32(b[88:], flags)
		le.PutUint32(b[96:], expOff[fi])
		le.PutUint32(b[100:], uint32(len(f.Exports)))
		le.PutUint32(b[104:], hintOff[fi])
		le.PutUint32(b[108:], uint32(len(f.Hints)))
	}
	search := encodeSearch(e, sorted, syms)
	pkgFiles := make([]uint32, len(sorted))
	for i := range pkgFiles {
		pkgFiles[i] = uint32(i)
	}
	slices.SortStableFunc(pkgFiles, func(a, b uint32) int { return cmp.Compare(sorted[a].Package, sorted[b].Package) })
	if len(e.strs) >= maxStringBytes {
		return nil, fmt.Errorf("encode segment: string table exceeds %d bytes", maxStringBytes)
	}

	impIdx := make([]uint32, len(impByPath))
	for i, k := range impByPath {
		impIdx[i] = k.idx
	}
	sections := [numSections][]byte{
		secStrings - 1:         e.strs,
		secFiles - 1:           fileSec,
		secSymbols - 1:         symSec,
		secSymByFile - 1:       u32s(symByFile),
		secImports - 1:         impSec,
		secImportsByPath - 1:   u32s(impIdx),
		secRefs - 1:            refSec,
		secRefsByEnclosing - 1: u32s(byEnclosing),
		secRefsByFile - 1:      u32s(refsByFile),
		secSearchStats - 1:     search.stats,
		secDocLen - 1:          search.docLen,
		secTerms - 1:           search.terms,
		secPostings - 1:        search.postings,
		secNames - 1:           search.names,
		secTrigrams - 1:        search.trigrams,
		secTriPost - 1:         u32s(search.triPost),
		secPkgFiles - 1:        u32s(pkgFiles),
		secImportNames - 1:     impNameSec,
		secExports - 1:         expSec,
		secHints - 1:           hintSec,
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

type searchSections struct {
	stats, docLen, terms, postings, names, trigrams []byte
	triPost                                         []uint32
}

// encodeSearch builds BM25 postings over each declaration's name,
// receiver, signature, doc, package and path, and a trigram index over the
// distinct lower-cased declaration names. syms is in global symbol order.
func encodeSearch(e *encoder, files []*facts.File, syms []symEntry) searchSections {
	var out searchSections
	post := map[string][]byte{}
	out.docLen = make([]byte, len(syms)*4)
	fileTerms := make([]map[string]float32, len(files))
	tf := map[string]float32{}
	var nDocs uint64
	var sumLen float64
	var names []string
	for gi, se := range syms {
		sym := se.sym
		if sym.Kind == facts.KindEmbed {
			continue
		}
		clear(tf)
		terms.Add(tf, sym.Name, terms.WeightName)
		terms.Add(tf, sym.Receiver, terms.WeightOther)
		terms.Add(tf, sym.Signature, terms.WeightOther)
		terms.Add(tf, sym.Doc, terms.WeightOther)
		if fileTerms[se.file] == nil {
			ft := map[string]float32{}
			terms.Add(ft, files[se.file].Package, terms.WeightOther)
			terms.Add(ft, files[se.file].Path, terms.WeightOther)
			fileTerms[se.file] = ft
		}
		for t, n := range fileTerms[se.file] {
			tf[t] += n
		}
		var n float32
		for t, w := range tf {
			var rec [postingRecSize]byte
			le.PutUint32(rec[0:], uint32(gi))
			le.PutUint32(rec[4:], math.Float32bits(w))
			post[t] = append(post[t], rec[:]...)
			n += w
		}
		le.PutUint32(out.docLen[gi*4:], math.Float32bits(n))
		nDocs++
		sumLen += float64(n)
		if len(names) == 0 || names[len(names)-1] != sym.Name {
			names = append(names, sym.Name)
		}
	}
	out.stats = make([]byte, 16)
	le.PutUint64(out.stats[0:], nDocs)
	le.PutUint64(out.stats[8:], math.Float64bits(sumLen))

	keys := make([]string, 0, len(post))
	for t := range post {
		keys = append(keys, t)
	}
	slices.Sort(keys)
	out.terms = make([]byte, len(keys)*termRecSize)
	for i, t := range keys {
		b := out.terms[i*termRecSize:]
		putRef(b[0:], e.ref(t))
		le.PutUint32(b[8:], uint32(len(out.postings)/postingRecSize))
		le.PutUint32(b[12:], uint32(len(post[t])/postingRecSize))
		out.postings = append(out.postings, post[t]...)
	}

	tri := map[string][]uint32{}
	out.names = make([]byte, len(names)*nameRecSize)
	for i, n := range names {
		lower := strings.ToLower(n)
		putRef(out.names[i*nameRecSize:], e.ref(n))
		putRef(out.names[i*nameRecSize+8:], e.ref(lower))
		for _, g := range terms.Trigrams(lower) {
			if l := tri[g]; len(l) == 0 || l[len(l)-1] != uint32(i) {
				tri[g] = append(l, uint32(i))
			}
		}
	}
	grams := make([]string, 0, len(tri))
	for g := range tri {
		grams = append(grams, g)
	}
	slices.Sort(grams)
	out.trigrams = make([]byte, len(grams)*termRecSize)
	for i, g := range grams {
		b := out.trigrams[i*termRecSize:]
		putRef(b[0:], e.ref(g))
		le.PutUint32(b[8:], uint32(len(out.triPost)))
		le.PutUint32(b[12:], uint32(len(tri[g])))
		out.triPost = append(out.triPost, tri[g]...)
	}
	return out
}
