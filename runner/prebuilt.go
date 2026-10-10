package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner/test2json"
)

// A prebuilt test job (job.Tree set) runs what Workshop built for its tree and builds nothing (Kirk's law, Oct 10,
// #pcn6prz). The job names its tree's build by key; trees/<key>.json in the action store names, by sha256, each
// package's test binary, the chunks of the tree's source and the products each package's tests read. The runner
// fetches those blobs through its blob cache (blobs.go), every hash checked, only the chunks it doesn't already hold,
// keeps the tree's source assembled for every unit of the tree (sources.go, assemble.go), unpacks the products and
// binaries into the unit's own directory, and runs each package's
// binary in its directory of that source as go test -json would: the
// binary's output, through the same test2json conversion go test applies (runner/test2json, vendored), is the event
// stream the Judge reads. Whatever the store can't give whole (an index, a binary, the source, a product: never built,
// past the 7-day lifecycle, or poisoned) is named and the unit is broken, Loom's to place again, never red.
//
// The runner never builds. The tests find a stand-in go first on PATH (standInGo): a read-only query (go version, go
// env, go list without a flag that builds), which adamic's buildcache asks to key a product, goes to the runner's own
// go under GOTOOLCHAIN set to the tree's Go release, checked before the tests run; every build is refused and recorded,
// and a unit that then fails is Loom's, never the change's. A runner with no go answers no query, and a unit whose
// tests asked one is unfit, a placement fact.
//
// Everything before the tests is held to the unit's time, so a store that stalls breaks a unit, never wedges it.

// blobFetchJobs is how many blobs a unit fetches at once, and chunkFetchJobs how many when it fetches its tree's
// source: hundreds of chunks, most of them small, each a round trip.
const (
	blobFetchJobs  = 4
	chunkFetchJobs = 16
)

// A prebuiltPackage is one of the job's packages and what its tree's build holds for it. One whose test binary
// didn't compile on Workshop for the change's reasons (built.Error, builder.ChangeFailure) is the change's red.
type prebuiltPackage struct {
	test  protocol.TestPackage
	built builder.TreePackage
}

// A neededBlob is one blob a unit reads, and what it is, for the record and any error. A chunk of the tree's source
// is only had in the blob cache, and said in a sum, not a line of its own.
type neededBlob struct {
	sum   string
	what  string
	chunk bool
}

