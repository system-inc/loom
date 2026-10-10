package main

import (
	"bytes"
	"strings"
	"testing"
)

// build-tree exits 1 when a package failed, and when the store kept an earlier build's index, saying so.
func TestBuildTreeExitsNonZeroWhenTheIndexWasKept(t *testing.T) {
	var stderr bytes.Buffer
	if code := buildTreeExit(&stderr, "k", 0, true); code != 0 || stderr.Len() != 0 {
		t.Fatalf("a clean build: %d %q", code, stderr.String())
	}
	if code := buildTreeExit(&stderr, "k", 2, true); code != 1 {
		t.Fatalf("a build with failed packages: %d", code)
	}
	if code := buildTreeExit(&stderr, "k", 0, false); code != 1 || !strings.Contains(stderr.String(), "trees/k.json keeps an earlier build's index") {
		t.Fatalf("a kept index: %d %q", code, stderr.String())
	}
}
