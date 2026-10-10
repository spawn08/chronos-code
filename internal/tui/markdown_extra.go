package tui

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// This file extends RenderMarkdownLite with two things terminals cannot do
// natively: LaTeX math (converted to Unicode text) and GFM pipe tables
// (drawn with box characters and wrapped to the available width).

var latexSymbols = map[string]string{
	"alpha": "α", "beta": "β", "gamma": "γ", "delta": "δ", "epsilon": "ε", "varepsilon": "ε",
	"zeta": "ζ", "eta": "η", "theta": "θ", "vartheta": "ϑ", "iota": "ι", "kappa": "κ",
	"lambda": "λ", "mu": "μ", "nu": "ν", "xi": "ξ", "pi": "π", "rho": "ρ", "sigma": "σ",
	"tau": "τ", "upsilon": "υ", "phi": "φ", "varphi": "φ", "chi": "χ", "psi": "ψ", "omega": "ω",
	"Gamma": "Γ", "Delta": "Δ", "Theta": "Θ", "Lambda": "Λ", "Xi": "Ξ", "Pi": "Π",
	"Sigma": "Σ", "Phi": "Φ", "Psi": "Ψ", "Omega": "Ω",
	"sum": "∑", "prod": "∏", "int": "∫", "partial": "∂", "nabla": "∇", "infty": "∞",
	"cdot": "·", "times": "×", "div": "÷", "pm": "±", "mp": "∓", "ldots": "…", "cdots": "…", "dots": "…",
	"forall": "∀", "exists": "∃", "neg": "¬", "land": "∧", "lor": "∨", "cap": "∩", "cup": "∪",
	"emptyset": "∅", "prime": "′", "degree": "°", "ell": "ℓ", "langle": "⟨", "rangle": "⟩",
	"quad": " ", "qquad": "  ", "mid": " ∣ ", "vert": "|", "lvert": "|", "rvert": "|",
	// relations and arrows are padded so "a\ge b" and "a\ge0" both read well.
	"propto": " ∝ ", "ge": " ≥ ", "geq": " ≥ ", "le": " ≤ ", "leq": " ≤ ", "neq": " ≠ ", "ne": " ≠ ",
	"approx": " ≈ ", "equiv": " ≡ ", "sim": " ∼ ", "in": " ∈ ", "notin": " ∉ ", "subset": " ⊂ ",
	"subseteq": " ⊆ ", "to": " → ", "rightarrow": " → ", "leftarrow": " ← ", "Rightarrow": " ⇒ ",
	"Leftrightarrow": " ⇔ ", "leftrightarrow": " ↔ ", "mapsto": " ↦ ", "gg": " ≫ ", "ll": " ≪ ",
}

var latexWrappers = map[string]bool{
	"text": true, "mathrm": true, "mathbf": true, "mathit": true, "operatorname": true,
	"mathbb": true, "mathcal": true, "boldsymbol": true, "rm": true, "textbf": true,
	"mathsf": true, "mathtt": true, "displaystyle": true, "bar": true, "hat": true, "tilde": true, "vec": true,
}

var subscripts = map[rune]rune{
	'0': '₀', '1': '₁', '2': '₂', '3': '₃', '4': '₄', '5': '₅', '6': '₆', '7': '₇', '8': '₈', '9': '₉',
	'+': '₊', '-': '₋', '=': '₌', '(': '₍', ')': '₎', 'a': 'ₐ', 'e': 'ₑ', 'h': 'ₕ', 'i': 'ᵢ', 'j': 'ⱼ',
	'k': 'ₖ', 'l': 'ₗ', 'm': 'ₘ', 'n': 'ₙ', 'o': 'ₒ', 'p': 'ₚ', 'r': 'ᵣ', 's': 'ₛ', 't': 'ₜ', 'u': 'ᵤ',
	'v': 'ᵥ', 'x': 'ₓ',
}

