package orchestrator

import "strings"

type shellWord struct {
	raw   string
	value string
}

// Parse literal words only; expansions require evidence from the actual shell.
func literalShellWords(command string) ([]shellWord, bool) {
	var words []shellWord
	for i := 0; i < len(command); {
		for i < len(command) && strings.ContainsRune(" \t\r\n", rune(command[i])) {
			i++
		}
		if i == len(command) {
			break
		}
		start := i
		var value strings.Builder
		var quote byte
		for i < len(command) {
			ch := command[i]
			if quote == 0 && strings.ContainsRune(" \t\r\n", rune(ch)) {
				break
			}
			if ch == '\\' && quote != '\'' {
				i++
				if i == len(command) {
					return nil, false
				}
				if quote == '"' && !strings.ContainsRune("$`\"\\\n", rune(command[i])) {
					value.WriteByte('\\')
				}
				if command[i] != '\n' {
					value.WriteByte(command[i])
				}
				i++
				continue
			}
			if quote == 0 && (ch == '\'' || ch == '"') {
				quote = ch
			} else if quote != 0 && ch == quote {
				quote = 0
			} else {
				if quote != '\'' && strings.ContainsRune("$`", rune(ch)) || quote == 0 && strings.ContainsRune(";&|()<>*?[]~", rune(ch)) {
					return nil, false
				}
				value.WriteByte(ch)
			}
			i++
		}
		if quote != 0 {
			return nil, false
		}
		words = append(words, shellWord{raw: command[start:i], value: value.String()})
	}
	return words, true
}
