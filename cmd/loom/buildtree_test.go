package main

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
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

// build-tree refuses a tree that keys otherwise than the plan's tree key, a go that isn't the release the plan's
// units are keyed on, and a machine that isn't the runners' platform, naming each, before anything is built: an index
// under another key is one no unit of the plan would read. No key or release asked, or the same ones, builds. Mutants:
// the key check dropped; the release check dropped; the platform check dropped.
func TestBuildTreeRefusesATreeThatKeysOtherwiseThanThePlan(t *testing.T) {
	identity := planner.TreeIdentity{Tree: strings.Repeat("a", 40), Go: "go1.27.1", Goos: "linux", Goarch: "amd64"}
	if err := checkTreeKey(identity, "", "", "linux/amd64"); err != nil {
		t.Fatalf("no key asked: %v", err)
	}
	if err := checkTreeKey(identity, identity.Key(), "go1.27.1", "linux/amd64"); err != nil {
		t.Fatalf("the same key and release: %v", err)
	}
	other := identity
	other.Goos, other.Goarch = "darwin", "arm64"
	if err := checkTreeKey(identity, other.Key(), "", "linux/amd64"); err == nil || !strings.Contains(err.Error(), other.Key()) || !strings.Contains(err.Error(), identity.Key()) ||
		!strings.Contains(err.Error(), "linux/amd64") {
		t.Fatalf("a tree keying for another platform: %v", err)
	}
	if err := checkTreeKey(identity, identity.Key(), "go1.27.2", "linux/amd64"); err == nil || !strings.Contains(err.Error(), "keyed on go1.27.2, and this go is go1.27.1") {
		t.Fatalf("another release: %v", err)
	}
	if err := checkTreeKey(identity, identity.Key(), "go1.27.1", "darwin/arm64"); err == nil || !strings.Contains(err.Error(), "this is darwin/arm64") {
		t.Fatalf("another host: %v", err)
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
