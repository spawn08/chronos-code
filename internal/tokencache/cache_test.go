package tokencache

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/spawn08/chronos/engine/model"
)

func TestCountsMatchSDKAcrossModelsAndEdits(t *testing.T) {
	var cache Cache
	messages := []model.Message{
		{Role: model.RoleSystem, Content: "Project instructions: 日本語, café, 👩🏽‍💻"},
		{Role: model.RoleUser, Content: "Read the file."},
		{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "c1", Name: "file_read", Arguments: `{"path":"main.go"}`}}},
		{Role: model.RoleTool, Name: "file_read", ToolCallID: "c1", Content: "package main\n"},
	}
	for _, id := range []string{"gpt-4", "gpt-4o", "claude-sonnet-4-6", "gpt-4"} {
		cached, sdk := cache.ForModel(id), model.NewTokenCounter(id)
		for range 2 {
			if got, want := cached.CountTokens(messages), sdk.CountTokens(messages); got != want {
				t.Fatalf("%s: cached=%d SDK=%d", id, got, want)
			}
		}
		messages[3].Content += "func main() { println(\"updated\") }\n"
	}
	if got := cache.ForModel("gpt-4o").CountTokens(nil); got != model.NewTokenCounter("gpt-4o").CountTokens(nil) {
		t.Fatalf("empty conversation framing = %d", got)
	}
}

type countingCounter struct{ calls int }

func (c *countingCounter) CountString(s string) int        { c.calls++; return len(s) }
func (c *countingCounter) CountTokens([]model.Message) int { panic("unused") }

func TestCacheBoundsAndReuse(t *testing.T) {
	base := &countingCounter{}
	cache := Cache{modelID: "fixture", base: base}
	counter := cache.ForModel("fixture")
	counter.CountString("kept")
	counter.CountString("kept")
	if base.calls != 1 {
		t.Fatal("identical text was tokenized twice")
	}
	for i := range maxEntries + 10 {
		counter.CountString(fmt.Sprint(i))
	}
	if len(cache.entries) > maxEntries {
		t.Fatal("entry bound exceeded")
	}
	before := base.calls
	counter.CountString("kept")
	if base.calls != before+1 {
		t.Fatal("least-recently used entry was not evicted")
	}
	for i := range 20 {
		counter.CountString(strings.Repeat("x", maxEntryBytes-100) + fmt.Sprint(i))
	}
	if cache.bytes > maxBytes {
		t.Fatal("byte bound exceeded")
	}
	before = len(cache.entries)
	counter.CountString(strings.Repeat("x", maxEntryBytes+1))
	if len(cache.entries) != before {
		t.Fatal("oversized text retained")
	}
}

func TestCacheConcurrentModelBindings(t *testing.T) {
	var cache Cache
	var wg sync.WaitGroup
	for _, id := range []string{"gpt-4", "gpt-4o"} {
		counter := cache.ForModel(id)
		text := "Concurrency preserves 日本語 token counts."
		want := model.NewTokenCounter(id).CountString(text)
		for range 8 {
			wg.Go(func() {
				for range 50 {
					if got := counter.CountString(text); got != want {
						t.Errorf("%s: count=%d want=%d", id, got, want)
					}
				}
			})
		}
	}
	wg.Wait()
}