// runPrebuilt runs a test job whose tree Workshop built. Failed: a package's tests failed or the unit ran out of time.
// Broken: the instance was unfit (under the free floor with its blob cache empty, or without adamic's toolchain), the
// store couldn't give what the unit reads whole, or a test that failed had tried to run go.
func (run *unitRun) runPrebuilt(runContext context.Context, job *protocol.TestJob, root string, started, deadline time.Time) string {
	if run.options.Strict {
		// A strict runner runs one unit at a time on its root, so what earlier units left there is no one's.
		if err := trimRoot(runContext, root, run.options.Exclusive, io.Discard); err != nil {
			run.say(fmt.Sprintf("trimming %s: %v", root, err))
		}
	}
	prepareContext, cancelPrepare := context.WithDeadline(runContext, deadline)
	defer cancelPrepare()
	if err := readyRoot(run.options, root); err != nil {
		run.fail(protocol.PhaseStart, fmt.Errorf("refused as unfit: %w", err))
		return protocol.StatusBroken
	}
	cache, sources := newBlobCache(run.options, root), newSourceCache(root)
	index, err := run.treeIndex(prepareContext, job.Tree)
	if err != nil {
		run.fail(protocol.PhaseFetch, fmt.Errorf("%w: Loom's, never the change's", err))
		return protocol.StatusBroken
	}
	// Binaries built for another platform can't run here: the placement's mistake, refused before anything is fetched.
	if index.Goos != runtime.GOOS || index.Goarch != runtime.GOARCH {
		run.fail(protocol.PhaseStart, fmt.Errorf("refused as unfit: the tree's binaries are built for %s/%s, and this runner is %s/%s: Loom's, never the change's",
			index.Goos, index.Goarch, runtime.GOOS, runtime.GOARCH))
		return protocol.StatusBroken
	}
	packages, needed, err := prebuiltPackages(job, index)
	if err != nil {
		run.fail(protocol.PhaseFetch, fmt.Errorf("%w: Loom's, never the change's", err))
		return protocol.StatusBroken
	}
	sourceSum := builder.SourceSum(index.Source)
	held, err := sources.hold(prepareContext, sourceSum)
	if err != nil {
		run.fail(protocol.PhaseStart, fmt.Errorf("holding the tree's source: %w (the instance's, never the change's)", err))
		return protocol.StatusBroken
	}
	defer held.release()
	// The kept tree the source is made from, when one is near: only the chunks it lacks are fetched.
	var base *sourceBase
	if held.ready {
		run.say("the tree's source, " + sourceSum + ", is already assembled here")
	} else {
		var passed []string
		base, passed = sources.nearest(index.Source)
		defer base.release()
		for _, note := range passed {
			run.say(note)
		}
		kept := map[builder.SourceChunk]bool{}
		if base != nil {
			for _, chunk := range base.state.Chunks {
				kept[chunk] = true
			}
		}
		fetching, fetchingBytes := 0, int64(0)
		for _, chunk := range index.Source {
			if !kept[chunk] {
				needed = append(needed, neededBlob{sum: chunk.Blob, what: "the tree's source chunk " + strconv.Quote(chunk.First) + " to " + strconv.Quote(chunk.Last), chunk: true})
				fetching++
				fetchingBytes += chunk.Bytes
			}
		}
		from := "no kept tree"
		if base != nil {
			from = "kept tree " + base.sum
		}
		run.say(fmt.Sprintf("the tree's source, %s: %d chunks, %d bytes; from %s, %d chunks to have, %d bytes", sourceSum, len(index.Source),
			sourceBytes(index.Source), from, fetching, fetchingBytes))
	}
	// The tree's module cache, unpacked once like its source: the only place the tests' go queries read a module.
	var modules *heldSource
	if index.Modules != "" {
		if modules, err = sources.hold(prepareContext, index.Modules); err != nil {
			run.fail(protocol.PhaseStart, fmt.Errorf("holding the tree's module cache: %w (the instance's, never the change's)", err))
			return protocol.StatusBroken
		}
		defer modules.release()
		if !modules.ready {
			needed = append(needed, neededBlob{sum: index.Modules, what: "the tree's module cache"})
		}
	}
	fetchStarted := time.Now()
	files, err := run.fetchBlobs(prepareContext, cache, needed)
	defer func() {
		for _, file := range files {
			file.Close()
		}
	}()
	if err != nil {
		run.fail(protocol.PhaseFetch, fmt.Errorf("%w: Loom's, never the change's", err))
		return protocol.StatusBroken
	}
	fetchSeconds := time.Since(fetchStarted).Seconds()
	keep := map[string]bool{}
	for _, blob := range needed {
		keep[blob.sum] = true
	}
	// The unit's own blobs stay; the cache is held to its bound by the rest.
	if err := cache.trim(keep); err != nil {
		run.say(fmt.Sprintf("trimming the blob cache: %v", err))
	}
	unpackStarted := time.Now()
	if !held.ready {
		open := func(openContext context.Context, sum string) (*os.File, error) {
			file, _, err := cache.open(openContext, sum)
			return file, err
		}
		done, err := sources.assemble(prepareContext, held, index.Source, base, open)
		if err != nil {
			run.fail(protocol.PhaseStart, fmt.Errorf("assembling the tree's source: %w (Loom's, never the change's)", err))
			return protocol.StatusBroken
		}
		if done.base != "" {
			run.say(fmt.Sprintf("assembled the tree's source in %.1f s from kept tree %s: %d chunks kept, %d entries removed, %d chunks unpacked",
				time.Since(unpackStarted).Seconds(), done.base, done.kept, done.removed, done.unpacked))
		} else {
			run.say(fmt.Sprintf("assembled the tree's source in %.1f s: %d chunks unpacked", time.Since(unpackStarted).Seconds(), done.unpacked))
		}
		if done.passed != "" {
			run.say("the kept tree wasn't made into this one, so it was assembled from nothing: " + done.passed)
		}
		if done.stateErr != nil {
			run.say(fmt.Sprintf("the tree's state wasn't written, so it is never made into another: %v", done.stateErr))
		}
	}
	if modules != nil && !modules.ready {
		if err := sources.unpack(prepareContext, modules, files[index.Modules]); err != nil {
			run.fail(protocol.PhaseStart, fmt.Errorf("unpacking the tree's module cache: %w (Loom's, never the change's)", err))
			return protocol.StatusBroken
		}
	}
	sources.trim(keepSources)
	source := held.directory
	products := filepath.Join(run.directory, "adamic-build")
	binaries := filepath.Join(run.directory, "binaries")
	if err := unpackPrebuilt(prepareContext, index, packages, files, products, binaries); err != nil {
		run.fail(protocol.PhaseStart, fmt.Errorf("unpacking the tree's build: %w (the instance's, never the change's)", err))
		return protocol.StatusBroken
	}
	for _, file := range files {
		file.Close()
	}
	files = nil
	run.say(fmt.Sprintf("ready in %.1f s: fetched in %.1f s, unpacked in %.1f s", time.Since(started).Seconds(), fetchSeconds, time.Since(unpackStarted).Seconds()))

	script := filepath.Join(run.directory, "prepare.sh")
	environmentFile := filepath.Join(run.directory, "environment")
	if err := os.WriteFile(script, prepareScript, 0o700); err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	prepared, _, _, err := run.stream(runContext, []string{"bash", script, "environment", source, job.GateInputs, environmentFile, root}, run.environment(), run.workspace, time.Until(deadline))
	switch {
	case err != nil:
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	case prepared.interrupted:
		run.fail(protocol.PhaseRun, fmt.Errorf("the runner was stopped while readying the environment"))
		return protocol.StatusBroken
	case prepared.timedOut || prepared.code == nil || *prepared.code != 0:
		run.fail(protocol.PhaseStart, fmt.Errorf("readying the environment failed (%s): the instance's, never the change's", describeOutcome(prepared)))
		return protocol.StatusBroken
	}
	environment, err := run.testEnvironment(environmentFile, job)
	if err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	// testEnvironment names the unit's own buildcache directory, where the products were unpacked.
	if environment["ADAMIC_BUILD_CACHE_DIR"] != products {
		run.fail(protocol.PhaseStart, fmt.Errorf("the products are in %s and the tests would read %s", products, environment["ADAMIC_BUILD_CACHE_DIR"]))
		return protocol.StatusBroken
	}
	for _, testPackage := range job.Packages {
		if wasiPattern.MatchString(testPackage.Run) {
			if err := wasiReady(environment); err != nil {
				run.fail(protocol.PhaseStart, err)
				return protocol.StatusBroken
			}
			break
		}
	}
	standIn, err := run.standInGo(prepareContext, environment, index, sources, source)
	if err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	out := filepath.Join(run.workspace, "loom-out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	status := run.runPackages(runContext, job, out, started, deadline, func(testContext context.Context, index int, part string) packageResult {
		if packages[index].built.Error != "" {
			return buildFailedPackage(packages[index], part)
		}
		return run.runBinary(testContext, packages[index], filepath.Join(binaries, fmt.Sprintf("%d.test", index)),
			packageEnvironment(environment, packages[index].test), source, part)
	})
	return run.settleGo(standIn, status)
}

