package runner

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner/test2json"
)

// A prebuilt test job (job.Tree set) runs what Workshop built for its tree and builds nothing (Kirk's law, Oct 10,
// #pcn6prz). The job names its tree's build by key; trees/<key>.json in the action store names, by sha256, each
// package's test binary, the tree's source archive and the products each package's tests read. The runner fetches
// those blobs through its blob cache (blobs.go), every hash checked, keeps the tree's source unpacked for every unit
// of the tree (sources.go), unpacks the products and binaries into the unit's own directory, and runs each package's
// binary in its directory of that source as go test -json would: the
// binary's output, through the same test2json conversion go test applies (runner/test2json, vendored), is the event
// stream the Judge reads. Whatever the store can't give whole (an index, a binary, the source, a product: never built,
// past the 7-day lifecycle, or poisoned) is named and the unit is broken, Loom's to place again, never red.
//
// The runner never runs go. A go its tests run (a product missing from the cache that buildcache would build, a test
// that runs go build itself) meets a stand-in first on PATH, which refuses and records it, and a unit that then fails
// is Loom's, never the change's.

// blobFetchJobs is how many blobs a unit fetches at once.
const blobFetchJobs = 4

// A prebuiltPackage is one of the job's packages and what its tree's build holds for it.
type prebuiltPackage struct {
	test  protocol.TestPackage
	built builder.TreePackage
}

// A neededBlob is one blob a unit reads, and what it is, for the record and any error.
type neededBlob struct {
	sum  string
	what string
}

// runPrebuilt runs a test job whose tree Workshop built. Failed: a package's tests failed or the unit ran out of time.
// Broken: the instance was unfit (under the free floor with its blob cache empty, or without adamic's toolchain), the
// store couldn't give what the unit reads whole, or a test that failed had tried to run go.
func (run *unitRun) runPrebuilt(runContext context.Context, job *protocol.TestJob, root string, started, deadline time.Time) string {
	if run.options.Strict {
		// The instance is the runner's alone, so what earlier units left on its root is no one's.
		if err := trimRoot(runContext, root, io.Discard); err != nil {
			run.say(fmt.Sprintf("trimming %s: %v", root, err))
		}
	}
	cache, sources := newBlobCache(run.options, root), newSourceCache(root)
	err := cache.ready()
	if errors.Is(err, errUnfit) {
		// Then the sources no unit holds, before the unit is refused.
		sources.trim(0)
		err = cache.ready()
	}
	if err != nil {
		run.fail(protocol.PhaseStart, fmt.Errorf("refused as unfit: %w", err))
		return protocol.StatusBroken
	}
	index, err := run.treeIndex(runContext, job.Tree)
	if err != nil {
		run.fail(protocol.PhaseFetch, fmt.Errorf("%w: Loom's, never the change's", err))
		return protocol.StatusBroken
	}
	packages, needed, err := prebuiltPackages(job, index)
	if err != nil {
		run.fail(protocol.PhaseFetch, fmt.Errorf("%w: Loom's, never the change's", err))
		return protocol.StatusBroken
	}
	held, err := sources.hold(index.Source)
	if err != nil {
		run.fail(protocol.PhaseStart, fmt.Errorf("holding the tree's source: %w (the instance's, never the change's)", err))
		return protocol.StatusBroken
	}
	defer held.release()
	if held.ready {
		run.say("the tree's source, blob " + index.Source + ", is already unpacked here")
	} else {
		needed = append([]neededBlob{{sum: index.Source, what: "the tree's source"}}, needed...)
	}
	fetchStarted := time.Now()
	files, err := run.fetchBlobs(runContext, cache, needed)
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
		if err := sources.unpack(held, files[index.Source]); err != nil {
			run.fail(protocol.PhaseStart, fmt.Errorf("unpacking the tree's source: %w (Loom's, never the change's)", err))
			return protocol.StatusBroken
		}
	}
	sources.trim(keepSources)
	source := held.directory
	products := filepath.Join(run.directory, "adamic-build")
	binaries := filepath.Join(run.directory, "binaries")
	if err := unpackPrebuilt(index, packages, files, products, binaries); err != nil {
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
	attempts := filepath.Join(run.directory, "go-attempts")
	if err := run.refuseGo(environment, attempts); err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	out := filepath.Join(run.workspace, "loom-out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	status := run.runPackages(runContext, job, out, started, deadline, func(testContext context.Context, index int, part string) packageResult {
		return run.runBinary(testContext, packages[index], filepath.Join(binaries, fmt.Sprintf("%d.test", index)),
			packageEnvironment(environment, packages[index].test), source, part)
	})
	content, _ := os.ReadFile(attempts)
	tried := strings.Split(strings.TrimSpace(string(content)), "\n")
	if len(content) == 0 {
		return status
	}
	for _, attempt := range tried {
		run.say("a test ran " + attempt + ", and this runner runs no go")
	}
	if status == protocol.StatusFailed {
		run.fail(protocol.PhaseRun, fmt.Errorf("a test tried to build (%d go commands, the first %q), and this runner never builds: Loom's, never the change's", len(tried), tried[0]))
		return protocol.StatusBroken
	}
	return status
}

