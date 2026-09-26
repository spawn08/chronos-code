package manifest

import (
	"strings"
)

// toml is the subset of TOML manifests use: [table] and [[array]] headers
// and key = value lines, with values kept as raw text (strings, arrays and
// inline tables may span lines). Keys of an [[array]] table repeat.
type toml struct {
	order []string                     // tables in first-seen order ("" is the root)
	keys  map[string][]string          // table -> keys in order
	vals  map[string]map[string]string // table -> key -> raw value
}

func (t *toml) get(table, key string) string { return t.vals[table][key] }

func parseTOML(src []byte) *toml {
	t := &toml{keys: map[string][]string{}, vals: map[string]map[string]string{}}
	table := ""
	t.table(table)
	lines := strings.Split(string(src), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(stripTOMLComment(lines[i]))
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "[["):
			table = strings.TrimSpace(strings.Trim(line, "[]"))
			t.table(table)
			continue
		case strings.HasPrefix(line, "["):
			table = strings.TrimSpace(strings.Trim(line, "[]"))
			t.table(table)
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		// A value continues while its brackets are open.
		for depth(val) > 0 && i+1 < len(lines) {
			i++
			val += " " + strings.TrimSpace(stripTOMLComment(lines[i]))
		}
		// dotted keys: a.b = v belongs to table.a
		tb := table
		if dot := strings.LastIndexByte(key, '.'); dot > 0 && !strings.ContainsAny(key, `"'`) {
			if tb != "" {
				tb += "."
			}
			tb += key[:dot]
			key = key[dot+1:]
			t.table(tb)
		}
		if _, dup := t.vals[tb][key]; !dup {
			t.keys[tb] = append(t.keys[tb], key)
		}
		t.vals[tb][key] = val
	}
	return t
}

func (t *toml) table(name string) {
	if _, ok := t.vals[name]; !ok {
		t.vals[name] = map[string]string{}
		t.order = append(t.order, name)
	}
}

// depth counts unclosed [ and { outside strings.
func depth(s string) int {
	d := 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '[' || c == '{':
			d++
		case c == ']' || c == '}':
			d--
		}
	}
	return d
}

func stripTOMLComment(line string) string {
	var q byte
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '#':
			return line[:i]
		}
	}
	return line
}

func unquoteTOML(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		return v[1 : len(v)-1]
	}
	return ""
}

// splitTop splits s at top-level commas.
func splitTop(s string) []string {
	var out []string
	d, start := 0, 0
	var q byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case q != 0:
			if c == '\\' && q == '"' {
				i++
			} else if c == q {
				q = 0
			}
		case c == '"' || c == '\'':
			q = c
		case c == '[' || c == '{':
			d++
		case c == ']' || c == '}':
			d--
		case c == ',' && d == 0:
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	if rest := strings.TrimSpace(s[start:]); rest != "" {
		out = append(out, rest)
	}
	return out
}

// tomlStrings returns the strings of an array value ["a", "b"].
func tomlStrings(v string) []string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil
	}
	var out []string
	for _, item := range splitTop(v[1 : len(v)-1]) {
		if s := unquoteTOML(item); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// inlineTable returns the raw values of an inline table { k = v, ... }.
func inlineTable(v string) map[string]string {
	v = strings.TrimSpace(v)
	out := map[string]string{}
	if !strings.HasPrefix(v, "{") || !strings.HasSuffix(v, "}") {
		return out
	}
	for _, item := range splitTop(v[1 : len(v)-1]) {
		if k, val, ok := strings.Cut(item, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(val)
		}
	}
	return out
}

// tomlTables returns the inline tables of an array value [{...}, {...}].
func tomlTables(v string) []map[string]string {
	v = strings.TrimSpace(v)
	if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
		return nil
	}
	var out []map[string]string
	for _, item := range splitTop(v[1 : len(v)-1]) {
		out = append(out, inlineTable(item))
	}
	return out
}