// treeIndex reads trees/<treeKey>.json from the store, as builder.ParseTree checks it, and holds it to its key: the
// index's own tree, Go release, platform and the gate's environment must hash to the key the job named.
func (run *unitRun) treeIndex(runContext context.Context, treeKey string) (builder.TreeIndex, error) {
	started := time.Now()
	name := "trees/" + treeKey + ".json"
	request, err := http.NewRequestWithContext(runContext, http.MethodGet, strings.TrimSuffix(run.options.Store, "/")+"/"+name, nil)
	if err != nil {
		return builder.TreeIndex{}, err
	}
	response, err := run.options.Client.Do(request)
	if err != nil {
		return builder.TreeIndex{}, fmt.Errorf("the tree's index %s: %w", name, err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return builder.TreeIndex{}, fmt.Errorf("the tree's index %s isn't in the store (never built, or past its 7 days)", name)
	case response.StatusCode != http.StatusOK:
		return builder.TreeIndex{}, fmt.Errorf("the tree's index %s: the store answered %s", name, response.Status)
	}
	content, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return builder.TreeIndex{}, fmt.Errorf("the tree's index %s: %w", name, err)
	}
	run.say(fmt.Sprintf("fetched %s, %d bytes in %.2f s", name, len(content), time.Since(started).Seconds()))
	index, err := builder.ParseTree(treeKey, content)
	if err != nil {
		return builder.TreeIndex{}, err
	}
	if key := planner.TreeKey(index.Tree, index.Go, index.Goos, index.Goarch); key != treeKey {
		return builder.TreeIndex{}, fmt.Errorf("the tree's index %s describes tree %s on %s for %s/%s, whose key is %s: the store is poisoned", name, index.Tree, index.Go, index.Goos, index.Goarch, key)
	}
	return index, nil
}

// prebuiltPackages finds each of the job's packages in the tree's build and lists every blob its packages read, once
// each: each package's binary, and each product its tests read.
func prebuiltPackages(job *protocol.TestJob, index builder.TreeIndex) ([]prebuiltPackage, []neededBlob, error) {
	packages := make([]prebuiltPackage, len(job.Packages))
	needed := []neededBlob{}
	seen := map[string]bool{}
	add := func(sum, what string) {
		if !seen[sum] {
			seen[sum] = true
			needed = append(needed, neededBlob{sum: sum, what: what})
		}
	}
	for position, testPackage := range job.Packages {
		built, found := index.Packages[testPackage.Package]
		switch {
		case !found:
			return nil, nil, fmt.Errorf("the tree's build has no package %s", testPackage.Package)
		case built.Error != "" && built.Failure == builder.ChangeFailure:
			// The change's red: no binary to fetch, only its diagnostics to say.
			packages[position] = prebuiltPackage{test: testPackage, built: built}
			continue
		case built.Error != "":
			return nil, nil, fmt.Errorf("package %s didn't build on Workshop, for Workshop's reasons: %s", testPackage.Package, strings.TrimSpace(built.Error))
		}
		packages[position] = prebuiltPackage{test: testPackage, built: built}
		add(built.Binary, testPackage.Package+"'s test binary")
		for _, product := range built.Products {
			add(index.Products[product], "product "+product+", which "+testPackage.Package+"'s tests read")
		}
	}
	return packages, needed, nil
}