var superscripts = map[rune]rune{
	'0': '⁰', '1': '¹', '2': '²', '3': '³', '4': '⁴', '5': '⁵', '6': '⁶', '7': '⁷', '8': '⁸', '9': '⁹',
	'+': '⁺', '-': '⁻', '=': '⁼', '(': '⁽', ')': '⁾', 'n': 'ⁿ', 'i': 'ⁱ', 'T': 'ᵀ',
}

// latexToText converts a LaTeX math fragment to readable Unicode text. It is
// intentionally partial: unknown commands degrade to their bare name.
func latexToText(tex string) string {
	out, _ := latexConvert([]rune(tex), 0)
	return strings.TrimSpace(collapseSpaces(out))
}

func collapseSpaces(s string) string {
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	s = strings.ReplaceAll(s, "( ", "(")
	return strings.ReplaceAll(s, " )", ")")
}

// latexGroup reads one argument at r[i]: a {...} group or a single token.
func latexGroup(r []rune, i int) (string, int) {
	for i < len(r) && r[i] == ' ' {
		i++
	}
	if i >= len(r) {
		return "", i
	}
	if r[i] == '{' {
		depth, j := 1, i+1
		for j < len(r) && depth > 0 {
			switch r[j] {
			case '{':
				depth++
			case '}':
				depth--
			}
			j++
		}
		end := j
		if depth == 0 {
			end = j - 1
		}
		inner, _ := latexConvert(r[i+1:end], 0)
		return inner, j
	}
	if r[i] == '\\' {
		j := i + 1
		for j < len(r) && isASCIILetter(r[j]) {
			j++
		}
		if j == i+1 && j < len(r) {
			j++
		}
		s, _ := latexConvert(r[i:j], 0)
		return s, j
	}
	return string(r[i]), i + 1
}

