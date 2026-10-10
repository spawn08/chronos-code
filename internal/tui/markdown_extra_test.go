package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestLatexToText(t *testing.T) {
	cases := map[string]string{
		`\pi_t(\text{trend})\ge0.60`: "πₜ(trend) ≥ 0.60",
		`\pi_{t-1}(z')`:              "πₜ₋₁(z')",
		`p(x_t\mid z)`:               "p(xₜ ∣ z)",
		`\sum_{z'} P(z\mid z')`:      "∑_(z') P(z ∣ z')",
		`\frac{a+b}{c}`:              "(a+b)/c",
		`x^2`:                        "x²",
	}
	for in, want := range cases {
		if got := latexToText(in); got != want {
			t.Errorf("latexToText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderMarkdownLite_Math(t *testing.T) {
	src := "Trend: \\(\\pi_t(\\text{trend})\\ge0.60\\) and costs $5 and $10.\n\n\\[\n\\pi_t(z)\\propto\np(x_t\\mid z)\n\\]"
	got := ansi.Strip(RenderMarkdownLite(src, 0))
	for _, want := range []string{"πₜ(trend) ≥ 0.60", "$5 and $10", "πₜ(z) ∝ p(xₜ ∣ z)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	if strings.Contains(got, `\`) {
		t.Errorf("raw latex leaked: %q", got)
	}
}

func TestRenderMarkdownLite_Table(t *testing.T) {
	src := "| Gate | Rule |\n|---|:-:|\n| Trend | \\(\\pi_t\\ge0.60\\) |\n| Stress | no **new** entries and a very long explanation here |\n\nafter"
	for _, width := range []int{0, 30, 12} {
		got := RenderMarkdownLite(src, width)
		for i, line := range strings.Split(got, "\n") {
			if width > 0 && lipgloss.Width(line) > width {
				t.Errorf("width %d line %d too wide: %q", width, i, line)
			}
		}
		if width == 0 {
			plain := ansi.Strip(got)
			if !strings.Contains(plain, "┌") || !strings.Contains(plain, "πₜ ≥ 0.60") || strings.Contains(plain, "---") {
				t.Errorf("bad table: %q", plain)
			}
		}
		if !strings.Contains(ansi.Strip(got), "after") {
			t.Errorf("text after table lost")
		}
	}
}
