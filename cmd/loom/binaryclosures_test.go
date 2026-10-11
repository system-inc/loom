package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/resident"
)

// Build-tree keys a binary on the resident's closure when its keys name the package, on a cold one read from the tree
// when they don't, and not at all when the resident couldn't list it; with no keys, every closure is cold. Mutants:
// a package the keys don't name keyed on nothing; the resident's error read as a closure; its key ignored for a cold
// read.
func TestABinaryIsKeyedOnTheResidentsClosureWhenItHasOne(t *testing.T) {
	if binaryClosures(nil) != nil {
		t.Fatal("no keys read a closure other than cold")
	}
	tree := t.TempDir()
	for name, content := range map[string]string{"go.mod": "module example.com/closures\n\ngo 1.22\n", "p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n"} {
		os.MkdirAll(filepath.Dir(filepath.Join(tree, name)), 0o755)
		os.WriteFile(filepath.Join(tree, name), []byte(content), 0o644)
	}
	warm := strings.Repeat("a", 64)
	closure := binaryClosures(&resident.Keys{Closures: map[string]resident.Closure{
		"example.com/closures/warm":   {Key: warm},
		"example.com/closures/broken": {Error: "go list: no Go files"},
	}})
	if key, err := closure(tree, "example.com/closures/warm"); err != nil || key != warm {
		t.Fatalf("the resident's closure: %q %v", key, err)
	}
	if key, err := closure(tree, "example.com/closures/broken"); err == nil || !strings.Contains(err.Error(), "no Go files") {
		t.Fatalf("a closure the resident couldn't list: %q %v", key, err)
	}
	cold, err := planner.Closure(tree, "example.com/closures/p")
	if err != nil {
		t.Fatal(err)
	}
	if key, err := closure(tree, "example.com/closures/p"); err != nil || key != cold {
		t.Fatalf("a package the keys don't name: %q %v, cold %q", key, err, cold)
	}
}
