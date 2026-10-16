package tui

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"testing/quick"
	"unsafe"
)

func liveTextReference(text string, width int) string {
	return boundedTextTail(wrapText(boundedTextTail(text, maxItemRenderBytes, maxViewportLines), width), liveTextRenderBytes, maxViewportLines)
}

func TestLiveTextCachePrefixes(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"ascii", "a long word and some-space-separated text\nsecond line\n"},
		{"hardwrap", "abcdefghijklmnopqrstuvwxyz\n01234567890123456789"},
		{"whitespace", "   one  two   \n\n\tthree\r\nfour \t \n"},
		{"unicode", "café e\u0301 naïve\n你好世界 日本語\n👩‍💻 👨‍👩‍👧‍👦 🇮🇳 🏳️‍🌈"},
		{"unicode-space", "one\u00a0two\u2003three\u2028four\nnext"},
		{"ansi-style", "\x1b[31mred words\nmore red\x1b[0m plain\nlast"},
		{"ansi-link", "\x1b]8;;https://example.test\x1b\\linked words\nnext\x1b]8;;\x1b\\ end"},
		{"ansi-unclosed", "\x1b[1;38;2;1;2;3mstyled\nacross lines\n"},
		{"ansi-split-newline", "before\x1b]8;;https://example.test\nmore\x1b\\after\n"},
		{"controls", "one\x00two\x08three\x7ffour\nnext"},
		{"invalid-utf8", "start\xff\x9b31m\ncaf\xc3"},
	}
	for _, tc := range cases {
		for _, width := range []int{-1, 0, 1, 2, 5, 12, 80} {
			t.Run(fmt.Sprintf("%s/%d", tc.name, width), func(t *testing.T) {
				var cache liveTextCache
				for end := 0; end <= len(tc.text); end++ {
					text := tc.text[:end]
					got := cache.wrap(text, width)
					if want := wrapText(text, width); got != want {
						t.Fatalf("prefix %d: got %q, want %q", end, got, want)
					}
					if again := cache.wrap(text, width); again != got {
						t.Fatalf("idle frame changed prefix %d", end)
					}
				}
			})
		}
	}
}

func TestLiveTextCacheInvalidation(t *testing.T) {
	steps := []struct {
		text  string
		width int
	}{
		{"first line\nsecond word", 8},
		{"first line\nsecond words\nthird", 8},
		{"first line\nsecond words\nthird", 3},
		{"first line\nsecond words\nthird", 0},
		{"first line\nsecond words\nthird", 8},
		{"FIRST line\nsecond words\nthird", 8},
		{"short", 8},
		{"", 8},
		{"\x1b[31mred\nmore", 8},
		{"plain\nmore", 8},
		{"plain\nmore text", 8},
	}
	var cache liveTextCache
	for i, step := range steps {
		if got, want := cache.wrap(step.text, step.width), liveTextReference(step.text, step.width); got != want {
			t.Fatalf("step %d: got %q, want %q", i, got, want)
		}
	}
	cache.reset()
	if cache != (liveTextCache{}) {
		t.Fatal("reset retained state")
	}
	if got, want := cache.wrap(steps[0].text, 8), liveTextReference(steps[0].text, 8); got != want {
		t.Fatalf("after reset: got %q, want %q", got, want)
	}
}

func TestLiveTextCacheCompletedPrefix(t *testing.T) {
	var cache liveTextCache
	cache.wrap("first line\nunfinished", 6)
	prefix := cache.completed
	if prefix != wrapText("first line\n", 6) || cache.completedBytes != len("first line\n") {
		t.Fatalf("completed prefix was not cached: %+v", cache)
	}
	cache.wrap("first line\nunfinished words", 6)
	if unsafe.StringData(cache.completed) != unsafe.StringData(prefix) {
		t.Fatal("append reallocated the unchanged completed prefix")
	}
	cache.wrap("first line\nunfinished words\nnext", 6)
	if cache.completed != wrapText("first line\nunfinished words\n", 6) {
		t.Fatal("newly completed line did not extend the prefix")
	}
	cache.wrap("first line\nunfinished words\nnext\x1b[31mred", 6)
	if cache.completed != "" || cache.plain {
		t.Fatal("ANSI append did not invalidate plain prefix")
	}
}

