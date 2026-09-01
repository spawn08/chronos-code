package router

import "testing"

func TestClassifyTask(t *testing.T) {
	cases := []struct {
		message string
		want    Classification
	}{
		{"fix the null pointer in auth.go", Classification{ComplexityMedium, TaskKindDebug}},
		{"explain how the router works", Classification{ComplexityLow, TaskKindExplain}},
		{"refactor the entire auth package to use interfaces", Classification{ComplexityHigh, TaskKindRefactor}},
		{"design a new cache", Classification{ComplexityLow, TaskKindArchitect}},
		{"add a CLI flag", Classification{ComplexityLow, TaskKindEdit}},
		{"", Classification{ComplexityLow, TaskKindEdit}},
		{"fix errors across all packages", Classification{ComplexityHigh, TaskKindDebug}},
		{"rename auth.go, config.go, and main.go", Classification{ComplexityHigh, TaskKindRefactor}},
		{"what is the error handling strategy", Classification{ComplexityMedium, TaskKindDebug}},
		{"move this code", Classification{ComplexityLow, TaskKindRefactor}},
	}
	for _, tc := range cases {
		if got := ClassifyTask(tc.message); got != tc.want {
			t.Errorf("ClassifyTask(%q) = %+v, want %+v", tc.message, got, tc.want)
		}
	}
}
