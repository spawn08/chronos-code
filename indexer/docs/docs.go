// Package docs indexes documents (M8): Markdown files are split into one
// section per heading and plain-text files into blocks, each a
// facts.KindSection symbol whose Doc holds the section text, so BM25 search
// finds them like code. Mentions of code in a section are RefMention
// references, strongest first:
//
//	symbol  a backticked identifier (`Engine.Update`, `parse()`)
//	path    a file path (docs/x.md, [link](../cmd/main.go))
//	route   a route or URL path (GET /v1/users/{id}, https://host/v1/users)
//	ticket  a ticket id (ABC-123)
//	ident   a bare code-shaped identifier (camelCase, snake_case, Pkg.Name)
//
// The query layer resolves them against the symbol table, the file list and
// the contract nodes. Everything here reads one file.
package docs

import (
	"path"
	"regexp"
	"strings"

	"github.com/spawn08/chronos-code/indexer/contracts"
	"github.com/spawn08/chronos-code/indexer/facts"
)

// Languages recorded as facts.File.Lang.
const (
	LangMarkdown = "markdown"
	LangText     = "text"
)

// Limits.
const (
	maxDocBytes         = 2048 // section text kept for search
	maxMentions         = 64   // per section
	maxSignatureBytes   = 200
	textBlockLines      = 60 // plain-text files are split into blocks of about this many lines
	maxMentionTextBytes = 200
)

// Kind returns the document language of a root-relative path, or "".
func Kind(rel string) string {
	base := strings.ToLower(path.Base(rel))
	switch path.Ext(base) {
	case ".md", ".markdown", ".mdx":
		return LangMarkdown
	case ".txt", ".rst", ".adoc":
		if base == "cmakelists.txt" || strings.HasPrefix(base, "requirements") || strings.HasPrefix(base, "license") ||
			base == "robots.txt" || strings.HasSuffix(base, ".lock.txt") {
			return ""
		}
		return LangText
	}
	return ""
}

type section struct {
	name        string
	level       int // heading level; 0 for the preamble
	line, end   int // heading line; last line of the section with its subsections
	lines       []string
	lineNumbers []int
	inFence     []bool
	parent      int // index into sections, or -1
	sym         int // index into File.Symbols, or -1
}

// Extract fills f from a document: Lang and one section symbol per heading
// (text before the first heading is a section named after the file), with
// the mentions in each section.
func Extract(f *facts.File, lang string, src []byte) {
	f.Lang = lang
	lines := strings.Split(strings.ReplaceAll(string(src), "\r\n", "\n"), "\n")
	var secs []*section
	if lang == LangMarkdown {
		secs = markdownSections(lines, path.Base(f.Path))
	} else {
		secs = textSections(lines, path.Base(f.Path))
	}
	for _, s := range secs {
		body := strings.TrimSpace(strings.Join(s.lines, "\n"))
		if s.name == "" && body == "" {
			continue
		}
		sig := s.name
		for p := s.parent; p >= 0; p = secs[p].parent {
			sig = secs[p].name + " > " + sig
		}
		sym := facts.Symbol{
			Name: s.name, Kind: facts.KindSection, Line: s.line, EndLine: max(s.end, s.line),
			Signature: clip(sig, maxSignatureBytes), Doc: clip(body, maxDocBytes),
			Exported: true, Visibility: facts.VisPublic,
		}
		if s.parent >= 0 && secs[s.parent].sym >= 0 {
			sym.Container = secs[s.parent].sym + 1
		}
		s.sym = len(f.Symbols)
		f.Symbols = append(f.Symbols, sym)
		mentions(f, s)
	}
}

var (
	atxHeading = regexp.MustCompile(`^ {0,3}(#{1,6})\s+(.*?)\s*#*\s*$`)
	setextH1   = regexp.MustCompile(`^ {0,3}=+\s*$`)
	setextH2   = regexp.MustCompile(`^ {0,3}-+\s*$`)
	fence      = regexp.MustCompile("^ {0,3}(```|~~~)")
)

