package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
)

// buildTree is Kirk's shape on Workshop: one future's tree built cold, every test package's binary and the products
// its tests use, with the tree's source, all uploaded straight to R2 so a runner only downloads and runs. It prints
// one JSON line per package and the tree's summary, and exits 1 when any package failed to build.
//
// Workshop is the house's only builder, so build-tree keeps its disk and its threads (#ckv0pmg, #eqebk7b): it trims
// Go's build cache under --go-cache-gb, refuses to start while any filesystem it writes has under --floor-gb free and
// starts no job while one does, and removes the tree's working directory once its index is up.
func buildTree(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("build-tree", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	tree := flags.String("tree", "", "the future's checked-out tree")
	future := flags.String("future", "", "the future's commit")
	storeFlags := addStoreFlags(flags)
	cache := flags.String("cache", filepath.Join(home, "loom-builder", "trees"), "the base of each tree's own build directory, <base>/<tree hash>")
	keep := flags.Int("keep", 2, "tree directories kept under --cache, newest first, when a tree's upload fails")
	jobs := flags.Int("jobs", 8, "packages built at once")
	compile := flags.Int("compile", 0, "packages compiled at once across every go process (0: every thread but four)")
	floorGB := flags.Uint64("floor-gb", 200, "free space the cache base and Go's build cache keep, in GB: below it the build doesn't start, and no job starts")
	tempFloorGB := flags.Uint64("temp-floor-gb", 20, "free space the temporary directory keeps, in GB (it may be memory)")
	goCacheGB := flags.Uint64("go-cache-gb", 150, "the most Go's build cache may hold before a build, in GB; over it the least recently used go first")
	if err := flags.Parse(arguments); err != nil || *tree == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom build-tree --tree <dir> [--future <sha>] [--r2 <key file>] [--bucket <name>] [--cache <dir>] [--keep N] [--jobs N] [--compile N] [--floor-gb N] [--temp-floor-gb N] [--go-cache-gb N]")
		return 2
	}
	started := time.Now()
	fail := func(err error) int {
		fmt.Fprintln(stderr, "build-tree:", err)
		return 1
	}
	requests := &builder.Requests{}
	store, err := storeFlags.open(requests)
	if err != nil {
		return fail(err)
	}
	if err = os.MkdirAll(*cache, 0o755); err != nil {
		return fail(err)
	}
	goCache, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		return fail(err)
	}
	floor, err := builder.Gigabytes(*floorGB)
	if err != nil {
		return fail(err)
	}
	tempFloor, err := builder.Gigabytes(*tempFloorGB)
	if err != nil {
		return fail(err)
	}
	goCacheCap, err := builder.Gigabytes(*goCacheGB)
	if err != nil {
		return fail(err)
	}
	trimmed, err := builder.TrimGoCache(strings.TrimSpace(string(goCache)), goCacheCap)
	if err != nil {
		return fail(fmt.Errorf("trimming Go's build cache: %w", err))
	}
	if trimmed > 0 {
		fmt.Fprintf(stderr, "build-tree: trimmed %.1f GB from Go's build cache, least recently used first\n", float64(trimmed)/float64(builder.GB))
	}
	// The temporary directory keeps a smaller floor of its own: it may be memory.
	watched := map[string]builder.Watch{"the cache base": {Path: *cache, Floor: floor}, "the temporary directory": {Path: os.TempDir(), Floor: tempFloor}}
	// GOCACHE=off has no cache to trim or watch, and stops nothing.
	if strings.TrimSpace(string(goCache)) != "off" {
		watched["Go's build cache"] = builder.Watch{Path: strings.TrimSpace(string(goCache)), Floor: floor}
	}
	if err = builder.CheckFloor(watched, nil); err != nil {
		return fail(fmt.Errorf("not starting: %w", err))
	}
	directory, err := builder.TreeCache(*cache, *tree, *keep)
	if err != nil {
		return fail(err)
	}
	treeHash := filepath.Base(directory)
	goVersion, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return fail(err)
	}
	build := builder.TreeBuild{Tree: *tree, Cache: filepath.Join(directory, "cache"), Out: filepath.Join(directory, "out"), Environment: builder.GateEnvironment(),
		Jobs: *jobs, Compile: *compile, Watched: watched}
	for _, path := range []string{build.Cache, build.Out, filepath.Join(directory, "logs")} {
		if err = os.MkdirAll(path, 0o755); err != nil {
			return fail(err)
		}
	}
	packages, err := builder.TestPackages(*tree)
	if err != nil {
		return fail(err)
	}
	productTests, err := planner.ListProductTests(*tree, nil)
	if err != nil {
		return fail(err)
	}
	warmStarted := time.Now()
	if err = build.Warm(packages); err != nil {
		// A package that doesn't compile is named again by its own binary below; the rest are warm.
		fmt.Fprintf(stderr, "loom: warming the tree: %v\n", err)
	}
	warmSeconds := time.Since(warmStarted).Seconds()
	// The store's products are offered to buildcache before it builds one, so only what the store lacks is built.
	scratch, err := os.MkdirTemp(directory, "held-")
	if err != nil {
		return fail(err)
	}
	held, err := builder.ServeHeldProducts(store, scratch)
	if err != nil {
		return fail(err)
	}
	build.Held = held.Address
	productsStarted := time.Now()
	products, productFailures := build.Products(productTests, filepath.Join(directory, "logs"))
	productSeconds := time.Since(productsStarted).Seconds()
	held.Close()
	for _, note := range held.Notes() {
		fmt.Fprintln(stderr, "build-tree:", note)
	}
	if err = held.Err(); err != nil {
		return fail(fmt.Errorf("the store held products it couldn't give whole: %w", err))
	}
	build.Held = ""
	binariesStarted := time.Now()
	built := build.Binaries(packages)
	binarySeconds := time.Since(binariesStarted).Seconds()
	source, err := builder.SourceArchive(*tree)
	if err != nil {
		return fail(err)
	}
	treeIndex := builder.TreeIndex{Tree: treeHash, Future: *future, Go: strings.TrimSpace(string(goVersion)), Packages: map[string]builder.TreePackage{}}
	for _, result := range built {
		result.Products = products[result.Package]
		if result.Products == nil {
			result.Products = []string{}
		}
		if failure, broke := productFailures[result.Package]; broke && result.Error == "" {
			result.Error = "product tests: " + failure
		}
		treeIndex.Packages[result.Package] = result
	}
	uploadStarted := time.Now()
	treeIndex.Seconds = time.Since(started).Seconds()
	treeKey, indexWritten, err := builder.PublishTree(store, &treeIndex, build.Out, build.Cache, source, held.Held())
	if err != nil {
		return fail(err)
	}
	// A package fails here when it didn't build, or when a product it reads conflicts with the store's ref.
	failed := 0
	encoder := json.NewEncoder(stdout)
	for _, result := range built {
		if treeIndex.Packages[result.Package].Error != "" {
			failed++
		}
		encoder.Encode(treeIndex.Packages[result.Package])
	}
	encoder.Encode(map[string]any{
		"tree": treeHash, "future": *future, "treeKey": treeKey, "index": "trees/" + treeKey + ".json", "indexWritten": indexWritten,
		"packages": len(packages), "failed": failed, "productTests": len(productTests), "products": len(treeIndex.Products), "productsFetched": len(held.Held()),
		"warmSeconds": warmSeconds, "productSeconds": productSeconds, "binarySeconds": binarySeconds, "uploadSeconds": time.Since(uploadStarted).Seconds(),
		"seconds": time.Since(started).Seconds(), "sourceBytes": len(source),
		"storeReads": requests.Reads.Load(), "storeWrites": requests.Writes.Load(),
	})
	return finishTree(stderr, *cache, directory, treeKey, failed, indexWritten)
}

// finishTree removes the tree's working directory once its index is up (kept otherwise, for a retry) and exits. A
// removal that fails is warned about and left for the next TreeCache to sweep, and never fails the build: the index
// is up, and that is what runners read.
func finishTree(stderr io.Writer, cache, directory, treeKey string, failed int, indexWritten bool) int {
	if err := builder.TreeDone(cache, directory, indexWritten); err != nil {
		fmt.Fprintln(stderr, "build-tree: warning: removing the tree's directory:", err)
	}
	return buildTreeExit(stderr, treeKey, failed, indexWritten)
}

// buildTreeExit is build-tree's exit: 1 when a package failed, or when the store kept an earlier build's index, so
// what runners read isn't this build, said aloud rather than left for someone to notice.
func buildTreeExit(stderr io.Writer, treeKey string, failed int, indexWritten bool) int {
	if !indexWritten {
		fmt.Fprintf(stderr, "build-tree: trees/%s.json keeps an earlier build's index, which failed fewer packages; this build's isn't what runners read\n", treeKey)
		return 1
	}
	if failed > 0 {
		return 1
	}
	return 0
}
