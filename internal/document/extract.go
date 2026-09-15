// Package document extracts bounded text from common office documents.
package document

import (
	"archive/zip"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"rsc.io/pdf"
)

const maxSourceBytes int64 = 32 << 20

// Supported reports whether path has a format handled by Extract.
func Supported(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".pdf", ".docx":
		return true
	default:
		return false
	}
}

// Extract returns UTF-8 document text bounded to limit bytes. The source file
// is limited independently so a malicious archive cannot consume unbounded IO.
func Extract(ctx context.Context, path string, limit int) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.Size() > maxSourceBytes {
		return "", fmt.Errorf("document exceeds %d MiB extraction limit", maxSourceBytes>>20)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".docx":
		return extractDOCX(ctx, path, limit)
	case ".pdf":
		return extractPDF(ctx, path, limit)
	default:
		return "", fmt.Errorf("unsupported document format")
	}
}

func extractDOCX(ctx context.Context, path string, limit int) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", fmt.Errorf("open DOCX: %w", err)
	}
	defer zr.Close()
	for _, file := range zr.File {
		if file.Name != "word/document.xml" {
			continue
		}
		if file.UncompressedSize64 > uint64(maxSourceBytes) {
			return "", fmt.Errorf("DOCX document XML exceeds %d MiB extraction limit", maxSourceBytes>>20)
		}
		r, err := file.Open()
		if err != nil {
			return "", fmt.Errorf("open DOCX document XML: %w", err)
		}
		defer r.Close()
		return extractDOCXXML(ctx, io.LimitReader(r, maxSourceBytes+1), limit)
	}
	return "", fmt.Errorf("DOCX has no word/document.xml")
}

func extractDOCXXML(ctx context.Context, r io.Reader, limit int) (string, error) {
	decoder := xml.NewDecoder(r)
	var out strings.Builder
	paragraph := false
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("parse DOCX XML: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "p":
				if paragraph && out.Len() > 0 {
					out.WriteByte('\n')
				}
				paragraph = true
			case "tab":
				out.WriteByte('\t')
			case "br", "cr":
				out.WriteByte('\n')
			case "t":
				var text string
				if err := decoder.DecodeElement(&text, &value); err != nil {
					return "", fmt.Errorf("decode DOCX text: %w", err)
				}
				writeBounded(&out, text, limit)
			}
		}
		if limit > 0 && out.Len() >= limit {
			break
		}
	}
	return out.String(), nil
}

func extractPDF(ctx context.Context, path string, limit int) (string, error) {
	r, err := pdf.Open(path)
	if err != nil {
		return "", fmt.Errorf("open PDF: %w", err)
	}
	var out strings.Builder
	for page := 1; page <= r.NumPage(); page++ {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if page > 1 && out.Len() > 0 {
			out.WriteByte('\n')
		}
		for _, text := range r.Page(page).Content().Text {
			writeBounded(&out, text.S, limit)
			if limit > 0 && out.Len() >= limit {
				return out.String(), nil
			}
		}
	}
	if out.Len() == 0 {
		return "", fmt.Errorf("PDF contains no extractable text")
	}
	return out.String(), nil
}

func writeBounded(out *strings.Builder, text string, limit int) {
	if limit <= 0 || out.Len() >= limit {
		return
	}
	text = strings.ToValidUTF8(text, "�")
	remaining := limit - out.Len()
	if len(text) <= remaining {
		out.WriteString(text)
		return
	}
	text = text[:remaining]
	for len(text) > 0 && !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	out.WriteString(text)
}