// fetchBlobs has every needed blob open, blobFetchJobs at a time (chunkFetchJobs with any of the source's chunks
// among them), each fetch's bytes and seconds said on the unit's record, and every chunk of the source in the blob
// cache, closed, their fetches said in a sum. A blob that can't be had is named with what it is.
func (run *unitRun) fetchBlobs(fetchContext context.Context, cache blobCache, needed []neededBlob) (map[string]*os.File, error) {
	files := map[string]*os.File{}
	var mutex sync.Mutex
	var fetched, cached, chunksFetched, chunksCached int64
	var cachedCount, chunksFetchedCount, chunksCachedCount, chunksCorrupt int
	errs := make([]error, len(needed))
	next := make(chan int)
	var group sync.WaitGroup
	jobs := blobFetchJobs
	for _, blob := range needed {
		if blob.chunk {
			jobs = chunkFetchJobs
		}
	}
	for range jobs {
		group.Add(1)
		go func() {
			defer group.Done()
			for position := range next {
				blob := needed[position]
				file, fetch, err := cache.open(fetchContext, blob.sum)
				if errors.Is(err, builder.ErrNotStored) {
					err = fmt.Errorf("%s, blob %s, isn't in the store (never uploaded, or past its 7 days)", blob.what, blob.sum)
				} else if err != nil {
					err = fmt.Errorf("%s: %w", blob.what, err)
				}
				if err != nil {
					errs[position] = err
					continue
				}
				if blob.chunk {
					// Assembly opens it again through the cache, hashed again, fetched again if it went meanwhile.
					file.Close()
					mutex.Lock()
					if fetch.cached {
						chunksCached += fetch.bytes
						chunksCachedCount++
					} else {
						chunksFetched += fetch.bytes
						chunksFetchedCount++
					}
					if fetch.corrupt {
						chunksCorrupt++
					}
					mutex.Unlock()
					continue
				}
				from := "the store"
				if fetch.cached {
					from = "the cache"
				}
				if fetch.corrupt {
					from += ", in place of a cached copy that didn't hash to its name"
				}
				run.say(fmt.Sprintf("fetched %s, blob %s, %d bytes in %.2f s from %s", blob.what, blob.sum, fetch.bytes, fetch.seconds, from))
				mutex.Lock()
				files[blob.sum] = file
				if fetch.cached {
					cached += fetch.bytes
					cachedCount++
				} else {
					fetched += fetch.bytes
				}
				mutex.Unlock()
			}
		}()
	}
	for position := range needed {
		next <- position
	}
	close(next)
	group.Wait()
	if chunks := chunksFetchedCount + chunksCachedCount; chunks > 0 {
		corrupt := ""
		if chunksCorrupt > 0 {
			corrupt = fmt.Sprintf(", %d of them in place of a cached copy that didn't hash to its name", chunksCorrupt)
		}
		run.say(fmt.Sprintf("the tree's source chunks: %d bytes in %d chunks from the store%s, %d bytes in %d chunks from the cache",
			chunksFetched, chunksFetchedCount, corrupt, chunksCached, chunksCachedCount))
	}
	run.say(fmt.Sprintf("%d blobs: %d bytes from the store, %d bytes in %d blobs from the cache", len(needed), fetched+chunksFetched, cached+chunksCached, cachedCount+chunksCachedCount))
	return files, errors.Join(errs...)
}