func markdownSections(lines []string, file string) []*section {
	pre := &section{name: file, level: 0, line: 1, parent: -1, sym: -1}
	secs := []*section{pre}
	cur := pre
	inFence := false
	start := 0
	// front matter
	if len(lines) > 0 && strings.TrimSpace(lines[0]) == "---" {
		for i := 1; i < len(lines); i++ {
			if t := strings.TrimSpace(lines[i]); t == "---" || t == "..." {
				start = i + 1
				break
			}
		}
	}
	for i := start; i < len(lines); i++ {
		ln := lines[i]
		if fence.MatchString(ln) {
			inFence = !inFence
			cur.add(ln, i+1, true)
			continue
		}
		level, name := 0, ""
		if !inFence {
			if m := atxHeading.FindStringSubmatch(ln); m != nil {
				level, name = len(m[1]), m[2]
			} else if i+1 < len(lines) && strings.TrimSpace(ln) != "" && !strings.HasPrefix(strings.TrimSpace(ln), "|") &&
				(setextH1.MatchString(lines[i+1]) || setextH2.MatchString(lines[i+1]) && len(cur.lines) > 0 && strings.TrimSpace(cur.lines[len(cur.lines)-1]) == "") {
				level, name = 2, strings.TrimSpace(ln)
				if setextH1.MatchString(lines[i+1]) {
					level = 1
				}
				i++
			}
		}
		if level == 0 {
			cur.add(ln, i+1, inFence)
			continue
		}
		s := &section{name: cleanHeading(name), level: level, line: i + 1, parent: -1, sym: -1}
		for p := len(secs) - 1; p >= 1; p-- {
			if secs[p].level < level {
				s.parent = p
				break
			}
		}
		secs = append(secs, s)
		cur = s
	}
	// A section ends before the next heading of its level or higher.
	for i, s := range secs {
		s.end = len(lines)
		for _, n := range secs[i+1:] {
			if n.level <= s.level {
				s.end = n.line - 1
				break
			}
		}
	}
	if len(secs) > 1 {
		pre.end = secs[1].line - 1
		if strings.TrimSpace(strings.Join(pre.lines, "")) == "" {
			pre.name = "" // no preamble: dropped
		}
	}
	return secs
}

func (s *section) add(ln string, n int, fenced bool) {
	s.lines = append(s.lines, ln)
	s.lineNumbers = append(s.lineNumbers, n)
	s.inFence = append(s.inFence, fenced)
}

var emphasis = strings.NewReplacer("**", "", "__", "", "`", "")

func cleanHeading(h string) string {
	h = emphasis.Replace(strings.TrimSpace(h))
	// [text](link) -> text
	return mdLink.ReplaceAllString(h, "$1")
}

// textSections splits plain text into blocks of about textBlockLines
// lines, breaking at blank lines; each block is named after the file and
// its first line.
func textSections(lines []string, file string) []*section {
	var secs []*section
	for start := 0; start < len(lines); {
		end := min(start+textBlockLines, len(lines))
		for e := end; e < len(lines) && e < start+2*textBlockLines; e++ {
			if strings.TrimSpace(lines[e]) == "" {
				end = e
				break
			}
		}
		s := &section{level: 1, line: start + 1, end: end, parent: -1, sym: -1}
		for i := start; i < end; i++ {
			s.add(lines[i], i+1, false)
		}
		first := ""
		for _, l := range s.lines {
			if t := strings.TrimSpace(l); t != "" {
				first = t
				break
			}
		}
		s.name = file
		if first != "" && start > 0 {
			s.name = file + ": " + clip(first, 60)
		}
		secs = append(secs, s)
		start = max(end, start+1)
	}
	return secs
}

var (
	inlineCode = regexp.MustCompile("`([^`\n]{1,200})`")
	mdLink     = regexp.MustCompile(`\[([^\]]*)\]\(([^)\s]+)(?:\s+"[^"]*")?\)`)
	filePath   = regexp.MustCompile(`(?:^|[\s(\["'])((?:\.{1,2}/)?(?:[\w.-]+/)+[\w.-]+\.[A-Za-z0-9]{1,8})\b`)
	routeText  = regexp.MustCompile(`\b(GET|POST|PUT|DELETE|PATCH|HEAD|OPTIONS)\s+(/[\w{}:.$<>\[\]/-]*)`)
	urlText    = regexp.MustCompile(`https?://[\w.-]+(?::\d+)?(/[\w{}:.$/-]*)`)
	ticket     = regexp.MustCompile(`\b[A-Z][A-Z0-9]{1,9}-\d{1,7}\b`)
	identText  = regexp.MustCompile(`\b[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*\b`)
	symbolLike = regexp.MustCompile(`^[A-Za-z_$][\w$]*(?:(?:\.|::|#)[A-Za-z_$][\w$]*)*(?:\(\))?$`)
)

type mention struct {
	kind, text string
	line       int
}

