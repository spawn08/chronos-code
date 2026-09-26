package generic

import (
	"bytes"
	"regexp"
)

// cFamily lists the pack languages whose sources go through the C
// preprocessor.
var cFamily = map[string]bool{"c": true, "cpp": true, "objc": true}

// normalizePreproc works around two C preprocessor forms the tree-sitter C
// grammars cannot parse, each of which can turn the rest of a file into
// error nodes (a whole main() lost after a string literal split by #if).
// Offsets and line breaks are kept, so every position stays valid:
//
//   - A null directive, "#" alone or followed only by a comment, loses its
//     "#".
//   - A conditional group (#if/#ifdef/#ifndef ... #endif) that opens in the
//     middle of an expression (the previous code line does not end with
//     ";", "{", "}" or a label's ":") has its directive lines blanked, so
//     the branches parse as one expression (both are kept).
//   - An export macro between class or struct and the type's name
//     (class FOO_API Bar : public Base {) is blanked.
//
// src is returned unchanged when neither form occurs.
func normalizePreproc(src []byte) []byte {
	var out []byte // copy on first change
	blank := func(from, to int) {
		if out == nil {
			out = bytes.Clone(src)
		}
		for i := from; i < to; i++ {
			if out[i] != '\n' && out[i] != '\r' {
				out[i] = ' '
			}
		}
	}
	var (
		last      byte   // last significant byte of the previous code line
		inComment bool   // inside a /* */ comment spanning lines
		inMacro   bool   // continuation line of a directive
		groups    []bool // open conditional groups: blanked or not
	)
	for start := 0; start < len(src); {
		end := bytes.IndexByte(src[start:], '\n')
		if end < 0 {
			end = len(src)
		} else {
			end += start
		}
		line := src[start:end]
		next := end + 1
		cont := bytes.HasSuffix(bytes.TrimRight(line, "\r"), []byte{'\\'})
		switch {
		case inMacro:
			inMacro = cont
		case inComment:
			if i := bytes.Index(line, []byte("*/")); i >= 0 {
				inComment = false
				if c := lastSignificant(line[i+2:]); c != 0 {
					last = c
				}
			}
		default:
			t := bytes.TrimLeft(line, " \t")
			if len(t) > 0 && t[0] == '#' {
				inMacro = cont
				word, rest := directive(t[1:])
				off := start + (len(line) - len(t))
				switch word {
				case "":
					if r := bytes.TrimSpace(rest); len(r) == 0 || bytes.HasPrefix(r, []byte("//")) || bytes.HasPrefix(r, []byte("/*")) {
						blank(off, off+1) // null directive
					}
				case "if", "ifdef", "ifndef":
					mid := last != 0 && last != ';' && last != '{' && last != '}' && last != ':'
					groups = append(groups, mid)
					if mid {
						blank(start, end)
					}
				case "elif", "elifdef", "elifndef", "else":
					if n := len(groups); n > 0 && groups[n-1] {
						blank(start, end)
					}
				case "endif":
					if n := len(groups); n > 0 {
						if groups[n-1] {
							blank(start, end)
						}
						groups = groups[:n-1]
					}
				}
				break
			}
			for _, m := range exportMacro.FindAllSubmatchIndex(line, -1) {
				blank(start+m[2], start+m[3])
			}
			code := line
			if i := bytes.LastIndex(line, []byte("/*")); i >= 0 && !bytes.Contains(line[i:], []byte("*/")) && !inString(line, i) {
				inComment, code = true, line[:i]
			}
			if c := lastSignificant(code); c != 0 {
				last = c
			}
		}
		start = next
	}
	if out == nil {
		return src
	}
	return out
}

// exportMacro matches an all-caps macro between class or struct and a type
// name followed by a class head or the end of the line.
var exportMacro = regexp.MustCompile(`\b(?:class|struct)\s+([A-Z][A-Z0-9_]+)\s+[A-Za-z_]\w*\s*(?:\bfinal\b|\{|:(?:[^:]|$)|\r?$)`)

// directive splits the text after '#' into the directive name and the rest.
func directive(t []byte) (string, []byte) {
	t = bytes.TrimLeft(t, " \t")
	i := 0
	for i < len(t) && (t[i] >= 'a' && t[i] <= 'z') {
		i++
	}
	return string(t[:i]), t[i:]
}

// lastSignificant returns the last byte of a code line that is not space
// or part of a trailing // or /* */ comment; 0 for a blank line. A "::"
// counts as mid-expression, unlike a label's ":".
func lastSignificant(line []byte) byte {
	if i := bytes.Index(line, []byte("//")); i >= 0 && !inString(line, i) {
		line = line[:i]
	}
	for {
		line = bytes.TrimRight(line, " \t\r")
		if !bytes.HasSuffix(line, []byte("*/")) {
			break
		}
		i := bytes.LastIndex(line, []byte("/*"))
		if i < 0 {
			return 0 // the end of a comment opened on an earlier line
		}
		line = line[:i]
	}
	if len(line) == 0 {
		return 0
	}
	if bytes.HasSuffix(line, []byte("::")) {
		return '.'
	}
	return line[len(line)-1]
}

// inString reports whether byte i of line is inside a string or character
// literal, counting unescaped quotes before it.
func inString(line []byte, i int) bool {
	var quote byte
	for j := 0; j < i; j++ {
		switch c := line[j]; {
		case quote != 0 && c == '\\':
			j++
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '"' || c == '\''):
			quote = c
		}
	}
	return quote != 0
}