// unpackPrebuilt unpacks each package's products into products (the unit's ADAMIC_BUILD_CACHE_DIR), each holding
// only its own entries, and each package's binary, gunzipped, into binaries as <index>.test. Both are the unit's own
// directory, so a runner killed partway leaves nothing a later unit reads. unpackContext bounds it.
func unpackPrebuilt(unpackContext context.Context, index builder.TreeIndex, packages []prebuiltPackage, files map[string]*os.File, products, binaries string) error {
	unpacked := map[string]bool{}
	for _, prebuilt := range packages {
		if prebuilt.built.Error != "" {
			continue
		}
		for _, product := range prebuilt.built.Products {
			if unpacked[product] {
				continue
			}
			unpacked[product] = true
			file := files[index.Products[product]]
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := builder.Unpack(contextReader{unpackContext, file}, products, builder.ProductEntries(product)); err != nil {
				return fmt.Errorf("product %s: %w", product, err)
			}
		}
	}
	if err := os.MkdirAll(products, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(binaries, 0o755); err != nil {
		return err
	}
	for position, prebuilt := range packages {
		if prebuilt.built.Error != "" {
			continue
		}
		file := files[prebuilt.built.Binary]
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := gunzipTo(contextReader{unpackContext, file}, filepath.Join(binaries, fmt.Sprintf("%d.test", position))); err != nil {
			return fmt.Errorf("%s's test binary: %w", prebuilt.test.Package, err)
		}
	}
	return nil
}

// gunzipTo writes a gzipped blob's content to a new executable file at path.
func gunzipTo(blob io.Reader, path string) error {
	reader, err := gzip.NewReader(blob)
	if err != nil {
		return err
	}
	defer reader.Close()
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	_, err = io.Copy(output, reader)
	if closeErr := output.Close(); err == nil {
		err = closeErr
	}
	return err
}

// delegatedEnvironment is what every go the runner runs for its tests sees above theirs: no go env file, the tree's
// module cache as the only proxy, no checksum database, and a module cache per tree under the runner's root, so
// nothing reaches the network. The tests' own GOFLAGS and GOTOOLCHAIN pass through as they set them (adamic's
// GoInputs keys products on what go env says of them, as Workshop's go said it), when the stand-in allows them.
func delegatedEnvironment(proxy, moduleCache string) []string {
	return []string{"GOENV=off", "GOPROXY=" + proxy, "GOSUMDB=off", "GOMODCACHE=" + moduleCache}
}

// runnersGo is what the runner's own go commands (the release check, filling the module cache, reading go.work) add
// above delegatedEnvironment: the runner's go as itself, and no test's flags.
var runnersGo = []string{"GOTOOLCHAIN=local", "GOFLAGS="}

// workspaceCopy writes the tree's go.work, its paths made absolute, and its go.work.sum when it has one, into
// directory, for the tests' go queries, and returns the copy's path: in workspace mode go writes the checksums it
// learns into go.work.sum beside go.work, and the tree's source is every unit's, so no go query may write there. ""
// when the tree's source has no go.work at its top.
func workspaceCopy(copyContext context.Context, real string, environment []string, source, directory string) (string, error) {
	original := filepath.Join(source, "go.work")
	if _, err := os.Stat(original); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	command := exec.CommandContext(copyContext, real, "work", "edit", "-json", original)
	command.Env, command.Dir = environment, source
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("go work edit -json: %w", err)
	}
	var work struct {
		Go        string
		Toolchain string
		Godebug   []struct{ Key, Value string }
		Use       []struct{ DiskPath string }
		Replace   []struct {
			Old, New struct{ Path, Version string }
		}
	}
	if err = json.Unmarshal(output, &work); err != nil {
		return "", err
	}
	local := func(path string) string {
		if filepath.IsAbs(path) {
			return path
		}
		return filepath.Join(source, filepath.FromSlash(path))
	}
	var text strings.Builder
	fmt.Fprintf(&text, "go %s\n", work.Go)
	if work.Toolchain != "" {
		fmt.Fprintf(&text, "toolchain %s\n", work.Toolchain)
	}
	for _, setting := range work.Godebug {
		fmt.Fprintf(&text, "godebug %s=%s\n", setting.Key, setting.Value)
	}
	for _, use := range work.Use {
		fmt.Fprintf(&text, "use %s\n", strconv.Quote(local(use.DiskPath)))
	}
	for _, replace := range work.Replace {
		old := replace.Old.Path
		if replace.Old.Version != "" {
			old += " " + replace.Old.Version
		}
		target := replace.New.Path + " " + replace.New.Version
		if replace.New.Version == "" {
			// A version-less replacement is a directory.
			target = strconv.Quote(local(replace.New.Path))
		}
		fmt.Fprintf(&text, "replace %s => %s\n", old, target)
	}
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return "", err
	}
	copied := filepath.Join(directory, "go.work")
	if err = os.WriteFile(copied, []byte(text.String()), 0o644); err != nil {
		return "", err
	}
	if sums, err := os.ReadFile(filepath.Join(source, "go.work.sum")); err == nil {
		if err = os.WriteFile(filepath.Join(directory, "go.work.sum"), sums, 0o644); err != nil {
			return "", err
		}
	}
	return copied, nil
}

// A goStandIn is the go a prebuilt unit's tests find first on PATH, and the files it writes what they asked of it in:
// each build it refused, each read-only query the runner's go answered (with its exit), each one no go here could.
type goStandIn struct {
	refused, answered, unanswered string
}

