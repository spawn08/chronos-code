// Package scip is the chronos indexer's precise tier for languages other
// than Go (M10). It reads SCIP indexes (github.com/sourcegraph/scip,
// scip.proto) produced by per-language indexers (scip-typescript,
// scip-python, scip-java, scip-clang, scip-dotnet, rust-analyzer, ...)
// and turns them into the same hash-gated facts the Go tier records
// (package precise): for each call or type reference site, the
// declaration it refers to. See Import.
package scip

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"google.golang.org/protobuf/encoding/protowire"
)

// Symbol roles (Occurrence.symbol_roles).
const (
	RoleDefinition        = 0x1
	RoleImport            = 0x2
	RoleForwardDefinition = 0x40
)

// Position encodings (Document.position_encoding).
const (
	EncodingUnspecified = 0 // treated as UTF-16, as scip.proto recommends
	EncodingUTF8        = 1
	EncodingUTF16       = 2
	EncodingUTF32       = 3
)

// Metadata is the subset of an index's metadata the import uses.
type Metadata struct {
	ProjectRoot string // URI, usually file:///...
	Tool        string // tool_info.name
	ToolVersion string
}

// Document is one indexed file.
type Document struct {
	Path        string // relative to the project root, slash-separated
	Language    string
	Encoding    int
	Text        []byte // the indexed text, when the indexer embedded it (rare)
	HasText     bool
	Occurrences []Occurrence
}

// Occurrence is one range of a document and the symbol it names.
type Occurrence struct {
	StartLine, StartChar, EndLine, EndChar int // 0-based, in the document's encoding
	Symbol                                 string
	Roles                                  int
}

// maxMessage bounds one top-level message (a document) in an index.
const maxMessage = 1 << 30

// ReadFile streams the index at path: meta is called with the metadata
// (if it is present, before any document in practice) and doc with each
// document in order. Either may be nil.
func ReadFile(path string, meta func(Metadata), doc func(Document) error) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("scip: %w", err)
	}
	defer f.Close()
	if err := Read(f, meta, doc); err != nil {
		return fmt.Errorf("scip: %s: %w", path, err)
	}
	return nil
}

// Read streams an index from r; see ReadFile. Only one document is held
// in memory at a time.
func Read(r io.Reader, meta func(Metadata), doc func(Document) error) error {
	br := bufio.NewReaderSize(r, 1<<16)
	var buf []byte
	for {
		tag, err := binary.ReadUvarint(br)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		num, typ := protowire.Number(tag>>3), protowire.Type(tag&7)
		switch typ {
		case protowire.VarintType:
			if _, err := binary.ReadUvarint(br); err != nil {
				return unexpected(err)
			}
			continue
		case protowire.Fixed32Type, protowire.Fixed64Type:
			n := 4
			if typ == protowire.Fixed64Type {
				n = 8
			}
			if _, err := br.Discard(n); err != nil {
				return unexpected(err)
			}
			continue
		case protowire.BytesType:
		default:
			return fmt.Errorf("unsupported wire type %d", typ)
		}
		size, err := binary.ReadUvarint(br)
		if err != nil {
			return unexpected(err)
		}
		if size > maxMessage {
			return fmt.Errorf("message of %d bytes", size)
		}
		wanted := (num == 1 && meta != nil) || (num == 2 && doc != nil)
		if !wanted {
			if _, err := br.Discard(int(size)); err != nil {
				return unexpected(err)
			}
			continue
		}
		if cap(buf) < int(size) {
			buf = make([]byte, size)
		}
		b := buf[:size]
		if _, err := io.ReadFull(br, b); err != nil {
			return unexpected(err)
		}
		if num == 1 {
			m, err := parseMetadata(b)
			if err != nil {
				return err
			}
			meta(m)
			continue
		}
		d, err := parseDocument(b)
		if err != nil {
			return err
		}
		if err := doc(d); err != nil {
			return err
		}
	}
}

func unexpected(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func parseMetadata(b []byte) (Metadata, error) {
	var m Metadata
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte) error {
		switch {
		case num == 3 && typ == protowire.BytesType:
			m.ProjectRoot = string(v)
		case num == 2 && typ == protowire.BytesType:
			return eachField(v, func(num protowire.Number, typ protowire.Type, v []byte) error {
				switch {
				case num == 1 && typ == protowire.BytesType:
					m.Tool = string(v)
				case num == 2 && typ == protowire.BytesType:
					m.ToolVersion = string(v)
				}
				return nil
			})
		}
		return nil
	})
	return m, err
}

func parseDocument(b []byte) (Document, error) {
	var d Document
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			d.Path = string(v)
		case num == 4 && typ == protowire.BytesType:
			d.Language = string(v)
		case num == 5 && typ == protowire.BytesType:
			d.Text, d.HasText = append([]byte(nil), v...), true
		case num == 6 && typ == protowire.VarintType:
			n, _ := protowire.ConsumeVarint(v)
			d.Encoding = int(n)
		case num == 2 && typ == protowire.BytesType:
			o, err := parseOccurrence(v)
			if err != nil {
				return err
			}
			d.Occurrences = append(d.Occurrences, o)
		}
		return nil
	})
	return d, err
}

func parseOccurrence(b []byte) (Occurrence, error) {
	var o Occurrence
	var rng [4]int
	n := 0
	add := func(x uint64) error {
		if n == len(rng) {
			return errors.New("occurrence range has more than 4 elements")
		}
		rng[n] = int(int32(x))
		n++
		return nil
	}
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType: // packed int32
			for len(v) > 0 {
				x, k := protowire.ConsumeVarint(v)
				if k < 0 {
					return errors.New("bad occurrence range")
				}
				if err := add(x); err != nil {
					return err
				}
				v = v[k:]
			}
		case num == 1 && typ == protowire.VarintType: // unpacked
			x, _ := protowire.ConsumeVarint(v)
			return add(x)
		case num == 2 && typ == protowire.BytesType:
			o.Symbol = string(v)
		case num == 3 && typ == protowire.VarintType:
			x, _ := protowire.ConsumeVarint(v)
			o.Roles = int(x)
		}
		return nil
	})
	if err != nil {
		return o, err
	}
	switch n {
	case 3:
		o.StartLine, o.StartChar, o.EndLine, o.EndChar = rng[0], rng[1], rng[0], rng[2]
	case 4:
		o.StartLine, o.StartChar, o.EndLine, o.EndChar = rng[0], rng[1], rng[2], rng[3]
	default:
		return o, fmt.Errorf("occurrence range has %d elements", n)
	}
	return o, nil
}

// eachField calls fn for every top-level field of a protobuf message; v
// holds the field's payload (the varint bytes for varints).
func eachField(b []byte, fn func(protowire.Number, protowire.Type, []byte) error) error {
	for len(b) > 0 {
		num, typ, n := protowire.ConsumeTag(b)
		if n < 0 {
			return protowire.ParseError(n)
		}
		b = b[n:]
		var v []byte
		switch typ {
		case protowire.VarintType:
			_, m := protowire.ConsumeVarint(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			v, n = b[:m], m
		case protowire.BytesType:
			val, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			v, n = val, m
		default:
			m := protowire.ConsumeFieldValue(num, typ, b)
			if m < 0 {
				return protowire.ParseError(m)
			}
			n = m
		}
		if err := fn(num, typ, v); err != nil {
			return err
		}
		b = b[n:]
	}
	return nil
}
