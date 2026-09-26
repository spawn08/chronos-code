package query_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/spawn08/chronos-code/indexer/facts"
	"github.com/spawn08/chronos-code/indexer/query"
)

const designDoc = `---
title: Store design
---
Intro text before any heading mentions ` + "`NewRepo`" + `.

# Store

The store saves orders through ` + "`Repo.Save`" + ` and ` + "`memRepo`" + `.
See [the store](../store/store.go) and store/store.go for details.

## HTTP API

Clients call GET /orders/{id}; see https://shop.example.com/orders/42 too.
Tracked in SHOP-123.

` + "```go" + `
func notAMention() { ` + "`Handle`" + ` }
` + "```" + `

Bare identifiers like parseHTTPRequest and max_tokens link weakly; plain words do not.

Setext heading
--------------

Mentions ` + "`nothing_here`" + `.
`

func TestDocumentSectionsAndMentions(t *testing.T) {
	files := map[string]string{"docs/design.md": designDoc, "notes/todo.txt": "Follow up on SHOP-123 in Handle.\n"}
	for k, v := range shopFiles {
		files[k] = v
	}
	files["api/server.go"] = "package api\n\nimport \"net/http\"\n\nfunc getOrder(w http.ResponseWriter, r *http.Request) {}\n\nfunc Mount(m *http.ServeMux) { m.HandleFunc(\"GET /orders/{id}\", getOrder) }\n"
	v := newFixture(t, files).view(t)

	secs := v.FileSymbols("docs/design.md")
	var names []string
	for _, s := range secs {
		if s.Kind != facts.KindSection {
			t.Fatalf("non-section symbol in a document: %+v", s)
		}
		names = append(names, s.Name)
	}
	if want := []string{"design.md", "Store", "HTTP API", "Setext heading"}; !sameSet(names, want) {
		t.Fatalf("sections = %q, want %q", names, want)
	}
	api := section(t, v, "HTTP API")
	if api.Signature != "Store > HTTP API" || api.Parent != "Store" || !strings.Contains(api.Doc, "SHOP-123") {
		t.Errorf("HTTP API section = %+v", api)
	}
	store := section(t, v, "Store")
	if store.Line != 6 || store.EndLine != 26 {
		t.Errorf("Store spans %d-%d, want 6-26 (with its subsections)", store.Line, store.EndLine)
	}

	links := map[string]string{}
	for _, name := range []string{"design.md", "Store", "HTTP API", "Setext heading"} {
		for _, l := range v.Mentions(section(t, v, name)) {
			var targets []string
			for _, s := range l.Targets {
				targets = append(targets, s.Qualified())
			}
			slices.Sort(targets)
			links[name+": "+l.Kind+" "+l.Text] = l.Resolution + " " + strings.Join(targets, ",")
		}
	}
	expect(t, links, map[string]string{
		"design.md: symbol NewRepo":           "name_matched NewRepo",
		"Store: symbol Repo.Save":             "name_matched Repo.Save",
		"Store: symbol memRepo":               "name_matched memRepo",
		"Store: path ../store/store.go":       "import_resolved Order,Repo,base,base.Close,memRepo,memRepo.Load,memRepo.Save,memRepo.helper",
		"Store: path store/store.go":          "import_resolved Order,Repo,base,base.Close,memRepo,memRepo.Load,memRepo.Save,memRepo.helper",
		"HTTP API: route GET /orders/{}":      "import_resolved GET /orders/{}",
		"HTTP API: route ANY /orders/{}":      "import_resolved GET /orders/{}",
		"HTTP API: ticket SHOP-123":           "name_matched todo.txt",
		"HTTP API: ident parseHTTPRequest":    "name_matched parseHTTPRequest",
		"HTTP API: ident max_tokens":          " ",
		"Setext heading: symbol nothing_here": " ",
	})
	for k := range links {
		if strings.Contains(k, "Handle") || strings.Contains(k, "notAMention") {
			t.Errorf("mention inside a code fence: %s", k)
		}
	}

	// Code → documents: by name and by the declaring file's path.
	save := v.Symbols("Repo.Save", "")[0]
	var how []string
	for _, l := range v.MentionedBy(save) {
		how = append(how, l.Section.Name+" "+l.Kind+" "+l.Text)
	}
	if want := []string{"Store symbol Repo.Save", "Store path store/store.go"}; !sameSet(how, want) {
		t.Errorf("mentions of Repo.Save = %q, want %q", how, want)
	}
	route := v.Symbols("GET /orders/{}", facts.KindRoute)[0]
	if got := linkSections(v.MentionedBy(route)); !slices.Equal(got, []string{"HTTP API", "HTTP API"}) {
		t.Errorf("sections mentioning the route = %v", got)
	}
	newRepo := v.Symbols("NewRepo", "")[0]
	if got := linkSections(v.MentionedBy(newRepo)); !slices.Equal(got, []string{"Store", "design.md"}) {
		t.Errorf("sections mentioning NewRepo (by name or file) = %v", got)
	}

	// Sections are found by search, by their heading and their text.
	found := false
	for _, h := range v.Search("store saves orders", 5) {
		found = found || h.Symbol.Name == "Store" && h.Symbol.Kind == facts.KindSection
	}
	if !found {
		t.Errorf("search misses the Store section")
	}
	found = false
	for _, h := range v.Search("SHOP-123 follow up", 5) {
		found = found || h.Symbol.File == "notes/todo.txt"
	}
	if !found {
		t.Errorf("search misses the text file")
	}
}

func section(t *testing.T, v *query.View, name string) query.Symbol {
	t.Helper()
	got := v.Symbols(name, facts.KindSection)
	if len(got) != 1 {
		t.Fatalf("section %q: %+v", name, got)
	}
	return got[0]
}

func linkSections(links []query.DocLink) []string {
	var out []string
	for _, l := range links {
		out = append(out, l.Section.Name)
	}
	slices.Sort(out)
	return out
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}
