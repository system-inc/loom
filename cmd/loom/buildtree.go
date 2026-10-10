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
func buildTree(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("build-tree", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	tree := flags.String("tree", "", "the future's checked-out tree")
	future := flags.String("future", "", "the future's commit")
	storeFlags := addStoreFlags(flags)
	cache := flags.String("cache", filepath.Join(home, "loom-builder", "trees"), "the base of each tree's own build directory, <base>/<tree hash>")
	keep := flags.Int("keep", 2, "tree directories kept under --cache, newest first")
	jobs := flags.Int("jobs", 8, "packages built at once")
	compile := flags.Int("compile", 0, "packages compiled at once across every go process (0: every thread but four)")
	if err := flags.Parse(arguments); err != nil || *tree == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom build-tree --tree <dir> [--future <sha>] [--r2 <key file>] [--bucket <name>] [--cache <dir>] [--keep N] [--jobs N] [--compile N]")
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
	directory, err := builder.TreeCache(*cache, *tree, *keep)
	if err != nil {
		return fail(err)
	}
	treeHash := filepath.Base(directory)
	goVersion, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return fail(err)
	}
	build := builder.TreeBuild{Tree: *tree, Cache: filepath.Join(directory, "cache"), Out: filepath.Join(directory, "out"), Environment: builder.GateEnvironment(), Jobs: *jobs, Compile: *compile}
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
	defer os.RemoveAll(scratch)
	held, err := builder.ServeHeldProducts(store, scratch)
	if err != nil {
		return fail(err)
	}
	build.Held = held.Address
	productsStarted := time.Now()
	products, productFailures := build.Products(productTests, filepath.Join(directory, "logs"))
	productSeconds := time.Since(productsStarted).Seconds()
	held.Close()
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
	treeKey, err := builder.PublishTree(store, &treeIndex, build.Out, build.Cache, source, held.Held())
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
		"tree": treeHash, "future": *future, "treeKey": treeKey, "index": "trees/" + treeKey + ".json",
		"packages": len(packages), "failed": failed, "productTests": len(productTests), "products": len(treeIndex.Products), "productsFetched": len(held.Held()),
		"warmSeconds": warmSeconds, "productSeconds": productSeconds, "binarySeconds": binarySeconds, "uploadSeconds": time.Since(uploadStarted).Seconds(),
		"seconds": time.Since(started).Seconds(), "sourceBytes": len(source),
		"storeReads": requests.Reads.Load(), "storeWrites": requests.Writes.Load(),
	})
	if failed > 0 {
		return 1
	}
	return 0
}
