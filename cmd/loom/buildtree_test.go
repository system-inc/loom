package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/builder"
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

// Once the index is up, a tree directory that won't go is a warning, never a failed build; one whose index is up
// goes, and one whose index didn't stays.
func TestATreeThatWontGoIsAWarningOnceItsIndexIsUp(t *testing.T) {
	base := t.TempDir()
	tree := filepath.Join(base, strings.Repeat("a", 40))
	os.MkdirAll(filepath.Join(tree, "out"), 0o755)
	var stderr bytes.Buffer
	if code := finishTree(&stderr, base, tree, "k", 0, false, nil); code != 1 {
		t.Fatalf("a kept index: %d", code)
	}
	if _, err := os.Stat(tree); err != nil {
		t.Fatal("a tree whose index didn't go up was removed")
	}
	stderr.Reset()
	if code := finishTree(&stderr, base, filepath.Join(base, "not-a-tree"), "k", 0, true, nil); code != 0 || !strings.Contains(stderr.String(), "warning: removing the tree's directory") {
		t.Fatalf("a removal that fails after the index is up: %d %q", code, stderr.String())
	}
	if code := finishTree(&stderr, base, tree, "k", 0, true, nil); code != 0 {
		t.Fatalf("a clean finish: %d", code)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Fatal("the tree's directory is still there")
	}
}

// The cache base and Go's build cache keep the floor, the temporary directory its own, and GOCACHE=off watches none.
func TestBuildTreeWatchesEachFilesystemAtItsFloor(t *testing.T) {
	watched := buildTreeWatches("/trees", "/gocache", "/tmp", 200, 20)
	want := map[string]builder.Watch{"the cache base": {Path: "/trees", Floor: 200}, "Go's build cache": {Path: "/gocache", Floor: 200}, "the temporary directory": {Path: "/tmp", Floor: 20}}
	if !maps.Equal(watched, want) {
		t.Fatalf("%v", watched)
	}
	if _, watching := buildTreeWatches("/trees", "off", "/tmp", 200, 20)["Go's build cache"]; watching {
		t.Fatal("GOCACHE=off was watched")
	}
}
