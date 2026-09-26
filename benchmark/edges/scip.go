package main

import (
	"fmt"
	"os"

	"google.golang.org/protobuf/encoding/protowire"
)

// The subset of the SCIP index format (github.com/sourcegraph/scip,
// scip.proto) the edge evaluation needs.

// Symbol roles (Occurrence.symbol_roles).
const (
	roleDefinition = 0x1
	roleImport     = 0x2
)

// Position encodings (Document.position_encoding).
const (
	encodingUnspecified = 0
	encodingUTF8        = 1
	encodingUTF16       = 2
	encodingUTF32       = 3
)

type scipIndex struct {
	Documents []scipDocument
}

type scipDocument struct {
	Path        string
	Language    string
	Encoding    int
	Occurrences []scipOccurrence
}

type scipOccurrence struct {
	StartLine, StartChar, EndLine, EndChar int // 0-based
	Symbol                                 string
	Roles                                  int
}

func readSCIP(path string) (*scipIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scip index: %w", err)
	}
	idx := &scipIndex{}
	err = eachField(data, func(num protowire.Number, typ protowire.Type, v []byte) error {
		if num == 2 && typ == protowire.BytesType {
			doc, err := parseDocument(v)
			if err != nil {
				return err
			}
			idx.Documents = append(idx.Documents, doc)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse scip index %s: %w", path, err)
	}
	return idx, nil
}

func parseDocument(b []byte) (scipDocument, error) {
	var d scipDocument
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType:
			d.Path = string(v)
		case num == 4 && typ == protowire.BytesType:
			d.Language = string(v)
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

func parseOccurrence(b []byte) (scipOccurrence, error) {
	var o scipOccurrence
	var rng []int
	err := eachField(b, func(num protowire.Number, typ protowire.Type, v []byte) error {
		switch {
		case num == 1 && typ == protowire.BytesType: // packed int32
			for len(v) > 0 {
				n, k := protowire.ConsumeVarint(v)
				if k < 0 {
					return fmt.Errorf("bad range")
				}
				rng = append(rng, int(int32(n)))
				v = v[k:]
			}
		case num == 1 && typ == protowire.VarintType: // unpacked
			n, _ := protowire.ConsumeVarint(v)
			rng = append(rng, int(int32(n)))
		case num == 2 && typ == protowire.BytesType:
			o.Symbol = string(v)
		case num == 3 && typ == protowire.VarintType:
			n, _ := protowire.ConsumeVarint(v)
			o.Roles = int(n)
		}
		return nil
	})
	switch len(rng) {
	case 3:
		o.StartLine, o.StartChar, o.EndLine, o.EndChar = rng[0], rng[1], rng[0], rng[2]
	case 4:
		o.StartLine, o.StartChar, o.EndLine, o.EndChar = rng[0], rng[1], rng[2], rng[3]
	default:
		return o, fmt.Errorf("occurrence range has %d elements", len(rng))
	}
	return o, err
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