// standInScript is the stand-in go. A read-only query (version, env, list, each with only the flags on its allow list:
// none that builds, none that asks a proxy) goes to the runner's go under delegatedEnvironment; anything else, a build
// among it, is refused.
const standInScript = `#!/bin/sh
# loom-runner's stand-in go (prebuilt.go): the runner never builds.
allowed=no
case "$1" in
version)
	allowed=yes
	for argument in "$@"; do case "$argument" in version | -m | -v | -json) ;; -*) allowed=no ;; esac; done ;;
env)
	allowed=yes
	for argument in "$@"; do case "$argument" in -json | -changed) ;; -*) allowed=no ;; esac; done ;;
list)
	allowed=yes
	for argument in "$@"; do case "$argument" in -deps | -json | -json=* | -e | -f | -f=* | -find | -m | -mod=readonly | -mod=vendor | -tags | -tags=*) ;; -*) allowed=no ;; esac; done ;;
esac
# The test's own GOFLAGS and GOTOOLCHAIN pass through, each flag one that neither compiles, nor runs or reads anything
# of the test's choosing, and the toolchain the tree's or the runner's own.
set -f
for flag in $GOFLAGS; do
	case "$flag" in
	-buildvcs | -buildvcs=* | -trimpath | -trimpath=* | -p=* | -mod=readonly | -mod=mod | -tags=* | -ldflags=* | -gcflags=* | -asmflags=* | -race | -race=* | -cover | -cover=* | -covermode=* | -coverpkg=* | -pgo=off | -pgo=auto) ;;
	*) allowed=no ;;
	esac
done
set +f
case "${GOTOOLCHAIN:-auto}" in auto | local | RELEASE) ;; *) allowed=no ;; esac
if [ "$allowed" = no ]; then
	printf 'go %s (GOFLAGS=%s GOTOOLCHAIN=%s)\n' "$*" "$GOFLAGS" "$GOTOOLCHAIN" >> REFUSED
	echo "loom-runner: go $*: this runner runs Workshop's prebuilt tests and never builds; a test that needs a build is Loom's to fix, never the change's" >&2
	exit 1
fi
real=REAL
if [ -z "$real" ]; then
	printf 'go %s\n' "$*" >> UNANSWERED
	echo "loom-runner: go $*: this runner has no Go to answer it" >&2
	exit 1
fi
unset GONOSUMDB GONOSUMCHECK GOPRIVATE GONOPROXY GOINSECURE
ENVIRONMENT
"$real" "$@"
status=$?
printf '%s go %s\n' "$status" "$*" >> ANSWERED
exit "$status"
`

// standInGo puts the stand-in go first on the tests' PATH. The runner's own go, found on the tests' PATH behind it or
// else on the runner's, answers read-only queries under delegatedEnvironment, reading modules only from the tree's
// module cache (index.Modules, unpacked in sources): it must report exactly the tree's Go release under
// GOTOOLCHAIN=local, or the unit is unfit, both named, and every module the tree needs is put in the tree's own
// GOMODCACHE before the tests run, so none of them sees go fetch one, and a module the cache lacks breaks the unit,
// named. With no go, the record says so.
func (run *unitRun) standInGo(checkContext context.Context, environment map[string]string, index builder.TreeIndex, sources sourceCache, directory string) (goStandIn, error) {
	release := strings.Fields(index.Go)
	if len(release) == 0 {
		return goStandIn{}, fmt.Errorf("the tree's index names no Go release: the store is poisoned")
	}
	proxy, moduleCache := "off", filepath.Join(run.directory, "modules")
	if index.Modules != "" {
		proxy, moduleCache = "file://"+filepath.Join(sources.directory, index.Modules), sources.moduleCache(index.Modules)
	}
	delegated := delegatedEnvironment(proxy, moduleCache)
	real, err := lookPath("go", environment["PATH"])
	if err != nil {
		real, err = lookPath("go", os.Getenv("PATH"))
	}
	if err != nil {
		real = ""
		run.say("no go here: a test's read-only go query goes unanswered, and a unit that asks one is unfit; no test builds here")
	} else {
		goCommand := func(arguments ...string) ([]byte, error) {
			command := exec.CommandContext(checkContext, real, arguments...)
			command.Env, command.Dir = append(append(packageEnvironment(environment, protocol.TestPackage{}), delegated...), runnersGo...), directory
			return command.CombinedOutput()
		}
		output, err := goCommand("env", "GOVERSION")
		if says := strings.TrimSpace(string(output)); err != nil || says != release[0] {
			return goStandIn{}, fmt.Errorf("refused as unfit: the runner's go at %s is %q under GOTOOLCHAIN=local (%v), and the tree was built with %s: Loom's, never the change's",
				real, says, err, release[0])
		}
		work, err := workspaceCopy(checkContext, real, append(append(packageEnvironment(environment, protocol.TestPackage{}), delegated...), runnersGo...), directory, filepath.Join(run.directory, "workspace-go"))
		if err != nil {
			return goStandIn{}, fmt.Errorf("copying the tree's go.work: %w (Loom's, never the change's)", err)
		}
		if work != "" {
			delegated = append(delegated, "GOWORK="+work)
		}
		if index.Modules != "" {
			started := time.Now()
			if output, err := goCommand("mod", "download", "all"); err != nil {
				lines := strings.Split(strings.TrimSpace(string(output)), "\n")
				return goStandIn{}, fmt.Errorf("a module the tree needs isn't in its module cache, blob %s (%v): %s: Loom's, never the change's", index.Modules, err, lines[len(lines)-1])
			}
			run.say(fmt.Sprintf("the tree's modules are in %s in %.1f s", moduleCache, time.Since(started).Seconds()))
		}
		run.say("go at " + real + " answers the tests' read-only go queries as " + release[0] + ", from the tree's module cache; no test builds or downloads here")
	}
	bin := filepath.Join(run.directory, "stand-in")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return goStandIn{}, err
	}
	standIn := goStandIn{refused: filepath.Join(run.directory, "go-refused"), answered: filepath.Join(run.directory, "go-answered"),
		unanswered: filepath.Join(run.directory, "go-unanswered")}
	exports := ""
	for _, variable := range delegated {
		name, value, _ := strings.Cut(variable, "=")
		if name == "GOWORK" {
			// Only a query in the tree's source: one in a module of the test's own elsewhere would find it outside
			// the workspace and fail. A test that chose its own workspace (GOWORK=off, say) keeps it.
			tree := directory
			if resolved, err := filepath.EvalSymlinks(directory); err == nil {
				tree = resolved
			}
			exports += "if [ -z \"$GOWORK\" ]; then case \"$(pwd -P)\" in " + shellQuote(tree) + " | " + shellQuote(tree) + "/*) GOWORK=" +
				shellQuote(value) + "; export GOWORK ;; esac; fi\n"
			continue
		}
		exports += name + "=" + shellQuote(value) + "\nexport " + name + "\n"
	}
	script := strings.NewReplacer("REFUSED", shellQuote(standIn.refused), "UNANSWERED", shellQuote(standIn.unanswered),
		"ANSWERED", shellQuote(standIn.answered), "REAL", shellQuote(real), "RELEASE", shellQuote(release[0]), "ENVIRONMENT\n", exports).Replace(standInScript)
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o755); err != nil {
		return goStandIn{}, err
	}
	if environment["PATH"] == "" {
		// An empty entry would be the working directory.
		environment["PATH"] = bin
	} else {
		environment["PATH"] = bin + string(os.PathListSeparator) + environment["PATH"]
	}
	return standIn, nil
}

