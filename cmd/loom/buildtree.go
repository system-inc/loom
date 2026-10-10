package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/resident"
)

// buildTree is Kirk's shape on Workshop: one future's tree built cold, every test package's binary and the products
// its tests use, with the tree's source, all uploaded straight to R2 so a runner only downloads and runs. It prints
// one JSON line per package and the tree's summary, and exits 1 when any package failed to build.
//
// Workshop is the house's only builder, so build-tree keeps its disk and its threads (#ckv0pmg, #eqebk7b): it trims
// Go's build cache under --go-cache-gb, refuses to start while any filesystem it writes has under --floor-gb free and
// starts no job while one does, and removes the tree's working directory once its index is up.
//
// --tree-key is the key the plan carries for the tree (`loom build-trees` passes it): a tree that keys otherwise here
// (another tree hash, Go release or platform than the planner read) is refused before anything is built, naming both,
// since an index under another key is one no unit of the plan would ever read.
func buildTree(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("build-tree", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	tree := flags.String("tree", "", "the future's checked-out tree")
	future := flags.String("future", "", "the future's commit")
	wantKey := flags.String("tree-key", "", "the tree key the plan carries for this tree: refused before building when the tree keys otherwise")
	wantGo := flags.String("go", "", "the Go release the plan's units are keyed on: refused before building when this go is another")
	keysFile := flags.String("keys", "", "the resident's keys for this tree (`loom build-trees --resident`): its test packages and closure keys, read instead of asked of go again; refused when they're another tree's")
	storeFlags := addStoreFlags(flags)
	cache := flags.String("cache", filepath.Join(home, "loom-builder", "trees"), "the base of each tree's own build directory, <base>/<tree hash>")
	nodeCache := flags.String("node-cache", filepath.Join(home, "loom-builder", "node"), "where each npm project's packages are installed once per lockfile, <base>/<lockfile sha256>, and the pinned npm")
	keep := flags.Int("keep", 2, "tree directories kept under --cache, newest first, when a tree's upload fails")
	jobs := flags.Int("jobs", 8, "packages built at once")
	compile := flags.Int("compile", 0, "packages compiled at once across every go process (0: every thread but four)")
	floorGB := flags.Uint64("floor-gb", 100, "free space the cache base and Go's build cache keep, in GB: below it the build doesn't start, and no job starts")
	tempFloorGB := flags.Uint64("temp-floor-gb", 20, "free space the temporary directory keeps, in GB (it may be memory)")
	goCacheGB := flags.Uint64("go-cache-gb", 500, "the most Go's build cache may hold before a build, in GB; over it the least recently used go first")
	if err := flags.Parse(arguments); err != nil || *tree == "" || flags.NArg() != 0 {
		fmt.Fprintln(stderr, "usage: loom build-tree --tree <dir> [--future <sha>] [--tree-key <key>] [--go <release>] [--keys <file>] [--r2 <key file>] [--bucket <name>] [--cache <dir>] [--node-cache <dir>] [--keep N] [--jobs N] [--compile N] [--floor-gb N] [--temp-floor-gb N] [--go-cache-gb N]")
		return 2
	}
	started := time.Now()
	// Every phase is timed, into the summary line and the live status (#s0cqqhk).
	clock := newTreeClock(started)
	// The build's live status, for `loom top`: its tree, its phase, its products built and hit.
	var live *livestatus.Writer
	defer func() { live.Close() }()
	phase := func(phase string, change func(tree *livestatus.Tree)) {
		live.Update(func(status *livestatus.Status) {
			status.Tree.Phase = phase
			if change != nil {
				change(status.Tree)
			}
		})
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "build-tree:", err)
		phase("failed", nil)
		return 1
	}
	// The tree's identity, by the one function the planner keys its plan's trees with, read before anything is built.
	identity, err := planner.ReadTreeIdentity(*tree)
	if err != nil {
		return fail(err)
	}
	live = livestatus.NewWriter(livestatus.TreePath(*cache), livestatus.Status{Kind: livestatus.KindTree, StartedAt: started,
		Tree: &livestatus.Tree{Key: identity.Key(), Future: *future, Phase: "readying", StartedAt: started}})
	clock.live = live
	if err = checkTreeKey(identity, *wantKey, *wantGo, runtime.GOOS+"/"+runtime.GOARCH); err != nil {
		return fail(err)
	}
	// The resident's keys, read before anything is built, so keys of another tree refuse the build rather than name it.
	var keys *resident.Keys
	if *keysFile != "" {
		read, err := resident.ReadKeys(*keysFile, identity.Tree)
		if err != nil {
			return fail(err)
		}
		keys = &read
	}
	requests := &builder.Requests{}
	store, err := storeFlags.open(requests)
	if err != nil {
		return fail(err)
	}
	store.Phases = &clock.phases
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
	watched := buildTreeWatches(*cache, strings.TrimSpace(string(goCache)), os.TempDir(), floor, tempFloor)
	// What killed removals left goes before the floor is read, or it could keep every later build from starting.
	if err = builder.Ready(*cache, watched, nil); err != nil {
		return fail(fmt.Errorf("not starting: %w", err))
	}
	directory, err := builder.TreeCache(*cache, *tree, *keep)
	if err != nil {
		return fail(err)
	}
	// The tree is this build's until it ends: no other build-tree removes it meanwhile.
	treeLock, err := builder.LockTree(directory)
	if err != nil {
		return fail(err)
	}
	defer treeLock.Close()
	build := builder.TreeBuild{Tree: *tree, Cache: filepath.Join(directory, "cache"), Out: filepath.Join(directory, "out"), Environment: append(planner.GateEnvironmentList(), planner.TreeBuildEnvironment()...),
		Jobs: *jobs, Compile: *compile, Watched: watched, Phases: &clock.phases}
	for _, path := range []string{build.Cache, build.Out, filepath.Join(directory, "logs")} {
		if err = os.MkdirAll(path, 0o755); err != nil {
			return fail(err)
		}
	}
	clock.lap(&clock.phases.Readying)
	// The tree's npm packages, installed here once per lockfile and shipped in its source, so no runner runs npm: first,
	// so an install that fails fails the build before its products and binaries, not after (Workshop, Oct 10).
	installs, err := builder.InstallNodePackages(*tree, *nodeCache)
	if err != nil {
		return fail(fmt.Errorf("the tree's npm packages: %w", err))
	}
	clock.lap(&clock.phases.NpmInstall)
	packages, err := testPackages(*tree, keys)
	if err != nil {
		return fail(err)
	}
	productTests, err := planner.ListProductTests(*tree, nil)
	if err != nil {
		return fail(err)
	}
	clock.lap(&clock.phases.Listing)
	phase("warming", func(tree *livestatus.Tree) { tree.Packages, tree.ProductTests = len(packages), len(productTests) })
	if err = build.Warm(packages); err != nil {
		// A package that doesn't compile is named again by its own binary below; the rest are warm.
		fmt.Fprintf(stderr, "loom: warming the tree: %v\n", err)
	}
	clock.lap(&clock.phases.Warm)
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
	phase("products", nil)
	// The products the store held count as they are taken, once a second.
	counted := make(chan struct{})
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-counted:
				return
			case <-ticker.C:
				hit := len(held.Held())
				phase("products", func(tree *livestatus.Tree) { tree.ProductsHit = hit })
			}
		}
	}()
	products, productFailures := build.Products(productTests, filepath.Join(directory, "logs"))
	close(counted)
	held.Close()
	clock.phases.ProductsFetched, clock.phases.ProductsFetchedSeconds, clock.phases.ProductsBuilt, clock.phases.ProductsBuiltSeconds = builder.ProductCensus(filepath.Join(directory, "logs"))
	clock.lap(&clock.phases.Products)
	distinct := map[string]bool{}
	for _, keys := range products {
		for _, key := range keys {
			distinct[key] = true
		}
	}
	phase("binaries", func(tree *livestatus.Tree) { tree.Products, tree.ProductsHit = len(distinct), len(held.Held()) })
	for _, note := range held.Notes() {
		fmt.Fprintln(stderr, "build-tree:", note)
	}
	if err = held.Err(); err != nil {
		return fail(fmt.Errorf("the store held products it couldn't give whole: %w", err))
	}
	build.Held = ""
	built := build.Binaries(packages)
	clock.lap(&clock.phases.Binaries)
	phase("source and modules", nil)
	source, err := builder.SourceChunks(*tree, installs)
	if err != nil {
		return fail(err)
	}
	node, err := builder.NodeChunks(source, installs)
	if err != nil {
		return fail(err)
	}
	clock.lap(&clock.phases.SourceChunks)
	// The tree's modules, after its source is chunked (go may write go.sum), from Workshop's own module cache first.
	proxy, err := exec.Command("go", "env", "GOMODCACHE", "GOPROXY").Output()
	if err != nil {
		return fail(err)
	}
	moduleCache, upstream, _ := strings.Cut(strings.TrimSpace(string(proxy)), "\n")
	modules, err := builder.ModuleCacheArchive(*tree, []string{"GOPROXY=file://" + filepath.Join(strings.TrimSpace(moduleCache), "cache", "download") + "," + strings.TrimSpace(upstream)})
	if err != nil {
		return fail(fmt.Errorf("the tree's modules: %w", err))
	}
	// The platform the binaries are built for is in the key: a runner on another can't run them.
	treeIndex := builder.TreeIndex{Tree: identity.Tree, Future: *future, Go: identity.Go, Goos: identity.Goos, Goarch: identity.Goarch,
		Node: node, Packages: map[string]builder.TreePackage{}}
	for _, result := range built {
		result.Products = products[result.Package]
		if result.Products == nil {
			result.Products = []string{}
		}
		if failure, broke := productFailures[result.Package]; broke && result.Error == "" {
			// A product test that failed on Workshop is Workshop's to look at until its failure is classed too.
			result.Error, result.Failure = "product tests: "+failure, builder.WorkshopFailure
		}
		treeIndex.Packages[result.Package] = result
	}
	clock.lap(&clock.phases.ModuleCache)
	phase("uploading", nil)
	treeIndex.Seconds = time.Since(started).Seconds()
	// The module cache goes up before the index that names it, as every other blob does.
	modulesStarted := time.Now()
	if treeIndex.Modules, err = store.PutBlob(modules); err != nil {
		return fail(fmt.Errorf("the tree's modules: %w", err))
	}
	clock.phases.UploadModules = time.Since(modulesStarted).Seconds()
	treeKey, indexWritten, err := builder.PublishTree(store, &treeIndex, build.Out, build.Cache, &source, held.Held())
	if err != nil {
		return fail(err)
	}
	clock.lap(&clock.phases.Upload)
	// A package fails here when it didn't build, or when a product it reads conflicts with the store's ref.
	failed := 0
	for _, result := range built {
		if treeIndex.Packages[result.Package].Error != "" {
			failed++
		}
	}
	// The tree's directory goes before the summary, so its removal is a phase like the rest (Workshop, Oct 10: 46 s of
	// a 599 s build came after the summary, and nothing named them).
	exit := finishTree(stderr, *cache, directory, treeKey, failed, indexWritten, treeLock)
	clock.lap(&clock.phases.Removal)
	clock.finish()
	encoder := json.NewEncoder(stdout)
	for _, result := range built {
		encoder.Encode(treeIndex.Packages[result.Package])
	}
	encoder.Encode(map[string]any{
		"tree": identity.Tree, "future": *future, "treeKey": treeKey, "index": "trees/" + treeKey + ".json", "indexWritten": indexWritten,
		"packages": len(packages), "failed": failed, "productTests": len(productTests), "products": len(treeIndex.Products), "productsFetched": len(held.Held()),
		"warmSeconds": clock.phases.Warm, "productSeconds": clock.phases.Products, "binarySeconds": clock.phases.Binaries, "uploadSeconds": clock.phases.Upload,
		"seconds": clock.phases.Total, "phases": clock.phases, "sourceChunks": len(source.Chunks), "sourceBytes": source.Bytes(),
		"sourceChunksSent": source.Sent, "sourceBytesSent": source.SentBytes, "node": treeIndex.Node,
		"storeReads": requests.Reads.Load(), "storeWrites": requests.Writes.Load(),
	})
	writePhaseTable(stderr, clock.phases)
	phase("built", func(tree *livestatus.Tree) { tree.Failed = failed })
	return exit
}

