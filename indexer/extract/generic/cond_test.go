package generic

import (
	"fmt"
	"slices"
	"testing"

	"github.com/spawn08/chronos-code/internal/indexer/extract/packs"
	"github.com/spawn08/chronos-code/internal/indexer/facts"
)

func TestEvalCond(t *testing.T) {
	macros := map[string]string{"WIDTH": "(32)", "ALIAS": "WIDTH", "FLAG": "", "FN": ""}
	for expr, want := range map[string]string{
		"1":                                 "true",
		"0":                                 "false",
		"WIDTH == 32":                       "true",
		"(ALIAS == 16)":                     "false",
		"defined(FLAG) && !defined(OTHER)":  "true",
		"defined FLAG":                      "true",
		"UNDEFINED":                         "false",
		"UINT_MAX == 0xFFFFFFFF":            "true",
		"UINTPTR_MAX <= 0xFFFFFFFF":         "false",
		"ULONG_MAX == 0xFFFFFFFFFFFFFFFFUL": "true",
		"WIDTH > 16 ? 1 : 0":                "true",
		"FN(2)":                             "unknown",
		"FLAG + 1":                          "unknown",
		"1 +":                               "unknown",
	} {
		v, ok := evalCond(expr, macros)
		got := fmt.Sprint(v)
		if !ok {
			got = "unknown"
		}
		if got != want {
			t.Errorf("evalCond(%q) = %s, want %s", expr, got, want)
		}
	}
}

func TestInactiveLines(t *testing.T) {
	src := `#ifndef GUARD_H
#define GUARD_H
#ifndef UNITY_INT_WIDTH
  #ifdef UINT_MAX
    #if (UINT_MAX == 0xFFFF)
      #define UNITY_INT_WIDTH (16)
    #elif (UINT_MAX == 0xFFFFFFFF)
      #define UNITY_INT_WIDTH (32)
    #endif
  #endif
#endif
#if (UNITY_INT_WIDTH == 32)
    typedef int UNITY_INT32;
#elif (UNITY_INT_WIDTH == 16)
    typedef long UNITY_INT32;
#else
    #error bad
#endif
#ifdef _WIN32
int win(void);
#else
int posix(void);
#endif
#if FN(1)
int maybe(void);
#else
int other(void);
#endif
#if 0
int never(void);
#endif
#endif
`
	got := inactiveLines([]byte(src))
	var lines []int
	for l := range got {
		lines = append(lines, l)
	}
	slices.Sort(lines)
	// 15: the 16-bit typedef, 20: win, 30: never (directive lines are not listed)
	if fmt.Sprint(lines) != "[15 20 30]" {
		t.Fatalf("inactive lines = %v", lines)
	}
	f := &facts.File{Path: "unity.h"}
	shared.Extract(f, packs.Default().ForPath("x.c"), []byte(src))
	for _, s := range f.Symbols {
		inactive := s.Modifiers&facts.ModInactive != 0
		if inactive != (s.Line == 15 || s.Line == 20 || s.Line == 30) {
			t.Errorf("%s at line %d: inactive = %v", s.Name, s.Line, inactive)
		}
	}
}