// fileLines is a file's lines, none when it is missing or empty.
func fileLines(path string) []string {
	content, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(content)) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(content)), "\n")
}

// settleGo says on the record what the tests asked of go, and decides what it means for the unit: a query no go here
// could answer makes it unfit; a build refused, or a query that failed here, makes a red Loom's, never the change's.
func (run *unitRun) settleGo(standIn goStandIn, status string) string {
	counts, order := map[string]int{}, []string{}
	failed := []string{}
	for _, line := range fileLines(standIn.answered) {
		code, query, _ := strings.Cut(line, " ")
		if code != "0" {
			failed = append(failed, query+" (exit "+code+")")
		}
		if counts[line] == 0 {
			order = append(order, line)
		}
		counts[line]++
	}
	for _, line := range order {
		code, query, _ := strings.Cut(line, " ")
		run.say(fmt.Sprintf("answered for the tests, %d times: %s (exit %s)", counts[line], query, code))
	}
	refused := fileLines(standIn.refused)
	for _, line := range refused {
		run.say("a test ran " + line + ", and this runner never builds or downloads")
	}
	if unanswered := fileLines(standIn.unanswered); len(unanswered) > 0 {
		run.fail(protocol.PhaseRun, fmt.Errorf("refused as unfit: the tests need Go for %q (%d queries), and this runner has none: a placement fact, never the change's",
			unanswered[0], len(unanswered)))
		return protocol.StatusBroken
	}
	if status != protocol.StatusFailed {
		return status
	}
	if len(refused) > 0 {
		run.fail(protocol.PhaseRun, fmt.Errorf("a test ran go for more than a read-only query (%d go commands, the first %q), and this runner never builds or downloads: Loom's, never the change's", len(refused), refused[0]))
		return protocol.StatusBroken
	}
	if len(failed) > 0 {
		run.fail(protocol.PhaseRun, fmt.Errorf("a read-only go query failed here (%s): Loom's, never the change's", failed[0]))
		return protocol.StatusBroken
	}
	return status
}

// buildFailedPackage writes what go test -json writes for a package whose test binary doesn't compile or vet, with
// Workshop's diagnostics as its build output, and fails the package: the change's red, as go test said it.
func buildFailedPackage(prebuilt prebuiltPackage, part string) packageResult {
	result := packageResult{log: ".output", code: 1}
	_, diagnostics, _ := strings.Cut(prebuilt.built.Error, "\n")
	if err := os.WriteFile(part+".output", []byte(diagnostics), 0o644); err != nil {
		result.err = err
		return result
	}
	jsonl, err := os.Create(part + ".jsonl")
	if err != nil {
		result.err = err
		return result
	}
	defer jsonl.Close()
	importPath := prebuilt.test.Package + " [" + prebuilt.test.Package + ".test]"
	encoder := json.NewEncoder(jsonl)
	encoder.SetEscapeHTML(false)
	type buildEvent struct {
		ImportPath string
		Action     string
		Output     string `json:",omitempty"`
	}
	for _, line := range strings.SplitAfter(diagnostics, "\n") {
		if line != "" {
			encoder.Encode(buildEvent{ImportPath: importPath, Action: "build-output", Output: line})
		}
	}
	encoder.Encode(buildEvent{ImportPath: importPath, Action: "build-fail"})
	converter := test2json.NewConverter(jsonl, prebuilt.test.Package, test2json.Timestamp)
	converter.SetFailedBuild(importPath)
	converter.Write([]byte("FAIL\t" + prebuilt.test.Package + " [build failed]\n"))
	converter.Exited(errors.New("build failed"))
	converter.Close()
	return result
}