func mentions(f *facts.File, s *section) {
	seen := map[string]bool{}
	var out []mention
	add := func(kind, text string, line int) {
		text = strings.TrimSpace(text)
		if text == "" || len(text) > maxMentionTextBytes || len(out) >= maxMentions {
			return
		}
		key := kind + "\x00" + text
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, mention{kind, text, line})
	}
	for i, ln := range s.lines {
		line := s.lineNumbers[i]
		if s.inFence[i] {
			continue
		}
		masked := ln
		// backticked spans
		for _, m := range inlineCode.FindAllStringSubmatchIndex(ln, -1) {
			code := strings.TrimSpace(ln[m[2]:m[3]])
			switch {
			case routeText.MatchString(code) || strings.HasPrefix(code, "/") && !strings.Contains(path.Base(code), "."):
				if k := routeKey(code); k != "" {
					add(facts.MentionRoute, k, line)
				}
			case isPath(code):
				add(facts.MentionPath, strings.TrimPrefix(code, "./"), line)
			case symbolLike.MatchString(code) && len(code) >= 2:
				name := strings.TrimSuffix(code, "()")
				name = strings.NewReplacer("::", ".", "#", ".").Replace(name)
				add(facts.MentionSymbol, name, line)
			}
			masked = masked[:m[0]] + strings.Repeat(" ", m[1]-m[0]) + masked[m[1]:]
		}
		for _, m := range mdLink.FindAllStringSubmatchIndex(masked, -1) {
			target := masked[m[4]:m[5]]
			if i := strings.IndexByte(target, '#'); i >= 0 {
				target = target[:i]
			}
			if !strings.Contains(target, "://") && target != "" && isPath(target) {
				add(facts.MentionPath, target, line)
			}
			masked = masked[:m[4]] + strings.Repeat(" ", m[5]-m[4]) + masked[m[5]:]
		}
		for _, m := range routeText.FindAllStringSubmatch(masked, -1) {
			if k := routeKey(m[0]); k != "" {
				add(facts.MentionRoute, k, line)
			}
		}
		for _, m := range urlText.FindAllStringSubmatch(masked, -1) {
			if k := contracts.RouteKey("", m[1]); k != "" && m[1] != "/" && m[1] != "" {
				add(facts.MentionRoute, k, line)
			}
		}
		for _, m := range filePath.FindAllStringSubmatch(masked, -1) {
			if !strings.Contains(m[1], "://") {
				add(facts.MentionPath, strings.TrimPrefix(m[1], "./"), line)
			}
		}
		for _, t := range ticket.FindAllString(masked, -1) {
			add(facts.MentionTicket, t, line)
		}
		for _, w := range identText.FindAllString(urlText.ReplaceAllString(masked, ""), -1) {
			if codeShaped(w) {
				add(facts.MentionIdent, w, line)
			}
		}
	}
	for _, m := range out {
		f.Refs = append(f.Refs, facts.Ref{Kind: facts.RefMention, Enclosing: s.sym, Name: m.text, Qualifier: m.kind, Line: m.line})
	}
}

func routeKey(s string) string {
	s = strings.TrimSpace(s)
	if m := routeText.FindStringSubmatch(s); m != nil {
		return contracts.RouteKey(m[1], m[2])
	}
	return contracts.RouteKey("", s)
}

// isPath reports text shaped like a file path: a directory part and a
// short extension, or a bare file name with a known source extension.
func isPath(s string) bool {
	if strings.ContainsAny(s, " \t()<>{}\"'`") || strings.Contains(s, "://") {
		return false
	}
	ext := path.Ext(s)
	if len(ext) < 2 || len(ext) > 9 {
		return false
	}
	return strings.Contains(s, "/")
}

// codeShaped reports an identifier that does not read as a word: mixed
// case after the first letter (parseFile, HTTPServer), an inner
// underscore (max_tokens), or a dotted name with an upper-case part
// (Engine.Update).
func codeShaped(w string) bool {
	if len(w) < 4 || strings.Trim(w, "_") != w && !strings.Contains(strings.Trim(w, "_"), "_") {
		return false
	}
	if strings.Contains(w, ".") {
		parts := strings.Split(w, ".")
		for _, p := range parts {
			if p == "" {
				return false
			}
		}
		// Engine.Update, os.Exit; not e.g, README.md
		last := parts[len(parts)-1]
		return strings.ToLower(w) != w && len(last) > 1 && (strings.ToLower(last) != last || len(last) > 4)
	}
	if strings.Contains(strings.Trim(w, "_"), "_") {
		return strings.ToUpper(w) != w || len(w) > 4 // SCREAMING_CASE constants count too
	}
	rs := []rune(w)
	for i := 1; i < len(rs); i++ {
		if rs[i] >= 'A' && rs[i] <= 'Z' && (rs[i-1] >= 'a' && rs[i-1] <= 'z') {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// cut at a rune boundary
	for n > 0 && n < len(s) && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