func TestLiveTextCacheProperty(t *testing.T) {
	tokens := []string{"a", " word", "-", "\n", "\n\n", "\t", "\r", "界", "é", "e\u0301", "👩‍💻", "\u00a0", "\x1b[31m", "\x1b[0m", "\xff", "\xc3", "\xa9", "\x1b]8;;url\x1b\\", "\x00"}
	property := func(operations []uint8, initialWidth uint8) bool {
		var cache liveTextCache
		text, width := "", int(initialWidth%20)-1
		for _, operation := range operations {
			switch operation % 7 {
			case 0:
				width = int(operation%21) - 1
			case 1:
				text = text[:len(text)/2]
			case 2:
				text = tokens[int(operation)%len(tokens)]
			default:
				text += tokens[int(operation)%len(tokens)]
			}
			if got, want := cache.wrap(text, width), liveTextReference(text, width); got != want {
				t.Logf("operation %d, width %d, text %q: got %q, want %q", operation, width, text, got, want)
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500, Rand: rand.New(rand.NewSource(42))}); err != nil {
		t.Fatal(err)
	}
}

func TestLiveTextCacheRetention(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		width int
	}{
		{"bytes", strings.Repeat("界 words ", 30000), 80},
		{"lines", strings.Repeat("line\n", maxViewportLines*3), 80},
		{"wrapped-lines", strings.Repeat("abcdef\n", maxViewportLines), 1},
		{"ansi-expansion", "\x1b[31m" + strings.Repeat("x", maxItemRenderBytes), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cache liveTextCache
			for _, suffix := range []string{"", "é", "\nnext", strings.Repeat("z", maxItemRenderBytes+1)} {
				text := tc.text + suffix
				got := cache.wrap(text, tc.width)
				if want := liveTextReference(text, tc.width); got != want {
					t.Fatal("bounded render differs from reference")
				}
				if len(cache.source) > maxItemRenderBytes || len(cache.completed) > liveTextRenderBytes || len(cache.rendered) > liveTextRenderBytes {
					t.Fatalf("byte limits exceeded: source=%d completed=%d rendered=%d", len(cache.source), len(cache.completed), len(cache.rendered))
				}
				for _, retained := range []string{cache.source, cache.completed, cache.rendered} {
					if strings.Count(retained, "\n") >= maxViewportLines {
						t.Fatal("line retention limit exceeded")
					}
				}
				bounded := boundedTextTail(text, maxItemRenderBytes, maxViewportLines)
				if len(bounded) > 0 && unsafe.StringData(cache.source) == unsafe.StringData(bounded) {
					t.Fatal("cache retained a view onto the unbounded input")
				}
			}
		})
	}
}

func TestLiveTextCacheRollingTail(t *testing.T) {
	for _, chunk := range []string{strings.Repeat("row 界 has words\n", 200), strings.Repeat("界 words ", 600)} {
		var cache liveTextCache
		var text strings.Builder
		for i := 0; i < 40; i++ {
			text.WriteString(chunk)
			text.WriteString(fmt.Sprintf("frame %d", i))
			if got, want := cache.wrap(text.String(), 40), liveTextReference(text.String(), 40); got != want {
				t.Fatalf("rolling tail frame %d: got %d bytes, want %d", i, len(got), len(want))
			}
		}
	}
}

func TestLiveTextCacheIdleAllocations(t *testing.T) {
	var cache liveTextCache
	text := strings.Repeat("completed words\n", 100) + "trailing words"
	cache.wrap(text, 40)
	if got := testing.AllocsPerRun(100, func() { cache.wrap(text, 40) }); got != 0 {
		t.Fatalf("idle cache allocates: %g", got)
	}
}

func FuzzLiveTextCache(f *testing.F) {
	for _, seed := range []string{"plain\ntext", "界\ne\u0301", "\x1b[31mred\nmore\x1b[0m", "a\xff\nc"} {
		f.Add(seed, uint8(8))
	}
	f.Fuzz(func(t *testing.T, text string, w uint8) {
		if len(text) > 2048 {
			text = text[:2048]
		}
		width := int(w%40) - 1
		var cache liveTextCache
		for _, end := range []int{len(text) / 3, len(text) / 2, len(text)} {
			if got, want := cache.wrap(text[:end], width), liveTextReference(text[:end], width); got != want {
				t.Fatalf("prefix %d width %d: got %q, want %q", end, width, got, want)
			}
		}
		if got, want := cache.wrap(text, width+1), liveTextReference(text, width+1); got != want {
			t.Fatalf("resize: got %q, want %q", got, want)
		}
	})
}

var liveTextBenchmarkResult string

func BenchmarkLiveTextCacheIdle(b *testing.B) {
	text := strings.Repeat("completed line with words 界\n", 700) + "trailing words"
	b.Run("wrapText", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			liveTextBenchmarkResult = liveTextReference(text, 80)
		}
	})
	b.Run("cached", func(b *testing.B) {
		var cache liveTextCache
		cache.wrap(text, 80)
		b.ResetTimer()
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			liveTextBenchmarkResult = cache.wrap(text, 80)
		}
	})
}

func BenchmarkLiveTextCache(b *testing.B) {
	cases := []struct {
		name     string
		base     string
		newlines bool
	}{
		{"multiline", strings.Repeat("completed line with words and some Unicode 界\n", 700), true},
		{"single-line", strings.Repeat("unfinished words 界 ", 1400), false},
		{"ansi", "\x1b[31m" + strings.Repeat("styled words\n", 700), false},
	}
	for _, tc := range cases {
		frames := make([]string, 32)
		text := tc.base
		for i := range frames {
			text += " appended"
			if tc.newlines && i%8 == 7 {
				text += "\n"
			}
			frames[i] = text
		}
		b.Run(tc.name+"/wrapText", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for _, frame := range frames {
					liveTextBenchmarkResult = liveTextReference(frame, 80)
				}
			}
		})
		b.Run(tc.name+"/incremental", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				var cache liveTextCache
				for _, frame := range frames {
					liveTextBenchmarkResult = cache.wrap(frame, 80)
				}
			}
		})
	}
}