// treeIndex reads trees/<treeKey>.json from the store, as builder.ParseTree checks it, and holds it to its key: the
// index's own tree, Go release and the gate's environment must hash to the key the job named.
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
	if key := builder.TreeKey(index.Tree, index.Go, builder.GateEnvironment()); key != treeKey {
		return builder.TreeIndex{}, fmt.Errorf("the tree's index %s describes tree %s on %s, whose key is %s: the store is poisoned", name, index.Tree, index.Go, key)
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
		case built.Error != "":
			return nil, nil, fmt.Errorf("package %s didn't build on Workshop: %s", testPackage.Package, strings.TrimSpace(built.Error))
		}
		packages[position] = prebuiltPackage{test: testPackage, built: built}
		add(built.Binary, testPackage.Package+"'s test binary")
		for _, product := range built.Products {
			add(index.Products[product], "product "+product+", which "+testPackage.Package+"'s tests read")
		}
	}
	return packages, needed, nil
}

// fetchBlobs has every needed blob open, blobFetchJobs at a time, each fetch's bytes and seconds said on the unit's
// record. A blob that can't be had is named with what it is.
func (run *unitRun) fetchBlobs(fetchContext context.Context, cache blobCache, needed []neededBlob) (map[string]*os.File, error) {
	files := map[string]*os.File{}
	var mutex sync.Mutex
	var fetched, cached int64
	var cachedCount int
	errs := make([]error, len(needed))
	next := make(chan int)
	var group sync.WaitGroup
	for range blobFetchJobs {
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
	run.say(fmt.Sprintf("%d blobs: %d bytes from the store, %d bytes in %d blobs from the cache", len(needed), fetched, cached, cachedCount))
	return files, errors.Join(errs...)
}

// unpackPrebuilt unpacks each package's products into products (the unit's ADAMIC_BUILD_CACHE_DIR), each holding
// only its own entries, and each package's binary, gunzipped, into binaries as <index>.test. Both are the unit's own
// directory, so a runner killed partway leaves nothing a later unit reads.
func unpackPrebuilt(index builder.TreeIndex, packages []prebuiltPackage, files map[string]*os.File, products, binaries string) error {
	unpacked := map[string]bool{}
	for _, prebuilt := range packages {
		for _, product := range prebuilt.built.Products {
			if unpacked[product] {
				continue
			}
			unpacked[product] = true
			file := files[index.Products[product]]
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				return err
			}
			if err := builder.Unpack(file, products, builder.ProductEntries(product)); err != nil {
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
		file := files[prebuilt.built.Binary]
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return err
		}
		if err := gunzipTo(file, filepath.Join(binaries, fmt.Sprintf("%d.test", position))); err != nil {
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

// refuseGo puts a stand-in go first on the tests' PATH: it records each command a test ran it with in attempts and
// fails, so no test builds here and one that tried is named. It says on the record whether the tests' PATH held a
// go of its own behind it.
func (run *unitRun) refuseGo(environment map[string]string, attempts string) error {
	if found, err := lookPath("go", environment["PATH"]); err == nil {
		run.say("the tests' PATH holds go at " + found + ", behind the runner's refusal: no test builds here")
	} else {
		run.say("the tests' PATH holds no go: this runner has no Go toolchain, and no test builds here")
	}
	bin := filepath.Join(run.directory, "no-go")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return err
	}
	stand := "#!/bin/sh\n" +
		"printf 'go %s\\n' \"$*\" >> " + shellQuote(attempts) + "\n" +
		"echo \"loom-runner: go $*: this runner runs Workshop's prebuilt tests and never builds; a test that needs go is Loom's to fix, never the change's\" >&2\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(stand), 0o755); err != nil {
		return err
	}
	if environment["PATH"] == "" {
		// An empty entry would be the working directory.
		environment["PATH"] = bin
	} else {
		environment["PATH"] = bin + string(os.PathListSeparator) + environment["PATH"]
	}
	return nil
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
