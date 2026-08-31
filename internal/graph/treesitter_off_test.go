//go:build !treesitter

package graph

import (
	"context"
	"testing"
)

func TestSupportedTreeSitterExtensionsDefaultBuild(t *testing.T) {
	if exts := SupportedTreeSitterExtensions(); exts != nil {
		t.Fatalf("expected nil extensions in the default (non-treesitter) build, got %v", exts)
	}
}

func TestIndexNonGoFileDefaultBuildIsNoop(t *testing.T) {
	symbols, edges, err := IndexNonGoFile(context.Background(), nil, "", "foo.py")
	if err != nil || symbols != 0 || edges != 0 {
		t.Fatalf("expected (0, 0, nil) in the default build, got (%d, %d, %v)", symbols, edges, err)
	}
}