// shellQuote is text as one single-quoted shell word.
func shellQuote(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// testBinaryArguments are the flags go test -json gives a package's test binary for the job's patterns, each one
// argument: -test.v=test2json frames its output for the converter, and -test.paniconexit0 and the timeout are go
// test's own.
func testBinaryArguments(binary string, testPackage protocol.TestPackage) []string {
	arguments := []string{binary, "-test.paniconexit0", "-test.timeout=3h0m0s", "-test.count=1", "-test.v=test2json", "-test.run=" + testPackage.Run}
	if testPackage.Skip != "" {
		arguments = append(arguments, "-test.skip="+testPackage.Skip)
	}
	return arguments
}

// noTestsToRun is what a test binary prints when its patterns match nothing; go test marks its ok line for it.
var noTestsToRun = []byte("\ntesting: warning: no tests to run\n")

// A tailWriter counts what passes through it and remembers the last byte, as go test does to end the binary's
// output with a line before its own.
type tailWriter struct {
	writer io.Writer
	count  int64
	last   byte
}

func (tail *tailWriter) Write(content []byte) (int, error) {
	if len(content) > 0 {
		tail.count += int64(len(content))
		tail.last = content[len(content)-1]
	}
	return tail.writer.Write(content)
}

// runBinary runs one package's prebuilt test binary in its directory of the tree's source, its output (stdout and
// stderr as one, as go test takes it) through test2json into <part>.jsonl and as text into <part>.output, and then
// writes what go test writes after a binary (its ok or FAIL line), so the converter ends the package as go test -json
// does. Past the context's deadline the process group gets SIGTERM, then SIGKILL after KillGrace.
func (run *unitRun) runBinary(testContext context.Context, prebuilt prebuiltPackage, binary string, environment []string, source string, part string) packageResult {
	result := packageResult{log: ".output"}
	directory := filepath.Join(source, filepath.FromSlash(prebuilt.built.Directory))
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		result.err = fmt.Errorf("its directory %q isn't in the tree's source", prebuilt.built.Directory)
		return result
	}
	jsonl, err := os.Create(part + ".jsonl")
	if err != nil {
		result.err = err
		return result
	}
	defer jsonl.Close()
	text, err := os.Create(part + ".output")
	if err != nil {
		result.err = err
		return result
	}
	defer text.Close()
	converter := test2json.NewConverter(jsonl, prebuilt.test.Package, test2json.Timestamp)
	output := &tailWriter{writer: io.MultiWriter(converter, text)}
	started := time.Now()
	state, err := run.groupCommand(testContext, testBinaryArguments(binary, prebuilt.test), append(environment, "PWD="+directory), directory, output, output)
	if err != nil {
		converter.Exited(err)
		converter.Close()
		result.err = err
		return result
	}
	elapsed := fmt.Sprintf("%.3fs", time.Since(started).Seconds())
	if output.count > 0 && output.last != '\n' {
		output.Write([]byte("\n"))
	}
	if state.Success() {
		norun := ""
		if content, err := os.ReadFile(part + ".output"); err == nil && (bytes.HasPrefix(content, noTestsToRun[1:]) || bytes.Contains(content, noTestsToRun)) {
			norun = " [no tests to run]"
		}
		fmt.Fprintf(output, "ok  \t%s\t%s%s\n", prebuilt.test.Package, elapsed, norun)
		converter.Exited(nil)
	} else {
		if status, ok := state.Sys().(syscall.WaitStatus); output.count == 0 || (ok && status.Signaled()) {
			fmt.Fprintf(output, "%s\n", state.String())
		}
		fmt.Fprintf(output, "\x16FAIL\t%s\t%s\n", prebuilt.test.Package, elapsed)
		converter.Exited(errors.New(state.String()))
	}
	converter.Close()
	result.testSeconds = state.UserTime().Seconds() + state.SystemTime().Seconds()
	result.userSeconds, result.systemSeconds = state.UserTime().Seconds(), state.SystemTime().Seconds()
	result.code = state.ExitCode()
	if result.code < 0 {
		result.code = 1 // killed by a signal: at the deadline, or by the runner's stop
	}
	return result
}