// testPackages are the tree's test packages: the resident's, when its keys came with the build, else go's.
func testPackages(tree string, keys *resident.Keys) ([]planner.ProductTest, error) {
	if keys != nil {
		return keys.Packages, nil
	}
	return builder.TestPackages(tree)
}

// checkTreeKey refuses to build a tree here unless this machine is the runners' platform (host, GOOS/GOARCH), whose
// binaries and product tests it runs, its go is the Go release the plan's units are keyed on (wantGo; empty: no plan
// names one), and it keys as the plan's tree key want does (empty: no plan names one), each named.
func checkTreeKey(identity planner.TreeIdentity, want, wantGo, host string) error {
	if target := planner.RunnersGoos + "/" + planner.RunnersGoarch; host != target {
		return fmt.Errorf("build-tree builds for the runners' %s on that platform, and this is %s", target, host)
	}
	if wantGo != "" && identity.Go != wantGo {
		return fmt.Errorf("the plan's units are keyed on %s, and this go is %s: the tree is built only by the release its units name", wantGo, identity.Go)
	}
	if want == "" || identity.Key() == want {
		return nil
	}
	return fmt.Errorf("the plan carries tree key %s, and this tree keys %s (tree %s, %s, %s/%s): the planner and the builder read the tree apart",
		want, identity.Key(), identity.Tree, identity.Go, identity.Goos, identity.Goarch)
}

// buildTreeWatches are the filesystems a build writes, each with its floor: the cache base and Go's build cache keep
// floor, the temporary directory, which may be memory, a smaller one of its own, and GOCACHE=off has no cache to
// watch, so it stops nothing.
func buildTreeWatches(cache, goCache, temporary string, floor, temporaryFloor uint64) map[string]builder.Watch {
	watched := map[string]builder.Watch{"the cache base": {Path: cache, Floor: floor}, "the temporary directory": {Path: temporary, Floor: temporaryFloor}}
	if goCache != "off" {
		watched["Go's build cache"] = builder.Watch{Path: goCache, Floor: floor}
	}
	return watched
}

// finishTree removes the tree's working directory once its index is up (kept otherwise, for a retry) and exits. A
// removal that fails is warned about and left for the next TreeCache to sweep, and never fails the build: the index
// is up, and that is what runners read.
func finishTree(stderr io.Writer, cache, directory, treeKey string, failed int, indexWritten bool, lock *builder.TreeLock) int {
	if err := builder.TreeDone(cache, directory, indexWritten, lock); err != nil {
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
