package plan

import (
	"errors"
	"reflect"
	"testing"
)

func TestContextBuildIsBoundedAndDeterministic(t *testing.T) {
	entries := []ContextEntry{
		{ID: "b", Content: "bb"},
		{ID: "a", Content: "aa"},
		{ID: "c", Content: "ccc"},
	}

	first, err := BuildRestartContext(entries, 4, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildRestartContext([]ContextEntry{entries[2], entries[0], entries[1]}, 4, first.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	want := []ContextEntry{{ID: "a", Content: "aa"}, {ID: "b", Content: "bb"}}
	if !reflect.DeepEqual(first.Entries, want) || !reflect.DeepEqual(second.Entries, want) {
		t.Fatalf("restart entries = %#v, %#v; want %#v", first.Entries, second.Entries, want)
	}
	if !first.Truncated || !second.Truncated || first.Fingerprint != second.Fingerprint {
		t.Fatalf("restart contexts = %#v, %#v", first, second)
	}
}

func TestContextBlocksStaleFingerprint(t *testing.T) {
	context, err := BuildRestartContext([]ContextEntry{{ID: "context", Content: "original"}}, 8, "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = BuildRestartContext([]ContextEntry{{ID: "context", Content: "changed"}}, 8, context.Fingerprint)
	if !errors.Is(err, ErrStaleContextFingerprint) {
		t.Fatalf("BuildRestartContext() error = %v, want %v", err, ErrStaleContextFingerprint)
	}
}
