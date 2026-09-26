package query

import "testing"

func TestCastType(t *testing.T) {
	for in, want := range map[string]string{
		"((TextNode) node)":    "TextNode",
		"((const Foo*) p)":     "Foo",
		"((ns::Bar) x)":        "ns::Bar",
		"(x as Store)":         "Store",
		"(x as Store?)":        "Store",
		"((x))":                "",
		"(f(a))":               "",
		"((a + b) * c)":        "",
		"(node)":               "",
		"((List<Item>) items)": "List",
	} {
		if got := castType(in); got != want {
			t.Errorf("castType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsTypeParam(t *testing.T) {
	for _, tc := range []struct {
		sig, name, t string
		want         bool
	}{
		{"inline fun <reified T : Any> mock(extraInterfaces: Array<KClass<out Any>>? = null): T", "mock", "T", true},
		{"public <T> T fromJson(String json, Class<T> type)", "fromJson", "T", true},
		{"function first<T>(items: T[]): T", "first", "T", true},
		{"template <typename Item> Item max_of(Item a, Item b)", "max_of", "Item", true},
		{"fun build(): Store", "build", "Store", false},
		{"public Map<String, User> users()", "users", "Map", false},
		{"fun <T> KStubbing<T>.onBlocking(m: suspend T.() -> R): OngoingStubbing<R>", "onBlocking", "OngoingStubbing", false},
	} {
		if got := isTypeParam(tc.sig, tc.name, tc.t); got != tc.want {
			t.Errorf("isTypeParam(%q, %q) = %v, want %v", tc.sig, tc.t, got, tc.want)
		}
	}
}
