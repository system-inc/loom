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
// its tests use, with the tree's source, all uploaded so a runner only downloads and runs. It prints one JSON line
// per package and the tree's summary, and exits 1 when any package failed to build.
func buildTree(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("build-tree", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	tree := flags.String("tree", "", "the future's checked-out tree")
	future := flags.String("future", "", "the future's commit")
	read := flags.String("read", "https://adamic-store.kirkouimet.com", "the public store, read direct")
	write := flags.String("write", "", "loom-pipeline's action store, https://<pipeline>/actions")
	tokenFile := flags.String("token-file", filepath.Join(home, ".loom", "build-token"), "file holding this builder's build token")
	cache := flags.String("cache", filepath.Join(home, "loom-builder", "trees"), "the base of each tree's own build directory, <base>/<tree hash>")
	keep := flags.Int("keep", 2, "tree directories kept under --cache, newest first")
	indexDirectory := flags.String("index", filepath.Join(home, "loom-builder", "index"), "Workshop's index of what the store holds")
	jobs := flags.Int("jobs", 8, "packages built at once")
	if err := flags.Parse(arguments); err != nil || *tree == "" || *write == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom build-tree --tree <dir> --write <https://pipeline/actions> [--future <sha>] [--cache <dir>] [--keep N] [--jobs N] [--index <dir>] [--token-file <path>]")
		return 2
	}
	started := time.Now()
	fail := func(err error) int {
		fmt.Fprintln(stderr, "build-tree:", err)
		return 1
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		return fail(err)
	}
	index, err := builder.OpenIndex(*indexDirectory)
	if err != nil {
		return fail(err)
	}
	defer index.Close()
	directory, err := builder.TreeCache(*cache, *tree, *keep)
	if err != nil {
		return fail(err)
	}
	treeHash := filepath.Base(directory)
	goVersion, err := exec.Command("go", "env", "GOVERSION").Output()
	if err != nil {
		return fail(err)
	}
	build := builder.TreeBuild{Tree: *tree, Cache: filepath.Join(directory, "cache"), Out: filepath.Join(directory, "out"), Environment: builder.GateEnvironment(), Jobs: *jobs}
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
	productsStarted := time.Now()
	products, productFailures := build.Products(productTests, filepath.Join(directory, "logs"))
	productSeconds := time.Since(productsStarted).Seconds()
	binariesStarted := time.Now()
	built := build.Binaries(packages)
	binarySeconds := time.Since(binariesStarted).Seconds()
	source := filepath.Join(directory, "source.tar")
	if err = builder.SourceArchive(*tree, source); err != nil {
		return fail(err)
	}
	treeIndex := builder.TreeIndex{Tree: treeHash, Future: *future, Go: strings.TrimSpace(string(goVersion)), Packages: map[string]builder.TreePackage{}}
	failed := 0
	for _, result := range built {
		result.Products = products[result.Package]
		if result.Products == nil {
			result.Products = []string{}
		}
		if failure, broke := productFailures[result.Package]; broke && result.Error == "" {
			result.Error = "product tests: " + failure
		}
		if result.Error != "" {
			failed++
		}
		treeIndex.Packages[result.Package] = result
	}
	requests := &builder.Requests{}
	store := builder.Store{Read: strings.TrimSuffix(*read, "/"), Write: strings.TrimSuffix(*write, "/"), Token: strings.TrimSpace(string(token)), Requests: requests}
	uploadStarted := time.Now()
	treeIndex.Seconds = time.Since(started).Seconds()
	manifest, err := builder.PublishTree(store, index, treeIndex, build.Out, build.Cache, source)
	if err != nil {
		return fail(err)
	}
	encoder := json.NewEncoder(stdout)
	for _, result := range built {
		encoder.Encode(treeIndex.Packages[result.Package])
	}
	sourceInfo, _ := os.Stat(source)
	encoder.Encode(map[string]any{
		"tree": treeHash, "future": *future, "treeKey": builder.TreeKey(treeHash, treeIndex.Go, builder.GateEnvironment()), "indexManifest": manifest,
		"packages": len(packages), "failed": failed, "productTests": len(productTests),
		"productSeconds": productSeconds, "binarySeconds": binarySeconds, "uploadSeconds": time.Since(uploadStarted).Seconds(),
		"seconds": time.Since(started).Seconds(), "sourceBytes": sourceInfo.Size(),
		"storeReads": requests.Reads.Load(), "storeWrites": requests.Writes.Load(),
	})
	if failed > 0 {
		return 1
	}
	return 0
}
