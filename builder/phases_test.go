package builder

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/planner"
)

// The product census counts each product once by its key, however many product tests logged it: fetched from the
// store, or built (a miss, or an audit, which builds what it fetched), with every such line's seconds summed; a hit (a
// product this tree's cache already held), a cache that is off, buildcache's notes about the store and any other file
// in the logs count nothing. Mutants: a miss counted as fetched; a key counted once per line rather than once; a
// fetched line's seconds left out; a hit counted as built.
func TestTheProductCensusCountsEachProductOnceByWhatBuildcacheDid(t *testing.T) {
	logs := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(logs, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("product-0.log", "build cohere aaaaaaaaaaaa miss 12.50\nbuild grain bbbbbbbbbbbb fetched 0.25\nbuild grain_tool cccccccccccc hit 0.01\n")
	write("product-1.log", "build cohere aaaaaaaaaaaa hit 0.02\nbuild grain bbbbbbbbbbbb fetched 0.50\nstore: refs/build/dddd answered 404\n"+
		"build native eeeeeeeeeeee audited 3.00\nbuild off ffffffffffff off 9.00\n")
	write("product-2.log", "build cohere aaaaaaaaaaaa miss 1.00\n")
	write("build-tree.log", "build stray 111111111111 miss 100.00\n")
	fetched, fetchedSeconds, built, builtSeconds := ProductCensus(logs)
	if fetched != 1 || math.Abs(fetchedSeconds-0.75) > 1e-9 || built != 2 || math.Abs(builtSeconds-16.5) > 1e-9 {
		t.Fatalf("fetched %d in %.2fs, built %d in %.2fs; want 1 in 0.75s, 2 in 16.50s", fetched, fetchedSeconds, built, builtSeconds)
	}
	if fetched, _, built, _ := ProductCensus(filepath.Join(logs, "absent")); fetched != 0 || built != 0 {
		t.Fatalf("no logs counted %d fetched, %d built", fetched, built)
	}
}

// Warm times its two steps into Phases, the tests' compile even when a package doesn't compile (build-tree goes on
// after a warm that failed, and the seconds were spent), and the main packages' compile after it. Mutants: WarmTests
// recorded only when the tests compiled; WarmMains never recorded.
func TestWarmTimesItsTwoSteps(t *testing.T) {
	tree := gitTree(t, map[string]string{
		"go.mod":         "module example.com/warm\n\ngo 1.22\n",
		"good/g.go":      "package good\n\nfunc Answer() int { return 42 }\n",
		"good/g_test.go": "package good\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {}\n",
		"bad/b_test.go":  "package bad\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { undefinedThing() }\n",
		"tool/main.go":   "package main\n\nfunc main() {}\n",
	})
	phases := &TreePhases{}
	build := TreeBuild{Tree: tree, Cache: t.TempDir(), Environment: planner.GateEnvironmentList(), Jobs: 1, Compile: 1, Phases: phases}
	if err := build.Warm([]planner.ProductTest{{Package: "example.com/warm/good", Directory: "good"}}); err != nil {
		t.Fatal(err)
	}
	if phases.WarmTests <= 0 || phases.WarmMains <= 0 {
		t.Fatalf("a warm that compiled: %+v", *phases)
	}
	*phases = TreePhases{}
	if err := build.Warm([]planner.ProductTest{{Package: "example.com/warm/bad", Directory: "bad"}}); err == nil {
		t.Fatal("a package that doesn't compile warmed")
	}
	if phases.WarmTests <= 0 || phases.WarmMains != 0 {
		t.Fatalf("a warm whose tests didn't compile: %+v", *phases)
	}
}

// PublishTree times what it sends into the store's Phases, each kind on its own: the source's chunks, the products,
// the binaries and the index; a Store with no Phases times nothing and publishes the same. Mutants: the products'
// seconds never recorded; the index's never recorded; a nil Phases dereferenced.
func TestPublishTreeTimesEachKindItSends(t *testing.T) {
	tree := gitTree(t, map[string]string{"go.mod": "module example.com/tree\n", "a/a.go": "package a\n"})
	source, err := SourceChunks(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	product := strings.Repeat("c", 64)
	work := t.TempDir()
	binaries, cache := filepath.Join(work, "out"), filepath.Join(work, "cache")
	os.MkdirAll(binaries, 0o755)
	os.MkdirAll(filepath.Join(cache, product), 0o755)
	os.WriteFile(filepath.Join(cache, product, "tool"), []byte("a tool"), 0o755)
	os.WriteFile(filepath.Join(binaries, "example.com_tree_a.test"), []byte("a test binary"), 0o755)
	index := func() TreeIndex {
		return TreeIndex{Tree: "0123456789abcdef0123456789abcdef01234567", Future: "f", Go: "go1.27.1",
			Packages: map[string]TreePackage{"example.com/tree/a": {Package: "example.com/tree/a", Directory: "a", Products: []string{product}}}}
	}
	_, store := serve(t)
	store.Phases = &TreePhases{}
	published := index()
	if _, written, err := PublishTree(store, &published, binaries, cache, &source, nil); err != nil || !written {
		t.Fatal(written, err)
	}
	if phases := *store.Phases; phases.UploadChunks <= 0 || phases.UploadProducts <= 0 || phases.UploadBinaries <= 0 || phases.UploadIndex <= 0 {
		t.Fatalf("the upload's phases: %+v", phases)
	}
	store.Phases = nil
	again := index()
	if _, _, err := PublishTree(store, &again, binaries, cache, &source, nil); err != nil {
		t.Fatalf("a store with no phases: %v", err)
	}
}
