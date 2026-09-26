// Package sciptest writes SCIP indexes for tests.
package sciptest

import (
	"os"
	"strings"
	"unicode/utf16"

	"google.golang.org/protobuf/encoding/protowire"
)

// Occurrence is one occurrence to encode (0-based line, UTF-16 columns).
type Occurrence struct {
	Line, Start, End int
	Symbol           string
	Roles            int
}

// Document is one document to encode.
type Document struct {
	Path        string
	Language    string
	Text        string // embedded when HasText
	HasText     bool
	Encoding    int
	Occurrences []Occurrence
}

// At returns the occurrence of the nth (0-based) match of word in src.
func At(src, word string, nth int, symbol string, roles int) Occurrence {
	off := -1
	for i := 0; i <= nth; i++ {
		j := strings.Index(src[off+1:], word)
		if j < 0 {
			panic("sciptest: " + word + " not found")
		}
		off += 1 + j
	}
	line := strings.Count(src[:off], "\n")
	start := strings.LastIndexByte(src[:off], '\n') + 1
	col := len(utf16.Encode([]rune(src[start:off])))
	return Occurrence{Line: line, Start: col, End: col + len(utf16.Encode([]rune(word))), Symbol: symbol, Roles: roles}
}

// Encode returns an index with the given project root and documents.
func Encode(projectRoot string, docs ...Document) []byte {
	var meta []byte
	meta = protowire.AppendTag(meta, 3, protowire.BytesType)
	meta = protowire.AppendString(meta, projectRoot)
	var b []byte
	b = protowire.AppendTag(b, 1, protowire.BytesType)
	b = protowire.AppendBytes(b, meta)
	for _, d := range docs {
		var db []byte
		db = protowire.AppendTag(db, 1, protowire.BytesType)
		db = protowire.AppendString(db, d.Path)
		db = protowire.AppendTag(db, 4, protowire.BytesType)
		db = protowire.AppendString(db, d.Language)
		if d.HasText {
			db = protowire.AppendTag(db, 5, protowire.BytesType)
			db = protowire.AppendString(db, d.Text)
		}
		if d.Encoding != 0 {
			db = protowire.AppendTag(db, 6, protowire.VarintType)
			db = protowire.AppendVarint(db, uint64(d.Encoding))
		}
		for _, o := range d.Occurrences {
			var rng []byte
			rng = protowire.AppendVarint(rng, uint64(o.Line))
			rng = protowire.AppendVarint(rng, uint64(o.Start))
			rng = protowire.AppendVarint(rng, uint64(o.End)) // same line: 3 elements
			var ob []byte
			ob = protowire.AppendTag(ob, 1, protowire.BytesType)
			ob = protowire.AppendBytes(ob, rng)
			ob = protowire.AppendTag(ob, 2, protowire.BytesType)
			ob = protowire.AppendString(ob, o.Symbol)
			if o.Roles != 0 {
				ob = protowire.AppendTag(ob, 3, protowire.VarintType)
				ob = protowire.AppendVarint(ob, uint64(o.Roles))
			}
			db = protowire.AppendTag(db, 2, protowire.BytesType)
			db = protowire.AppendBytes(db, ob)
		}
		b = protowire.AppendTag(b, 2, protowire.BytesType)
		b = protowire.AppendBytes(b, db)
	}
	return b
}

// Write writes an index to name.
func Write(name, projectRoot string, docs ...Document) error {
	return os.WriteFile(name, Encode(projectRoot, docs...), 0o644)
}
