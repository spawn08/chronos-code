// Package segment implements the immutable on-disk index segment: a header,
// a checksummed section table, and sections of fixed-width little-endian
// records that point into a shared string table. Lookups are binary searches
// over sorted record arrays, so opening a segment builds no heap index.
//
// Every section checksum and every cross-reference (string offsets, file,
// symbol and reference indexes) is validated once in Parse; accessors rely
// on that.
package segment

import (
	"encoding/binary"
	"errors"
)

// Magic identifies a segment file; the last byte is the major format version.
var Magic = [8]byte{'C', 'H', 'X', 'S', 'E', 'G', 0, 1}

// Version is the current format version. v2 added the symbol container;
// v3 the search (terms, postings, names, trigrams) and package sections;
// v4 language-neutral facts: symbol visibility and modifiers, reference
// kinds, file flags, import kinds and names, exports and binding hints;
// v5 symbol arities, reference argument counts and argument type hints.
const Version = 5

// Kind classifies a segment.
type Kind uint16

// Segment kinds.
const (
	KindBase    Kind = 1 // full snapshot of all live files
	KindOverlay Kind = 2 // changed files and tombstones on top of older segments
)

// ErrCorrupt reports a segment that fails validation. Callers rebuild.
var ErrCorrupt = errors.New("index segment corrupt")

const (
	headerSize     = 64
	sectionEntry   = 32
	fileRecSize    = 112
	symbolRecSize  = 72
	importRecSize  = 40
	impNameRecSize = 16
	refRecSize     = 48
	exportRecSize  = 32
	hintRecSize    = 32
	strRefSize     = 8
	noCaller       = ^uint32(0)
	maxStringBytes = 1 << 31
)

// File flags.
const (
	flagDeleted   = 1
	flagGenerated = 2
	flagVendored  = 4
	flagTest      = 8
)

// Symbol flags.
const (
	flagExported = 1
	flagArity    = 2 // the arity bytes are set
)

// varArgs is the stored maximum arity of a variadic declaration.
const varArgs = 0xFF

// Section identifiers, in on-disk order.
const (
	secStrings = iota + 1
	secFiles
	secSymbols
	secSymByFile
	secImports
	secImportsByPath
	secRefs
	secRefsByEnclosing
	secRefsByFile
	secSearchStats // nDocs u64, sumLen f64
	secDocLen      // f32 per symbol (0 for embeds)
	secTerms       // sorted: term strRef, postings off u32, n u32
	secPostings    // symbol u32, tf f32; per term in symbol order
	secNames       // distinct declaration names, sorted: name strRef, lower strRef
	secTrigrams    // sorted: trigram strRef, off u32, n u32 (into triPost)
	secTriPost     // name indexes u32
	secPkgFiles    // file indexes u32 sorted by (package, path)
	secImportNames // imported names: name strRef, alias strRef
	secExports     // per-file contiguous, in line order
	secHints       // per-file contiguous, in line order
	numSections    = secHints
)

const (
	termRecSize    = 16
	postingRecSize = 8
	nameRecSize    = 16
)

var le = binary.LittleEndian

// strRef addresses bytes in the string section.
type strRef struct{ off, n uint32 }

func putRef(b []byte, r strRef) {
	le.PutUint32(b, r.off)
	le.PutUint32(b[4:], r.n)
}

func getRef(b []byte) strRef { return strRef{le.Uint32(b), le.Uint32(b[4:])} }

// File record layout (fileRecSize bytes):
//
//	  0 path    8 package  16 pkgName  24 lang  32 parseErr   (strRefs)
//	 40 hash u64  48 size i64  56 mtimeNS i64
//	 64 symOff u32   68 symN u32   (into symByFile)
//	 72 impOff u32   76 impN u32   (into imports; per-file contiguous)
//	 80 refOff u32   84 refN u32   (into refsByFile)
//	 88 flags u32    92 reserved
//	 96 expOff u32  100 expN u32   (into exports; per-file contiguous)
//	104 hintOff u32 108 hintN u32  (into hints; per-file contiguous)
//
// Symbol record (symbolRecSize): 0 name 8 receiver 16 signature 24 doc,
// 32 file u32, 36 line u32, 40 endLine u32, 44 kind u8, 45 flags u8,
// 46 visibility u8, 47 reserved, 48 container u32 (segment symbol index of
// the enclosing symbol, in the same file, or noCaller), 52 modifiers u32,
// 56 minArgs u8, 57 maxArgs u8 (varArgs when variadic; both valid only
// with flagArity), 58 reserved, 64 paramList strRef.
//
// Header: 0 magic, 8 version u16, 10 kind u16, 12 sections u32,
// 16 generation u64, 24 table offset u64, 32 table xxh64, 40 xxh64 of bytes 0-39.
//
// Import record (importRecSize): 0 path 8 name, 16 file u32, 20 line u32,
// 24 kind u8, 25 reserved, 28 namesOff u32, 32 namesN u32 (into
// importNames), 36 reserved.
//
// Ref record (refRecSize): 0 name 8 qualifier, 16 file u32,
// 20 enclosing u32 (segment symbol index or noCaller), 24 line u32,
// 28 col u16, 30 qualKind u8, 31 kind u8, 32 args u8 (facts.Ref.Args),
// 33 reserved, 36 argTypes strRef, 44 lambda u32 (segment ref index of
// the call whose lambda contains this ref, in the same file, or noCaller).
//
// Export record (exportRecSize): 0 name 8 source 16 sourceName, 24 file u32,
// 28 line u32.
//
// Hint record (hintRecSize): 0 name 8 type, 16 file u32, 20 scope u32
// (segment symbol index or noCaller), 24 line u32, 28 reserved.
