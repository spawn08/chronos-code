// Package segment implements the immutable on-disk index segment: a header,
// a checksummed section table, and sections of fixed-width little-endian
// records that point into a shared string table. Lookups are binary searches
// over sorted record arrays, so opening a segment builds no heap index.
//
// Every section checksum and every cross-reference (string offsets, file,
// symbol and call indexes) is validated once in Parse; accessors rely on that.
package segment

import (
	"encoding/binary"
	"errors"
)

// Magic identifies a segment file; the last byte is the major format version.
var Magic = [8]byte{'C', 'H', 'X', 'S', 'E', 'G', 0, 1}

// Version is the current format version. v2 added the symbol container.
const Version = 2

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
	fileRecSize    = 96
	symbolRecSize  = 56
	importRecSize  = 24
	callRecSize    = 32
	strRefSize     = 8
	noCaller       = ^uint32(0)
	flagDeleted    = 1
	flagExported   = 1
	maxStringBytes = 1 << 31
)

// Section identifiers, in on-disk order.
const (
	secStrings = iota + 1
	secFiles
	secSymbols
	secSymByFile
	secImports
	secImportsByPath
	secCalls
	secCallsByCaller
	secCallsByFile
	numSections = secCallsByFile
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
//	 0 path    8 package  16 pkgName  24 lang  32 parseErr   (strRefs)
//	40 hash u64  48 size i64  56 mtimeNS i64
//	64 symOff u32  68 symN u32   (into symByFile)
//	72 impOff u32  76 impN u32   (into imports; per-file contiguous)
//	80 callOff u32 84 callN u32  (into callsByFile)
//	88 flags u32   92 reserved
//
// Symbol record (symbolRecSize): 0 name 8 receiver 16 signature 24 doc,
// 32 file u32, 36 line u32, 40 endLine u32, 44 kind u8, 45 flags u8,
// 48 container u32 (segment symbol index of the enclosing symbol, in the
// same file, or noCaller), 52 reserved.
//
// Header: 0 magic, 8 version u16, 10 kind u16, 12 sections u32,
// 16 generation u64, 24 table offset u64, 32 table xxh64, 40 xxh64 of bytes 0-39.
//
// Import record (importRecSize): 0 path 8 name, 16 file u32, 20 line u32.
//
// Call record (callRecSize): 0 callee 8 qualifier, 16 file u32,
// 20 caller u32 (segment symbol index or noCaller), 24 line u32,
// 28 col u16, 30 qualKind u8.
