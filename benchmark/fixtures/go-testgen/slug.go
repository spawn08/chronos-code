package slug

import "strings"

func Slug(input string) string {
	return strings.Join(strings.Fields(strings.ToLower(input)), "-")
}
