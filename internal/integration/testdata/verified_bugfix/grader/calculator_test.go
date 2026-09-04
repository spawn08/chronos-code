package grader_test

import (
	"testing"

	seededbug "example.com/seededbug"
)

func TestAdd(t *testing.T) {
	if got := seededbug.Add(7, 5); got != 12 {
		t.Fatalf("Add(7, 5) = %d, want 12", got)
	}
}