func isASCIILetter(c rune) bool { return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') }

func latexScript(arg string, table map[rune]rune, marker string) string {
	mapped := make([]rune, 0, len(arg))
	for _, c := range arg {
		m, ok := table[c]
		if !ok {
			if len([]rune(arg)) == 1 {
				return marker + arg
			}
			return marker + "(" + arg + ")"
		}
		mapped = append(mapped, m)
	}
	return string(mapped)
}

func latexConvert(r []rune, i int) (string, int) {
	var b strings.Builder
	for i < len(r) {
		c := r[i]
		switch {
		case c == '{' || c == '}':
			i++
		case c == '_' || c == '^':
			arg, next := latexGroup(r, i+1)
			if c == '_' {
				b.WriteString(latexScript(arg, subscripts, "_"))
			} else {
				b.WriteString(latexScript(arg, superscripts, "^"))
			}
			i = next
		case c == '\\':
			i++
			if i >= len(r) {
				continue
			}
			if !isASCIILetter(r[i]) {
				switch r[i] {
				case ',', ';', ':', ' ':
					b.WriteByte(' ')
				case '{', '}', '%', '$', '&', '#', '_':
					b.WriteRune(r[i])
				case '|':
					b.WriteString("‖")
				case '\\':
					b.WriteByte(' ')
				}
				i++
				continue
			}
			j := i
			for j < len(r) && isASCIILetter(r[j]) {
				j++
			}
			name := string(r[i:j])
			i = j
			switch {
			case name == "left" || name == "right":
				if i < len(r) && r[i] == '.' {
					i++
				}
			case name == "frac" || name == "dfrac" || name == "tfrac":
				num, n1 := latexGroup(r, i)
				den, n2 := latexGroup(r, n1)
				i = n2
				b.WriteString(fracPart(num) + "/" + fracPart(den))
			case name == "sqrt":
				arg, next := latexGroup(r, i)
				i = next
				b.WriteString("√" + fracPart(arg))
			case latexWrappers[name]:
				arg, next := latexGroup(r, i)
				i = next
				b.WriteString(arg)
			default:
				if s, ok := latexSymbols[name]; ok {
					b.WriteString(s)
				} else {
					b.WriteString(name)
				}
			}
		default:
			b.WriteRune(c)
			i++
		}
	}
	return b.String(), i
}

func fracPart(s string) string {
	s = strings.TrimSpace(s)
	if strings.ContainsAny(s, " +-*/") {
		return "(" + s + ")"
	}
	return s
}

// replaceMath finds math spans in s — \(..\), \[..\], $$..$$ and $..$ — and
// replaces each with fn(tex). A lone "$" (e.g. prices such as "$5 and $10")
// is not treated as math: single-dollar spans must hug their content and
// must not be followed by a digit.
func replaceMath(s string, fn func(tex string) string) string {
	if !strings.ContainsAny(s, `\$`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		rest := s[i:]
		var open, closer string
		switch {
		case strings.HasPrefix(rest, `\(`):
			open, closer = `\(`, `\)`
		case strings.HasPrefix(rest, `\[`):
			open, closer = `\[`, `\]`
		case strings.HasPrefix(rest, `$$`):
			open, closer = `$$`, `$$`
		case strings.HasPrefix(rest, `\$`):
			b.WriteString(`\$`)
			i += 2
			continue
		case rest[0] == '$':
			open, closer = `$`, `$`
		}
		if open != "" {
			body := rest[len(open):]
			if end := strings.Index(body, closer); end > 0 && mathSpanOK(open, body, end, len(closer)) {
				b.WriteString(fn(body[:end]))
				i += len(open) + end + len(closer)
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func mathSpanOK(open, body string, end, closeLen int) bool {
	if open != "$" {
		return true
	}
	inner := body[:end]
	if inner[0] == ' ' || inner[len(inner)-1] == ' ' {
		return false
	}
	if after := body[end+closeLen:]; after != "" && after[0] >= '0' && after[0] <= '9' {
		return false
	}
	return true
}

// displayMathBlock detects a \[ ... \] or $$ ... $$ block starting at
// lines[i]. It returns the converted text and the index of the last line it
// consumed.
func displayMathBlock(lines []string, i int) (string, int, bool) {
	first := strings.TrimSpace(lines[i])
	var open, closer string
	switch {
	case strings.HasPrefix(first, `\[`):
		open, closer = `\[`, `\]`
	case strings.HasPrefix(first, `$$`):
		open, closer = `$$`, `$$`
	default:
		return "", i, false
	}
	var parts []string
	text := strings.TrimPrefix(first, open)
	for j := i; j < len(lines); j++ {
		if j > i {
			text = strings.TrimSpace(lines[j])
		}
		if end := strings.Index(text, closer); end >= 0 {
			parts = append(parts, text[:end])
			return latexToText(strings.Join(parts, " ")), j, true
		}
		parts = append(parts, text)
	}
	return "", i, false // unterminated: render as ordinary text
}

// --- tables ---

func isTableRow(line string) bool {
	t := strings.TrimSpace(line)
	return strings.Count(t, "|") >= 1 && strings.HasPrefix(t, "|") || strings.Count(t, "|") >= 2
}

// tableAligns parses a separator row like |:--|:-:|--:| ; ok is false if line
// is not one.
func tableAligns(line string) ([]byte, bool) {
	if !strings.Contains(line, "-") || !isTableRow(line) {
		return nil, false
	}
	var aligns []byte
	for _, c := range splitTableRow(line) {
		c = strings.TrimSpace(c)
		if c == "" || strings.Trim(c, "-:") != "" || !strings.Contains(c, "-") {
			return nil, false
		}
		l, r := strings.HasPrefix(c, ":"), strings.HasSuffix(c, ":")
		switch {
		case l && r:
			aligns = append(aligns, 'c')
		case r:
			aligns = append(aligns, 'r')
		default:
			aligns = append(aligns, 'l')
		}
	}
	return aligns, len(aligns) > 0
}

// splitTableRow splits on unescaped pipes outside backtick spans and math.
func splitTableRow(line string) []string {
	t := strings.TrimSpace(line)
	t = strings.TrimPrefix(t, "|")
	if strings.HasSuffix(t, "|") && !strings.HasSuffix(t, `\|`) {
		t = t[:len(t)-1]
	}
	var cells []string
	var cur strings.Builder
	inCode, inMath := false, false
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case c == '\\' && i+1 < len(t):
			if t[i+1] == '|' {
				cur.WriteByte('|')
			} else {
				cur.WriteByte(c)
				cur.WriteByte(t[i+1])
				if t[i+1] == '(' {
					inMath = true
				} else if t[i+1] == ')' {
					inMath = false
				}
			}
			i++
			continue
		case c == '`':
			inCode = !inCode
		case c == '|' && !inCode && !inMath:
			cells = append(cells, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteByte(c)
	}
	return append(cells, strings.TrimSpace(cur.String()))
}

// renderTable draws a boxed table that never exceeds width (when > 0).
// Cells wrap inside their column; if columns would be unreadably narrow the
// table falls back to a stacked "header: value" list per row.
func renderTable(header []string, aligns []byte, rows [][]string, width int) string {
	n := len(header)
	norm := func(r []string) []string {
		out := make([]string, n)
		for i := range out {
			if i < len(r) {
				out[i] = inlineStyle(r[i])
			}
		}
		return out
	}
	head := norm(header)
	body := make([][]string, len(rows))
	for i, r := range rows {
		body[i] = norm(r)
	}
	colW := make([]int, n)
	for _, r := range append([][]string{head}, body...) {
		for i, c := range r {
			for _, l := range strings.Split(c, "\n") {
				colW[i] = max(colW[i], ansi.StringWidth(l))
			}
		}
	}
	for i := range colW {
		colW[i] = max(colW[i], 1)
	}
	overhead := 3*n + 1
	if width > 0 {
		avail := width - overhead
		if avail < n*4 {
			return renderTableStacked(head, body, width)
		}
		for sum(colW) > avail {
			wi := 0
			for i, w := range colW {
				if w > colW[wi] {
					wi = i
				}
			}
			colW[wi]--
		}
	}
	rule := func(l, m, r string) string {
		segs := make([]string, n)
		for i, w := range colW {
			segs[i] = strings.Repeat("─", w+2)
		}
		return styleDim.Render(l + strings.Join(segs, m) + r)
	}
	bar := styleDim.Render("│")
	row := func(cells []string, bold bool) []string {
		wrapped := make([][]string, n)
		height := 1
		for i, c := range cells {
			wrapped[i] = strings.Split(ansi.Wrap(c, colW[i], ""), "\n")
			height = max(height, len(wrapped[i]))
		}
		lines := make([]string, height)
		for h := range lines {
			var b strings.Builder
			b.WriteString(bar)
			for i := range cells {
				cell := ""
				if h < len(wrapped[i]) {
					cell = wrapped[i][h]
				}
				if bold && cell != "" {
					cell = styleBold.Render(cell)
				}
				b.WriteString(" " + padCell(cell, colW[i], aligns[i]) + " " + bar)
			}
			lines[h] = b.String()
		}
		return lines
	}
	out := []string{rule("┌", "┬", "┐")}
	out = append(out, row(head, true)...)
	out = append(out, rule("├", "┼", "┤"))
	for _, r := range body {
		out = append(out, row(r, false)...)
	}
	out = append(out, rule("└", "┴", "┘"))
	return strings.Join(out, "\n")
}

func renderTableStacked(head []string, body [][]string, width int) string {
	var out []string
	for i, r := range body {
		if i > 0 {
			out = append(out, "")
		}
		for j, c := range r {
			out = append(out, wrapText(styleBold.Render(ansi.Strip(head[j]))+": "+c, width))
		}
	}
	return strings.Join(out, "\n")
}

func padCell(s string, w int, align byte) string {
	gap := w - ansi.StringWidth(s)
	if gap <= 0 {
		return s
	}
	switch align {
	case 'r':
		return strings.Repeat(" ", gap) + s
	case 'c':
		return strings.Repeat(" ", gap/2) + s + strings.Repeat(" ", gap-gap/2)
	}
	return s + strings.Repeat(" ", gap)
}

func sum(xs []int) int {
	t := 0
	for _, x := range xs {
		t += x
	}
	return t
}
