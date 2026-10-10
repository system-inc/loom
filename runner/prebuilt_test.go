package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/builder/moduletest"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

// A prebuilt test job runs Workshop's binaries and never builds (#pcn6prz). Each mutant below must make a test here
// fail:
//
//	the stand-in go dropped from the tests' PATH: TestAPrebuiltUnitRunsGreenWithNoGo, TestATestThatBuildsIsLoomsNeverRed
//	a test that ran go and failed reported failed, not broken: TestATestThatBuildsIsLoomsNeverRed
//	a fetched blob's hash not checked: TestACorruptedBlobIsNeverRun
//	a cached blob trusted without hashing it again: TestACorruptedBlobIsNeverRun, TestALeftoverPartialFetchIsNeverTrusted
//	a blob the store lacks reported failed, not broken: TestWhatTheStoreLacksIsLoomsAndNamed
//	a cache hit that doesn't touch the blob, or eviction newest first: TestTheBlobCacheIsBoundedLeastRecentlyUsedFirst
//	the free floor not checked, or checked without evicting first: TestUnderTheFloorAUnitIsRefusedAsUnfit
//	a fetch written straight to the blob's name: TestTwoFetchesOfOneBlob
//	no per-blob lock, so one runner fetches a blob twice at once: TestTwoFetchesOfOneBlob
//	the index not held to its key: TestWhatTheStoreLacksIsLoomsAndNamed
//	a dead runner's unpacking never swept: TestASourceUnpackedPartwayIsNeverTrusted
//	a module cache unpacked straight at its name: TestAModuleCacheUnpackedPartwayIsNeverTrusted (a source assembled at
//	its name: assemble_test.go)
//	a source a unit holds removed, or sources kept oldest first: TestSourcesAreBoundedAndAHeldOneStays
//	prepare.sh's environment mode reaching the checkout, or taking an instance with no toolchain:
//	TestPrepareEnvironmentNeedsNoGitAndNoGo
//	the platform check dropped: TestATreeForAnotherPlatformIsRefusedAsUnfit
//	the stand-in delegating a build: TestABuildIsRefusedWithAGoHere
//	the stand-in refusing go version: TestARedBesideAReadOnlyGoQueryStaysRed
//	the runner's go not held to the tree's release: TestAGoOfAnotherReleaseIsUnfit
//	a query no go here answered not unfit: TestAGoQueryWithNoGoHereIsUnfit
//	the change's build failure broken, or no build-fail event: TestABuildErrorIsTheChangesRed
//	fetches not held to the unit's time: TestAStalledStoreBreaksTheUnitInItsTime
//	a wait for another fetch unbounded: TestAWaitForAnotherFetchIsBounded
//	cached blobs writable: TestACorruptedBlobIsNeverRun
//	eviction on a disk the cache isn't on: TestEvictionFreesOnlyTheCachesOwnDisk
//	a source trusted without its marker: TestAnUnmarkedSourceIsUnpackedAgain
//	partials swept by age, not by lock: TestALeftoverPartialFetchIsNeverTrusted
//	a live unpacking swept: TestASourceUnpackedPartwayIsNeverTrusted
//	a partial left when the disk filled: TestAFullDiskMidFetchLeavesNothing
//	-test.paniconexit0 dropped: TestAPanicAndAnExitMidRunAreRed
//	either cache swept without its sweep lock: TestASweepNeverTakesANameBeforeItsMakerLocksIt
//	the test's GOFLAGS passed to the runner's go: TestAnAllowedGoListNeverCompiles
//	a proxy query allowed: TestAProxyQueryIsRefused
//	go list's -buildmode refused: TestAListInAnotherBuildModeIsAnswered
//	modules read elsewhere than the tree's module cache, or no GOMODCACHE of the tree's own:
//	TestATestsModulesComeFromTheTreesModuleCache
//	the module cache not readied before the tests: TestAModuleTheCacheLacksIsLooms
//	module caches not counted toward the bound: TestTheModuleCachesCountTowardTheBound
//	a sweep back on os.RemoveAll, or leftovers not counted: TestASweepRemovesAReadOnlyLeftoverAndCountsWhatStays
//	the wait for the sweep lock unbounded: TestAWaitToMakeAPartialIsBounded
//	the go.work copy unused: TestATestsModulesComeFromTheTreesModuleCache
//	the go.work copy's paths left relative: TestAWorkspaceCopyNamesTheTreesDirectories
//	the stand-in forcing its own GOFLAGS or GOTOOLCHAIN: TestTheTestsGoFlagsAndToolchainPassThrough
//	GOFLAGS passed without its allow list, or any GOTOOLCHAIN passed: TestAFlagOrAToolchainOffTheListIsRefused,
//	TestAnAllowedGoListNeverCompiles
//	the go.work copy forced on a query outside the tree: TestAModuleOutsideTheTreeIsntForcedIntoItsWorkspace

const lowerPackage = protocol.AdamicModule + "/internal/lower"

// fixtureProductKey is the product the fixture's TestProduct reads (testdata/prebuilt/internal/lower).
const fixtureProductKey = "abababababababababababababababababababababababababababababababab"

// fixtureBinary is the fixture package's test binary, compiled once per test process: the test needs Go, the runner
// it drives never does.
var fixtureBinary struct {
	once    sync.Once
	content []byte
	err     error
	// goVersion and goBinary are the Go that compiled it: the release its tree's index names, and a real go a
	// runner may have.
	goVersion string
	goBinary  string
	// files are the fixture tree's, and modules its module cache, as build-tree archives it: the tree requires a
	// third-party module (moduletest), which internal/uses imports.
	files   map[string]string
	modules []byte
}

func prebuiltBinary(t *testing.T) []byte {
	t.Helper()
	fixtureBinary.once.Do(func() { fixtureBinary.err = buildFixture(t) })
	if fixtureBinary.err != nil {
		t.Fatalf("compiling the fixture: %v", fixtureBinary.err)
	}
	return fixtureBinary.content
}

// buildFixture makes the fixture's tree from testdata/prebuilt, with a requirement of moduletest's module and a
// package importing it, compiles its test binary, and archives its module cache, all from a module proxy on disk.
func buildFixture(t *testing.T) error {
	directory, err := os.MkdirTemp("", "loom-prebuilt-")
	if err != nil {
		return err
	}
	defer func() {
		os.Chmod(directory, 0o755)
		removeDirectory(directory)
	}()
	proxy, tree := filepath.Join(directory, "proxy"), filepath.Join(directory, "tree")
	moduletest.Proxy(t, proxy)
	// A workspace, as adamic's is, whose module's go.sum lacks the module's lines: go learns them into go.work.sum.
	fixtureBinary.files = map[string]string{"go.work": "go 1.27\n\nuse .\n", "internal/uses/uses.go": "package uses\n\nimport _ \"" + moduletest.Import + "\"\n"}
	for _, name := range []string{"go.mod", "internal/lower/lower_test.go", "internal/lower/testdata/fixture.txt"} {
		content, err := os.ReadFile(filepath.Join("testdata", "prebuilt", filepath.FromSlash(name)))
		if err != nil {
			return err
		}
		fixtureBinary.files[name] = string(content)
	}
	fixtureBinary.files["go.mod"] += "\n" + moduletest.Require
	for name, content := range fixtureBinary.files {
		os.MkdirAll(filepath.Dir(filepath.Join(tree, name)), 0o755)
		if err := os.WriteFile(filepath.Join(tree, name), []byte(content), 0o644); err != nil {
			return err
		}
	}
	binary := filepath.Join(directory, "lower.test")
	command := exec.Command("go", "test", "-c", "-o", binary, "./internal/lower")
	command.Dir = tree
	command.Env = append(os.Environ(), "GOFLAGS=-mod=readonly -modcacherw", "GOPROXY=file://"+proxy, "GOSUMDB=off", "GOMODCACHE="+filepath.Join(directory, "modcache"))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("go test -c: %v: %s", err, output)
	}
	if fixtureBinary.content, err = os.ReadFile(binary); err != nil {
		return err
	}
	if fixtureBinary.modules, err = builder.ModuleCacheArchive(tree, []string{"GOPROXY=file://" + proxy, "GOSUMDB=off"}); err != nil {
		return err
	}
	version, err := exec.Command("go", "env", "GOVERSION", "GOROOT").Output()
	fields := strings.Fields(string(version))
	if err != nil || len(fields) != 2 {
		return fmt.Errorf("go env GOVERSION GOROOT: %q %v", version, err)
	}
	fixtureBinary.goVersion, fixtureBinary.goBinary = fields[0], filepath.Join(fields[1], "bin", "go")
	return nil
}

// A prebuiltStore is the action store's public domain: GET of trees/<key>.json and blobs/<sha256>, no credentials,
// each GET counted.
type prebuiltStore struct {
	mutex   sync.Mutex
	objects map[string][]byte
	gets    map[string]int
	server  *httptest.Server
}

func newPrebuiltStore(t *testing.T) *prebuiltStore {
	store := &prebuiltStore{objects: map[string][]byte{}, gets: map[string]int{}}
	store.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		name := strings.TrimPrefix(request.URL.Path, "/")
		store.mutex.Lock()
		store.gets[name]++
		content, found := store.objects[name]
		store.mutex.Unlock()
		if request.Method != http.MethodGet || !found {
			http.NotFound(writer, request)
			return
		}
		writer.Write(content)
	}))
	t.Cleanup(store.server.Close)
	return store
}

func (store *prebuiltStore) put(name string, content []byte) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.objects[name] = content
}

func (store *prebuiltStore) remove(name string) {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	delete(store.objects, name)
}

func (store *prebuiltStore) blobGets() int {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	count := 0
	for name, gets := range store.gets {
		if strings.HasPrefix(name, "blobs/") {
			count += gets
		}
	}
	return count
}

// putBlob stores content under its own sha256.
func (store *prebuiltStore) putBlob(content []byte) string {
	sum := hashOf(content)
	store.put("blobs/"+sum, content)
	return sum
}

func gzipped(t *testing.T, content []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	writer.Write(content)
	writer.Close()
	return buffer.Bytes()
}

// A prebuiltTree is the fixture's tree built as Workshop builds it: its index, and every blob it names, in the store.
type prebuiltTree struct {
	key     string
	index   builder.TreeIndex
	binary  string
	source  string // the source's sum, its directory's name
	modules string
	store   *prebuiltStore
}

// fixtureCuts are where the fixture's source is cut into chunks: three, the second holding testdata alone.
var fixtureCuts = []string{"internal/lower/testdata/", "internal/uses/"}

// putChunks stores files as chunks, in name order, a new one starting at the first name at or after each cut, and
// returns them as an index lists them.
func (store *prebuiltStore) putChunks(t *testing.T, files []tarEntry, cuts ...string) []builder.SourceChunk {
	t.Helper()
	chunks, blobs := makeChunks(t, files, cuts...)
	for _, blob := range blobs {
		store.putBlob(blob)
	}
	return chunks
}

// makeChunks makes files chunks, in name order, a new one starting at the first name at or after each cut, and
// returns them as an index lists them, and their blobs.
func makeChunks(t *testing.T, files []tarEntry, cuts ...string) ([]builder.SourceChunk, map[string][]byte) {
	t.Helper()
	files = sortedEntries(files)
	chunks, blobs := []builder.SourceChunk{}, map[string][]byte{}
	start := 0
	for index := range files {
		last := index == len(files)-1
		if !last && !slices.ContainsFunc(cuts, func(cut string) bool { return files[index].name < cut && files[index+1].name >= cut }) {
			continue
		}
		run := files[start : index+1]
		blob := makeTar(t, run, true)
		blobs[hashOf(blob)] = blob
		chunks = append(chunks, builder.SourceChunk{Blob: hashOf(blob), First: run[0].name, Last: run[len(run)-1].name, Files: len(run), Bytes: int64(len(blob))})
		start = index + 1
	}
	return chunks, blobs
}

// sortedEntries is files in name order, as an archive holds them.
func sortedEntries(files []tarEntry) []tarEntry {
	files = append([]tarEntry{}, files...)
	slices.SortFunc(files, func(left, right tarEntry) int { return strings.Compare(left.name, right.name) })
	return files
}

// fixtureFiles are the fixture tree's files as tar entries.
func fixtureFiles() []tarEntry {
	files := []tarEntry{}
	for name, content := range fixtureBinary.files {
		files = append(files, tarEntry{name: name, kind: tar.TypeReg, content: content})
	}
	return files
}

// setSource makes chunks the tree's source and publishes its index.
func (tree *prebuiltTree) setSource(t *testing.T, chunks []builder.SourceChunk) {
	tree.index.Source, tree.source = chunks, builder.SourceSum(chunks)
	tree.publish(t)
}

func newPrebuiltTree(t *testing.T, store *prebuiltStore) *prebuiltTree {
	t.Helper()
	tree := &prebuiltTree{store: store}
	tree.binary = store.putBlob(gzipped(t, prebuiltBinary(t)))
	chunks := store.putChunks(t, fixtureFiles(), fixtureCuts...)
	tree.source = builder.SourceSum(chunks)
	tree.modules = store.putBlob(fixtureBinary.modules)
	product := store.putBlob(makeTar(t, []tarEntry{
		{name: fixtureProductKey + ".inputs", kind: tar.TypeReg, content: "{}\n"},
		{name: fixtureProductKey + "/tool", kind: tar.TypeReg, content: "the product\n", mode: 0o755},
	}, true))
	tree.index = builder.TreeIndex{Format: builder.TreeIndexFormat, Tree: strings.Repeat("7", 40), Go: fixtureBinary.goVersion, Goos: runtime.GOOS, Goarch: runtime.GOARCH,
		Source: chunks, Modules: tree.modules,
		Products: map[string]string{fixtureProductKey: product},
		Packages: map[string]builder.TreePackage{lowerPackage: {Package: lowerPackage, Directory: "internal/lower", Binary: tree.binary, Products: []string{fixtureProductKey}}}}
	tree.rekey(t)
	return tree
}

// rekey publishes the index under the key its own tree, Go and platform name.
func (tree *prebuiltTree) rekey(t *testing.T) {
	tree.key = planner.TreeKey(tree.index.Tree, tree.index.Go, tree.index.Goos, tree.index.Goarch)
	tree.publish(t)
}

// publish writes the tree's index as it stands.
func (tree *prebuiltTree) publish(t *testing.T) {
	content, err := json.Marshal(tree.index)
	if err != nil {
		t.Fatal(err)
	}
	tree.store.put("trees/"+tree.key+".json", content)
}

// A prebuiltFixture stands in for an instance with no Go: the runner's PATH holds bash alone, and prepare.sh is a stub
// whose environment mode writes a PATH of that directory, and whose checkout mode, which a prebuilt unit never
// reaches, leaves a marker and fails.
type prebuiltFixture struct {
	directory string
	bin       string
	store     *prebuiltStore
	tree      *prebuiltTree
}

func newPrebuiltFixture(t *testing.T) *prebuiltFixture {
	t.Helper()
	prebuiltBinary(t)
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	fixture := &prebuiltFixture{directory: t.TempDir(), store: newPrebuiltStore(t)}
	// The tree's GOMODCACHE is read-only, as go leaves it.
	t.Cleanup(func() { removeDirectory(fixture.directory) })
	fixture.bin = filepath.Join(fixture.directory, "bin")
	os.MkdirAll(fixture.bin, 0o755)
	if err := os.Symlink(bash, filepath.Join(fixture.bin, "bash")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fixture.bin)
	// A HOME of its own, so a module go fetched outside the tree's module cache would show.
	t.Setenv("HOME", filepath.Join(fixture.directory, "home"))
	os.MkdirAll(filepath.Join(fixture.directory, "home"), 0o755)
	if found, err := exec.LookPath("go"); err == nil {
		t.Fatalf("the scrubbed PATH still finds go at %s", found)
	}
	original := prepareScript
	t.Cleanup(func() { prepareScript = original })
	prepareScript = []byte(`#!/bin/bash
case "$1" in
	trim-only) exit 0 ;;
	environment)
		echo "$2" > "` + fixture.directory + `/environment-tree"
		printf 'PATH=%s\0HOME=%s\0' "` + fixture.bin + `" "${HOME}" > "$4" ;;
	*) : > "` + fixture.directory + `/checkout"; exit 2 ;;
esac
`)
	fixture.tree = newPrebuiltTree(t, fixture.store)
	return fixture
}

// withGo gives the fixture's runner a go of its own, on its PATH and not on its tests': the Go that compiled the
// fixture, or, when one is given, a script standing in for another.
func (fixture *prebuiltFixture) withGo(t *testing.T, script string) {
	t.Helper()
	directory := filepath.Join(fixture.directory, "real-go")
	os.MkdirAll(directory, 0o755)
	if script != "" {
		os.WriteFile(filepath.Join(directory, "go"), []byte(script), 0o755)
	} else if err := os.Symlink(fixtureBinary.goBinary, filepath.Join(directory, "go")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fixture.bin+string(os.PathListSeparator)+directory)
}

func (fixture *prebuiltFixture) options(t *testing.T) Options {
	options := testOptions(t)
	options.Strict = true
	options.Keep = true
	options.Root = filepath.Join(fixture.directory, "root")
	options.Store = fixture.store.server.URL
	return options
}

func (fixture *prebuiltFixture) unit(run string) protocol.Unit {
	job := goodTestJob()
	job.Tree = fixture.tree.key
	job.Packages = []protocol.TestPackage{{Package: lowerPackage, Run: run}}
	return testJobUnit(job)
}

func (fixture *prebuiltFixture) blobs() string {
	return filepath.Join(fixture.directory, "root", blobDirectoryName)
}

// testLog is the unit's loom-out/test.jsonl.gz, unzipped.
func testLog(t *testing.T, result Result) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(result.Workspace, "loom-out", "test.jsonl.gz"))
	if err != nil {
		t.Fatalf("no test log: %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := io.ReadAll(reader)
	return string(plain)
}

// hasEvent reports whether the go test -json lines hold an event of action for test ("" for the package's own).
func hasEvent(t *testing.T, lines string, action, test string) bool {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		var event struct{ Action, Package, Test string }
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("not go test -json: %q", line)
		}
		if event.Action == action && event.Test == test && event.Package == lowerPackage {
			return true
		}
	}
	return false
}

func TestAPrebuiltUnitRunsGreenWithNoGo(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	result, events, _ := runUnit(t, fixture.unit("^(TestA|TestGate|TestProduct)$"), fixture.options(t))
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if result.Status != protocol.StatusPassed {
		t.Fatalf("%s; errors %q\n%s", result.Status, errorPhases(events), runner)
	}
	t.Logf("the runner's record:\n%s", runner)
	lines := testLog(t, result)
	for _, test := range []string{"TestA", "TestGate", "TestGate/Nested", "TestProduct"} {
		if !hasEvent(t, lines, "pass", test) {
			t.Errorf("no pass for %s in\n%s", test, lines)
		}
	}
	if !hasEvent(t, lines, "start", "") || !hasEvent(t, lines, "pass", "") || hasEvent(t, lines, "run", "TestFail") {
		t.Errorf("the package's own start and pass, and only the selected tests, as go test -json says them:\n%s", lines)
	}
	// It never built: nothing ran go, the checkout was never made, and the record says the tests had no go.
	if !strings.Contains(runner, "no go here") || strings.Contains(runner, "a test ran go") {
		t.Errorf("the record doesn't say the runner had no go:\n%s", runner)
	}
	if _, err := os.Stat(filepath.Join(fixture.directory, "checkout")); err == nil {
		t.Error("a prebuilt unit prepared a checkout")
	}
	// Every fetch is on the record, with its bytes and seconds, and the source's chunks in a sum.
	for _, blob := range []string{fixture.tree.binary, fixture.tree.index.Products[fixtureProductKey]} {
		if !strings.Contains(runner, "blob "+blob+", ") || !strings.Contains(runner, " from the store") {
			t.Errorf("no fetch of %s on the record:\n%s", blob, runner)
		}
	}
	if !strings.Contains(runner, "the tree's source chunks: ") || !strings.Contains(runner, " in 3 chunks from the store, 0 bytes in 0 chunks from the cache") {
		t.Errorf("the source's chunks aren't on the record:\n%s", runner)
	}
	if !strings.Contains(runner, "fetched trees/"+fixture.tree.key+".json, ") || !strings.Contains(runner, "ready in ") {
		t.Errorf("the index's fetch and the time to be ready aren't on the record:\n%s", runner)
	}
	// The timing event says the same as fields (#g1jvdbq): the bytes from the store, none from the cache, and a time
	// for each phase it had.
	if timing := eventsOfType(events, "timing")[0].Timing; timing.StoreBytes == 0 || timing.CacheBytes != 0 || timing.PrepareSeconds <= 0 || timing.TestSeconds <= 0 {
		t.Errorf("the first unit's timing: %+v", timing)
	}
	// A second unit of the same tree reads every blob from the cache.
	before := fixture.store.blobGets()
	result, events, _ = runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusPassed || fixture.store.blobGets() != before {
		t.Fatalf("%s, %d blob GETs after %d; errors %q", result.Status, fixture.store.blobGets(), before, errorPhases(events))
	}
	if runner := strings.Join(outputLines(events, "runner"), "\n"); !strings.Contains(runner, "2 blobs: 0 bytes from the store") {
		t.Errorf("the second unit's blobs didn't come from the cache:\n%s", runner)
	}
	if timing := eventsOfType(events, "timing")[0].Timing; timing.StoreBytes != 0 || timing.CacheBytes == 0 {
		t.Errorf("the second unit's timing: %+v", timing)
	}
}

func TestAPrebuiltUnitsFailingTestIsRed(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	result, events, _ := runUnit(t, fixture.unit("^(TestA|TestFail)$"), fixture.options(t))
	if result.Status != protocol.StatusFailed {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	if runner := strings.Join(outputLines(events, "runner"), "\n"); !strings.Contains(runner, "failed "+lowerPackage+" TestFail") {
		t.Errorf("the failed test isn't named:\n%s", runner)
	}
	lines := testLog(t, result)
	if !hasEvent(t, lines, "fail", "TestFail") || !hasEvent(t, lines, "pass", "TestA") || !hasEvent(t, lines, "fail", "") {
		t.Errorf("go test -json's lines for a red package:\n%s", lines)
	}
}

// A product Workshop's build didn't name for the package is missing from the unit's cache, so the test builds it with
// go, as buildcache would: it meets the runner's refusal, and the unit is Loom's, never red, and never built.
func TestATestThatBuildsIsLoomsNeverRed(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	built := fixture.tree.index.Packages[lowerPackage]
	built.Products = []string{}
	fixture.tree.index.Packages[lowerPackage] = built
	fixture.tree.publish(t)
	result, events, _ := runUnit(t, fixture.unit("^(TestA|TestProduct)$"), fixture.options(t))
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "a test ran go for more than a read-only query") {
		t.Fatalf("%s; errors %q\n%s", result.Status, errorPhases(events), runner)
	}
	if !strings.Contains(runner, "a test ran go build -o ") {
		t.Errorf("the go command isn't named:\n%s", runner)
	}
	if _, err := os.Stat(filepath.Join(result.Workspace, "..", "adamic-build", fixtureProductKey, "tool")); err == nil {
		t.Error("the product was built")
	}
}

func TestWhatTheStoreLacksIsLoomsAndNamed(t *testing.T) {
	for name, test := range map[string]struct {
		change func(t *testing.T, tree *prebuiltTree)
		named  string
	}{
		"the index": {func(t *testing.T, tree *prebuiltTree) { tree.store.remove("trees/" + tree.key + ".json") }, "trees/"},
		"the binary": {func(t *testing.T, tree *prebuiltTree) { tree.store.remove("blobs/" + tree.binary) },
			lowerPackage + "'s test binary, blob "},
		"a chunk of the source": {func(t *testing.T, tree *prebuiltTree) { tree.store.remove("blobs/" + tree.index.Source[1].Blob) },
			"the tree's source chunk \"internal/lower/testdata/fixture.txt\" to \"internal/lower/testdata/fixture.txt\", blob "},
		"a product": {func(t *testing.T, tree *prebuiltTree) {
			tree.store.remove("blobs/" + tree.index.Products[fixtureProductKey])
		},
			"product " + fixtureProductKey},
		"the package": {func(t *testing.T, tree *prebuiltTree) {
			delete(tree.index.Packages, lowerPackage)
			tree.publish(t)
		}, "has no package " + lowerPackage},
		"the package's build": {func(t *testing.T, tree *prebuiltTree) {
			tree.index.Packages[lowerPackage] = builder.TreePackage{Package: lowerPackage, Products: []string{}, Error: "go test -c: undefined: x"}
			tree.publish(t)
		}, "didn't build on Workshop, for Workshop's reasons: go test -c: undefined: x"},
		"an index under another tree's key": {func(t *testing.T, tree *prebuiltTree) {
			tree.index.Tree = strings.Repeat("8", 40)
			tree.publish(t)
		}, "the store is poisoned"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPrebuiltFixture(t)
			test.change(t, fixture.tree)
			result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
			errors := errorPhases(events)
			if result.Status != protocol.StatusBroken || !strings.Contains(errors, test.named) || !strings.Contains(errors, "Loom's, never the change's") {
				t.Fatalf("%s; errors %q", result.Status, errors)
			}
			if _, err := os.Stat(filepath.Join(result.Workspace, "loom-out", "part-0.jsonl")); err == nil {
				t.Error("a test ran")
			}
		})
	}
}

func TestACorruptedBlobIsNeverRun(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	honest := fixture.store.objects["blobs/"+fixture.tree.binary]
	fixture.store.put("blobs/"+fixture.tree.binary, gzipped(t, []byte("#!/bin/sh\necho PASS\n")))
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "blob "+fixture.tree.binary+" hashes to ") {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	entries, _ := os.ReadDir(fixture.blobs())
	for _, entry := range entries {
		if strings.Contains(entry.Name(), fixture.tree.binary) {
			t.Errorf("the cache kept %s", entry.Name())
		}
	}
	// A copy in the cache that no longer hashes to its name is fetched again, never run.
	fixture.store.put("blobs/"+fixture.tree.binary, honest)
	if result, events, _ = runUnit(t, fixture.unit("^TestA$"), fixture.options(t)); result.Status != protocol.StatusPassed {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	cached := filepath.Join(fixture.blobs(), fixture.tree.binary)
	if info, err := os.Stat(cached); err != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("a cached blob isn't read-only: %v %v", info.Mode(), err)
	}
	os.Chmod(cached, 0o644)
	if err := os.WriteFile(cached, honest[:len(honest)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	result, events, _ = runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if result.Status != protocol.StatusPassed || !strings.Contains(runner, "in place of a cached copy that didn't hash to its name") {
		t.Fatalf("%s; errors %q\n%s", result.Status, errorPhases(events), runner)
	}
}

// Under the floor a unit evicts the blob cache, least recently used first, and starts once there is room; with the
// cache empty and still no room, it is refused as unfit, Loom's, having fetched nothing.
func TestUnderTheFloorAUnitIsRefusedAsUnfit(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	plant := func() []string {
		os.MkdirAll(fixture.blobs(), 0o755)
		sums := []string{}
		for index := range 3 {
			content := bytes.Repeat([]byte{byte('a' + index)}, 100)
			sum := hashOf(content)
			path := filepath.Join(fixture.blobs(), sum)
			os.WriteFile(path, content, 0o644)
			at := time.Now().Add(time.Duration(index-10) * time.Minute)
			os.Chtimes(path, at, at)
			sums = append(sums, sum)
		}
		return sums
	}
	cacheBytes := func() uint64 {
		total := uint64(0)
		entries, _ := os.ReadDir(fixture.blobs())
		for _, entry := range entries {
			if info, err := entry.Info(); err == nil {
				total += uint64(info.Size())
			}
		}
		return total
	}
	sums := plant()
	options := fixture.options(t)
	options.FreeFloorBytes = 1000
	options.free = func(path string) (uint64, error) { return 999, nil }
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), options)
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "refused as unfit") || !strings.Contains(errorPhases(events), "with the blob cache empty") {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	if cacheBytes() != 0 || fixture.store.blobGets() != 0 {
		t.Fatalf("an unfit unit left %d bytes cached and fetched %d blobs", cacheBytes(), fixture.store.blobGets())
	}
	// Room once the two oldest are gone: the newest stays, and the unit runs.
	sums = plant()
	options.free = func(path string) (uint64, error) {
		if strings.HasPrefix(path, fixture.directory) {
			return 1150 - min(1150, cacheBytes()), nil
		}
		return 1 << 40, nil
	}
	result, events, _ = runUnit(t, fixture.unit("^TestA$"), options)
	if result.Status != protocol.StatusPassed {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	for index, sum := range sums {
		_, err := os.Stat(filepath.Join(fixture.blobs(), sum))
		if (index < 2) != (err != nil) {
			t.Errorf("blob %d of 3, oldest first: present %v", index+1, err == nil)
		}
	}
}

// blobServer serves blobs by sha256 at /blobs/<sum>, counting GETs, holding each response at hold when it isn't nil
// after its first half.
type blobServer struct {
	mutex  sync.Mutex
	blobs  map[string][]byte
	gets   int
	half   chan struct{}
	hold   chan struct{}
	server *httptest.Server
}

func newBlobServer(t *testing.T, contents ...[]byte) (*blobServer, []string) {
	served := &blobServer{blobs: map[string][]byte{}}
	sums := []string{}
	for _, content := range contents {
		served.blobs[hashOf(content)] = content
		sums = append(sums, hashOf(content))
	}
	served.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		served.mutex.Lock()
		served.gets++
		content, found := served.blobs[strings.TrimPrefix(request.URL.Path, "/blobs/")]
		hold, half := served.hold, served.half
		served.mutex.Unlock()
		if !found {
			http.NotFound(writer, request)
			return
		}
		if hold == nil {
			time.Sleep(50 * time.Millisecond)
			writer.Write(content)
			return
		}
		writer.Write(content[:len(content)/2])
		writer.(http.Flusher).Flush()
		half <- struct{}{}
		<-hold
		writer.Write(content[len(content)/2:])
	}))
	t.Cleanup(served.server.Close)
	return served, sums
}

func testCache(t *testing.T, served *blobServer, limit int64) blobCache {
	return blobCache{directory: filepath.Join(t.TempDir(), blobDirectoryName), limit: limit, store: served.server.URL, client: served.server.Client(),
		free: func(string) (uint64, error) { return 1 << 40, nil }}
}

func readBlob(t *testing.T, cache blobCache, sum string) ([]byte, blobFetch) {
	t.Helper()
	file, fetch, err := cache.open(context.Background(), sum)
	if err != nil {
		t.Fatalf("blob %s: %v", sum, err)
	}
	defer file.Close()
	content, _ := io.ReadAll(file)
	return content, fetch
}

func TestTheBlobCacheIsBoundedLeastRecentlyUsedFirst(t *testing.T) {
	contents := [][]byte{bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("b"), 100), bytes.Repeat([]byte("c"), 100), bytes.Repeat([]byte("d"), 100)}
	served, sums := newBlobServer(t, contents...)
	cache := testCache(t, served, 250)
	for index, sum := range sums[:3] {
		readBlob(t, cache, sum)
		at := time.Now().Add(time.Duration(index-10) * time.Minute)
		os.Chtimes(filepath.Join(cache.directory, sum), at, at)
	}
	// a, the oldest fetched, is used again, so b is now the least recently used.
	if _, fetch := readBlob(t, cache, sums[0]); !fetch.cached {
		t.Fatal("a cached blob was fetched again")
	}
	if err := cache.trim(nil); err != nil {
		t.Fatal(err)
	}
	present := func() string {
		names := ""
		for index, sum := range sums {
			if _, err := os.Stat(filepath.Join(cache.directory, sum)); err == nil {
				names += string(rune('a' + index))
			}
		}
		return names
	}
	if got := present(); got != "ac" {
		t.Fatalf("after trimming to 250 bytes the cache holds %q, not a and c", got)
	}
	// A unit's own blobs stay whatever their age; the rest go oldest first.
	readBlob(t, cache, sums[3])
	at := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(cache.directory, sums[3]), at, at)
	cache.trim(map[string]bool{sums[3]: true})
	if got := present(); got != "ad" {
		t.Fatalf("the cache holds %q, not a and the unit's own d", got)
	}
}

// Two units of one runner wanting a blob at once fetch it once; and while a fetch is in flight, the blob's name holds
// nothing, so another runner sharing the root never reads half a blob.
func TestTwoFetchesOfOneBlob(t *testing.T) {
	content := bytes.Repeat([]byte("blob"), 1<<16)
	served, sums := newBlobServer(t, content)
	cache := testCache(t, served, 1<<30)
	var group sync.WaitGroup
	results := make([][]byte, 2)
	for index := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			results[index], _ = readBlob(t, cache, sums[0])
		}()
	}
	group.Wait()
	if !bytes.Equal(results[0], content) || !bytes.Equal(results[1], content) || served.gets != 1 {
		t.Fatalf("two fetches at once: %d and %d bytes of %d, %d GETs", len(results[0]), len(results[1]), len(content), served.gets)
	}
	other := testCache(t, served, 1<<30)
	served.half, served.hold = make(chan struct{}), make(chan struct{})
	done := make(chan []byte)
	go func() {
		got, _ := readBlob(t, other, sums[0])
		done <- got
	}()
	<-served.half
	// Half the body is sent; wait for the fetch to have written it.
	var entries []os.DirEntry
	for waited := time.Duration(0); waited < 5*time.Second; waited += 10 * time.Millisecond {
		listed, _ := os.ReadDir(other.directory)
		entries = slices.DeleteFunc(listed, func(entry os.DirEntry) bool { return entry.Name() == sweepLockName })
		if len(entries) > 0 {
			if info, err := entries[0].Info(); err == nil && info.Size() >= int64(len(content)/2) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(other.directory, sums[0])); err == nil {
		t.Error("half a blob is at its name while it is fetched")
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), partialPrefix) {
		t.Errorf("the fetch in flight isn't a partial: %v", entries)
	}
	close(served.hold)
	if got := <-done; !bytes.Equal(got, content) {
		t.Fatalf("got %d bytes of %d", len(got), len(content))
	}
}

// What a killed fetch left (a partial, or a blob's name on bytes that never finished) is never read as the blob: the
// blob is fetched again, a partial whose lock no one holds goes before the next unit, however new, and a live one,
// another runner's fetch holding its lock, stays, however old.
func TestALeftoverPartialFetchIsNeverTrusted(t *testing.T) {
	content := bytes.Repeat([]byte("whole"), 1000)
	served, sums := newBlobServer(t, content)
	cache := testCache(t, served, 1<<30)
	os.MkdirAll(cache.directory, 0o755)
	dead := filepath.Join(cache.directory, partialPrefix+sums[0]+"-dead")
	live := filepath.Join(cache.directory, partialPrefix+sums[0]+"-live")
	os.WriteFile(dead, content[:100], 0o644)
	os.WriteFile(live, content[:100], 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(live, old, old)
	holder, err := os.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err = syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(cache.directory, sums[0]), content[:len(content)/2], 0o644)
	got, fetch := readBlob(t, cache, sums[0])
	if !bytes.Equal(got, content) || fetch.cached || !fetch.corrupt {
		t.Fatalf("got %d bytes of %d, %+v", len(got), len(content), fetch)
	}
	if err := cache.ready(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); err == nil {
		t.Error("a dead fetch's partial stayed")
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("another runner's fetch in flight was removed")
	}
}

// The real prepare.sh's environment mode readies a prebuilt unit's environment over an unpacked source with neither
// git, go nor npm on its PATH, and refuses an instance without adamic's toolchain as the instance's (exit 2). The
// source's npm packages came with it (#v03v751): it installs none and leaves them as they are.
func TestPrepareEnvironmentNeedsNoGitAndNoGo(t *testing.T) {
	directory := t.TempDir()
	bin, home, tree := filepath.Join(directory, "bin"), filepath.Join(directory, "home"), filepath.Join(directory, "source")
	for _, path := range []string{bin, home, tree} {
		os.MkdirAll(path, 0o755)
	}
	nodeModules := filepath.Join(tree, "stage3", "api", "node_modules")
	os.MkdirAll(filepath.Join(nodeModules, "@types", "node"), 0o755)
	os.WriteFile(filepath.Join(tree, "stage3", "api", "package-lock.json"), []byte("{}\n"), 0o644)
	os.WriteFile(filepath.Join(nodeModules, "@types", "node", "package.json"), []byte(`{"version":"25.3.3"}`+"\n"), 0o644)
	installed, err := os.Stat(nodeModules)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"bash", "df", "awk", "sort", "head", "mkdir", "env"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s here", tool)
		}
		os.Symlink(path, filepath.Join(bin, tool))
	}
	script := filepath.Join(directory, "prepare.sh")
	os.WriteFile(script, prepareScript, 0o700)
	environment := filepath.Join(directory, "environment")
	prepare := func() (int, string) {
		command := exec.Command(filepath.Join(bin, "bash"), script, "environment", tree, "", environment, filepath.Join(directory, "root"))
		command.Env = []string{"PATH=" + bin, "HOME=" + home}
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	if code, output := prepare(); code != 2 || !strings.Contains(output, "no adamic toolchain") {
		t.Fatalf("an instance without adamic's toolchain: exit %d: %s", code, output)
	}
	os.MkdirAll(filepath.Join(home, "adamic-tools"), 0o755)
	os.WriteFile(filepath.Join(home, "adamic-tools", "env.sh"), []byte("export ADAMIC_TOOLCHAIN_LOADED=1\n"), 0o644)
	if code, output := prepare(); code != 0 {
		t.Fatalf("exit %d: %s", code, output)
	}
	if content, _ := os.ReadFile(environment); !bytes.Contains(content, []byte("ADAMIC_TOOLCHAIN_LOADED=1\x00")) {
		t.Fatalf("the environment lacks the toolchain's: %q", content)
	}
	entries, _ := os.ReadDir(nodeModules)
	if now, err := os.Stat(nodeModules); err != nil || !os.SameFile(now, installed) || len(entries) != 1 || entries[0].Name() != "@types" {
		t.Errorf("the source's node_modules changed: %v, %v", entries, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "root", "adamic-npm")); err == nil {
		t.Error("prepare.sh kept npm trees for a prebuilt unit")
	}
}

// A source refused partway (a poisoned archive, a full disk, a runner killed) leaves nothing at the source's name, so
// the next unit of the tree unpacks it again, and what a dead runner's unpacking left is swept, never used.
func TestASourceUnpackedPartwayIsNeverTrusted(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	files := append(fixtureFiles(), tarEntry{name: "../outside", kind: tar.TypeReg, content: "x"})
	fixture.tree.setSource(t, fixture.store.putChunks(t, files, fixtureCuts...))
	sources := filepath.Join(fixture.directory, "root", sourceDirectoryName)
	// A dead runner's unpacking, whose lock is free, and a live one's, holding it.
	leftover := filepath.Join(sources, unpackingPrefix+fixture.tree.source+"-dead")
	os.MkdirAll(leftover, 0o755)
	live := filepath.Join(sources, unpackingPrefix+fixture.tree.source+"-live")
	os.MkdirAll(live, 0o755)
	holder, err := os.Open(live)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err = syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
		if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "assembling the tree's source") {
			t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
		}
		if _, err := os.Stat(filepath.Join(sources, fixture.tree.source)); err == nil {
			t.Fatal("a source refused partway is at its name")
		}
	}
	entries, _ := os.ReadDir(sources)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), unpackingPrefix) && filepath.Join(sources, entry.Name()) != live {
			t.Errorf("%s is left", entry.Name())
		}
	}
	if _, err := os.Stat(live); err != nil {
		t.Error("a live runner's unpacking was removed")
	}
}

// A tree's module cache refused partway leaves nothing at its name either: it is one archive, unpacked whole, never
// at its name until it is.
func TestAModuleCacheUnpackedPartwayIsNeverTrusted(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.tree.modules = fixture.store.putBlob(makeTar(t, []tarEntry{
		{name: "cache/download/a/@v/list", kind: tar.TypeReg, content: "v1.0.0\n"},
		{name: "../outside", kind: tar.TypeReg, content: "x"},
	}, true))
	fixture.tree.index.Modules = fixture.tree.modules
	fixture.tree.publish(t)
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "unpacking the tree's module cache") {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	if _, err := os.Lstat(filepath.Join(fixture.directory, "root", sourceDirectoryName, fixture.tree.modules)); err == nil {
		t.Fatal("a module cache refused partway is at its name")
	}
}

// A runner keeps keepSources trees' sources, least recently used going first, and never one a unit holds.
func TestSourcesAreBoundedAndAHeldOneStays(t *testing.T) {
	cache := newSourceCache(t.TempDir())
	held := []*heldSource{}
	for index := range 3 {
		archive := makeTar(t, []tarEntry{{name: "file", kind: tar.TypeReg, content: fmt.Sprint(index)}}, true)
		source, err := cache.hold(context.Background(), hashOf(archive))
		if err != nil {
			t.Fatal(err)
		}
		if err = cache.unpack(context.Background(), source, bytes.NewReader(archive)); err != nil || !source.ready {
			t.Fatalf("unpacking: %v", err)
		}
		at := time.Now().Add(time.Duration(index-10) * time.Minute)
		os.Chtimes(source.directory, at, at)
		held = append(held, source)
	}
	held[1].release()
	held[2].release()
	cache.trim(1)
	present := ""
	for index, source := range held {
		if _, err := os.Stat(filepath.Join(source.tree(), "file")); err == nil {
			present += fmt.Sprint(index)
		}
	}
	// 0 is the oldest but held; 2 is the newest; 1 goes.
	if present != "02" {
		t.Fatalf("sources %q are left, not the held 0 and the newest 2", present)
	}
	again, err := cache.hold(context.Background(), held[2].sum)
	if err != nil || !again.ready {
		t.Errorf("a kept source isn't found again: %v", err)
	}
	again.release()
	held[0].release()
}

// A tree built for another platform is refused as unfit before anything is fetched: its binaries can't run here.
// Mutant: the platform check dropped.
func TestATreeForAnotherPlatformIsRefusedAsUnfit(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.tree.index.Goos = "plan9"
	fixture.tree.rekey(t)
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "built for plan9/"+runtime.GOARCH) {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	if gets := fixture.store.blobGets(); gets != 0 {
		t.Fatalf("fetched %d blobs for another platform", gets)
	}
}

// buildcache asks go for its version and environment to key a product, and a runner with a go answers, as the tree's
// release: a real red beside such a test stays red, and the record says what was answered.
func TestARedBesideAReadOnlyGoQueryStaysRed(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	result, events, _ := runUnit(t, fixture.unit("^(TestToolKey|TestFail)$"), fixture.options(t))
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if result.Status != protocol.StatusFailed {
		t.Fatalf("a red beside a go query read as %s; errors %q\n%s", result.Status, errorPhases(events), runner)
	}
	for _, query := range []string{"go version (exit 0)", "go env GOVERSION (exit 0)"} {
		if !strings.Contains(runner, "answered for the tests, 1 times: "+query) {
			t.Errorf("%q isn't on the record:\n%s", query, runner)
		}
	}
	if !strings.Contains(runner, "answers the tests' read-only go queries as "+fixtureBinary.goVersion) {
		t.Errorf("the record doesn't name the go that answers:\n%s", runner)
	}
}

// With no go here, a query goes unanswered, and the unit is unfit, whatever its tests said.
func TestAGoQueryWithNoGoHereIsUnfit(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	result, events, _ := runUnit(t, fixture.unit("^TestToolKey$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), `refused as unfit: the tests need Go for "go version"`) {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
}

// A go that isn't the tree's release can't answer for it: the unit is unfit before its tests run.
func TestAGoOfAnotherReleaseIsUnfit(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "#!/bin/sh\necho go1.0.0\n")
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), `is "go1.0.0" under GOTOOLCHAIN=local (<nil>), and the tree was built with `+fixtureBinary.goVersion) {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	if _, err := os.Stat(filepath.Join(result.Workspace, "loom-out", "part-0.jsonl")); err == nil {
		t.Error("a test ran")
	}
}

// A runner with a go still builds nothing: a test's go build is refused, and its red is Loom's.
func TestABuildIsRefusedWithAGoHere(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	built := fixture.tree.index.Packages[lowerPackage]
	built.Products = []string{}
	fixture.tree.index.Packages[lowerPackage] = built
	fixture.tree.publish(t)
	result, events, _ := runUnit(t, fixture.unit("^TestProduct$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), `a test ran go for more than a read-only query (1 go commands, the first "go build -o `) {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
}

// A package whose test code didn't compile on Workshop is the change's red, said as go test -json says it.
func TestABuildErrorIsTheChangesRed(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	built := fixture.tree.index.Packages[lowerPackage]
	built.Error, built.Failure = "go test -c: exit status 1\n# "+lowerPackage+" ["+lowerPackage+".test]\n./lower_test.go:3:2: undefined: foo\n", builder.ChangeFailure
	fixture.tree.index.Packages[lowerPackage] = built
	fixture.tree.publish(t)
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusFailed {
		t.Fatalf("a change that doesn't compile read as %s; errors %q", result.Status, errorPhases(events))
	}
	lines := testLog(t, result)
	importPath := lowerPackage + " [" + lowerPackage + ".test]"
	for _, want := range []string{`{"ImportPath":"` + importPath + `","Action":"build-output","Output":"./lower_test.go:3:2: undefined: foo\n"}`,
		`{"ImportPath":"` + importPath + `","Action":"build-fail"}`, `"Output":"FAIL\t` + lowerPackage + ` [build failed]\n"`, `"FailedBuild":"` + importPath + `"`} {
		if !strings.Contains(lines, want) {
			t.Errorf("no %s in\n%s", want, lines)
		}
	}
	if !hasEvent(t, lines, "fail", "") {
		t.Errorf("the package doesn't fail:\n%s", lines)
	}
}

// A store that answers and then stalls breaks the unit within its time, never wedges it.
func TestAStalledStoreBreaksTheUnitInItsTime(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	stall := make(chan struct{})
	defer close(stall)
	inner := fixture.store.server.Config.Handler
	fixture.store.server.Config.Handler = http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/blobs/") {
			inner.ServeHTTP(writer, request)
			return
		}
		writer.Header().Set("Content-Length", "1000000")
		writer.WriteHeader(http.StatusOK)
		writer.Write([]byte("x"))
		writer.(http.Flusher).Flush()
		select {
		case <-stall:
		case <-time.After(20 * time.Second):
		}
	})
	unit := fixture.unit("^TestA$")
	unit.TimeoutSeconds = 2
	started := time.Now()
	result, events, _ := runUnit(t, unit, fixture.options(t))
	if result.Status != protocol.StatusBroken || time.Since(started) > 8*time.Second || !strings.Contains(errorPhases(events), "deadline exceeded") {
		t.Fatalf("a 2 s unit on a stalled store: %s after %.1f s; errors %q", result.Status, time.Since(started).Seconds(), errorPhases(events))
	}
}

// A binary still running at the unit's deadline is killed, and the unit is red, timed out, as go test's was.
func TestABinaryPastTheDeadlineIsKilled(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	unit := fixture.unit("^TestSleep$")
	unit.TimeoutSeconds = 3
	options := fixture.options(t)
	options.KillGrace = time.Second
	started := time.Now()
	result, events, _ := runUnit(t, unit, options)
	exits := eventsOfType(events, "exit")
	if result.Status != protocol.StatusFailed || len(exits) != 1 || !exits[0].TimedOut || time.Since(started) > 15*time.Second {
		t.Fatalf("%s after %.1f s, exits %+v; errors %q", result.Status, time.Since(started).Seconds(), exits, errorPhases(events))
	}
}

// A test that panics, and one that exits 0 mid-run (-test.paniconexit0), are each the change's red.
func TestAPanicAndAnExitMidRunAreRed(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	for _, test := range []string{"TestPanic", "TestExit"} {
		result, events, _ := runUnit(t, fixture.unit("^"+test+"$"), fixture.options(t))
		lines := testLog(t, result)
		if result.Status != protocol.StatusFailed || !hasEvent(t, lines, "fail", test) || !hasEvent(t, lines, "fail", "") {
			t.Errorf("%s: %s; errors %q\n%s", test, result.Status, errorPhases(events), lines)
		}
	}
}

// A disk that fills mid-fetch breaks the unit, named, and leaves no partial and no blob at its name.
func TestAFullDiskMidFetchLeavesNothing(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	original := copyBlob
	t.Cleanup(func() { copyBlob = original })
	copyBlob = func(destination io.Writer, source io.Reader) (int64, error) {
		written, _ := io.CopyN(destination, source, 10)
		return written, &os.PathError{Op: "write", Path: "partial", Err: syscall.ENOSPC}
	}
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "the disk filled while it was fetched") {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	entries, _ := os.ReadDir(fixture.blobs())
	if entries = slices.DeleteFunc(entries, func(entry os.DirEntry) bool { return entry.Name() == sweepLockName }); len(entries) != 0 {
		t.Fatalf("a full disk left %v", entries)
	}
}

// Removing blobs frees only the cache's own disk: a short disk elsewhere is unfit, and the cache keeps its blobs.
func TestEvictionFreesOnlyTheCachesOwnDisk(t *testing.T) {
	served, sums := newBlobServer(t, []byte("a blob"))
	cache := testCache(t, served, 1<<30)
	readBlob(t, cache, sums[0])
	other := "/dev"
	own, _ := deviceOf(cache.directory)
	if device, err := deviceOf(other); err != nil || device == own {
		t.Skipf("%s isn't another filesystem here", other)
	}
	cache.watched, cache.floor = []string{cache.directory, other}, 1000
	cache.free = func(path string) (uint64, error) {
		if path == other {
			return 0, nil
		}
		return 1 << 40, nil
	}
	if err := cache.ready(); !errors.Is(err, errUnfitElsewhere) {
		t.Fatalf("a short disk elsewhere: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache.directory, sums[0])); err != nil {
		t.Fatal("the cache gave up a blob that frees nothing on the short disk")
	}
}

// A directory at a source's name without its completion marker (a crash that lost part of it) is never trusted: it
// is unpacked again, whole.
func TestAnUnmarkedSourceIsUnpackedAgain(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	hollow := filepath.Join(fixture.directory, "root", sourceDirectoryName, fixture.tree.source)
	os.MkdirAll(filepath.Join(hollow, "internal", "lower"), 0o755)
	os.WriteFile(filepath.Join(hollow, "junk"), []byte("x"), 0o644)
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if result.Status != protocol.StatusPassed || strings.Contains(runner, "already assembled") {
		t.Fatalf("%s; errors %q\n%s", result.Status, errorPhases(events), runner)
	}
	if _, err := os.Stat(filepath.Join(hollow, "junk")); err == nil {
		t.Error("the unmarked source was trusted")
	}
	if marker, err := os.ReadFile(filepath.Join(hollow, sourceMarker)); err != nil || string(marker) != string(markerContent(fixture.tree.source)) {
		t.Errorf("the source's marker: %q %v", marker, err)
	}
}

// A unit waiting on another unit's fetch of the same blob waits only as long as its own time.
func TestAWaitForAnotherFetchIsBounded(t *testing.T) {
	served, sums := newBlobServer(t, []byte("a blob"))
	cache := testCache(t, served, 1<<30)
	unlock, err := cache.lock(context.Background(), sums[0])
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	waitContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := cache.open(waitContext, sums[0])
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "waiting for another unit's fetch") {
			t.Fatalf("the wait ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wait for another unit's fetch outlived the unit's time")
	}
}

// A fetch or an unpack makes its in-progress name under its directory's shared sweep lock and holds that name's own
// lock before letting go, so a sweep waits for it and never takes a name whose maker hasn't locked it yet.
func TestASweepNeverTakesANameBeforeItsMakerLocksIt(t *testing.T) {
	root := t.TempDir()
	cache, sources := blobCache{directory: filepath.Join(root, blobDirectoryName), limit: 1 << 30}, newSourceCache(root)
	for _, made := range []struct {
		directory string
		name      string
		sweep     func()
	}{
		{cache.directory, partialPrefix + strings.Repeat("a", 64) + "-making", func() { cache.blobs() }},
		{sources.directory, unpackingPrefix + strings.Repeat("a", 64) + "-making", sources.sweep},
	} {
		os.MkdirAll(made.directory, 0o755)
		// A maker between making its name and locking it, holding the sweep lock shared.
		maker, err := lockDirectory(made.directory, syscall.LOCK_SH)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(made.directory, made.name)
		os.Mkdir(path, 0o755)
		swept := make(chan struct{})
		go func() {
			made.sweep()
			close(swept)
		}()
		select {
		case <-swept:
			t.Fatalf("%s was swept while its maker held the sweep lock", made.name)
		case <-time.After(200 * time.Millisecond):
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s was taken before its maker locked it", made.name)
		}
		maker.Close()
		<-swept
	}
}

// A test's allowed go list runs with the runner's GOFLAGS, never the test's: GOFLAGS=-export=true can't make it compile.
func TestAnAllowedGoListNeverCompiles(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	result, events, _ := runUnit(t, fixture.unit("^TestListExport$"), fixture.options(t))
	if result.Status != protocol.StatusPassed {
		t.Fatalf("the stand-in let go list compile: %s; errors %q\n%s", result.Status, errorPhases(events), testLog(t, result))
	}
}

// adamic keys the checker archive on go list -deps -json -buildmode=c-archive (#nee3cfe): the mode changes which
// packages are listed, and nothing compiles, so the stand-in answers it.
func TestAListInAnotherBuildModeIsAnswered(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	result, events, _ := runUnit(t, fixture.unit("^TestListBuildMode$"), fixture.options(t))
	if result.Status != protocol.StatusPassed {
		t.Fatalf("go list -buildmode=c-archive: %s; errors %q\n%s", result.Status, errorPhases(events), testLog(t, result))
	}
}

// A test's go list of a package importing a third-party module reads it from the tree's module cache, put in the
// tree's own GOMODCACHE before the tests, and nothing goes to HOME's.
func TestATestsModulesComeFromTheTreesModuleCache(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	result, events, _ := runUnit(t, fixture.unit("^TestModules$"), fixture.options(t))
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if result.Status != protocol.StatusPassed || !strings.Contains(runner, "the tree's modules are in ") {
		t.Fatalf("%s; errors %q\n%s\n%s", result.Status, errorPhases(events), runner, testLog(t, result))
	}
	moduleCache := newSourceCache(filepath.Join(fixture.directory, "root")).moduleCache(fixture.tree.modules)
	if _, err := os.Stat(filepath.Join(moduleCache, moduletest.Path+"@"+moduletest.Version, "dep.go")); err != nil {
		t.Errorf("the module isn't in the tree's GOMODCACHE: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fixture.directory, "home", "go")); err == nil {
		t.Error("go wrote HOME's module cache")
	}
	// The tree's source, shared by every unit of the tree, is as Workshop archived it: go wrote no go.work.sum there.
	source := filepath.Join(fixture.directory, "root", sourceDirectoryName, fixture.tree.source, sourceTreeName)
	filepath.WalkDir(source, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		name, _ := filepath.Rel(source, path)
		if content, err := os.ReadFile(path); err != nil || string(content) != fixtureBinary.files[filepath.ToSlash(name)] {
			t.Errorf("the source's %s isn't as archived: %q", name, content)
		}
		return nil
	})
}

// A module the tree's module cache lacks breaks the unit, named, before any test runs.
func TestAModuleTheCacheLacksIsLooms(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	versions := moduletest.Path + "/@v/"
	fixture.tree.index.Modules = fixture.store.putBlob(makeTar(t, []tarEntry{
		{name: versions + "list", kind: tar.TypeReg, content: moduletest.Version + "\n"},
		{name: versions + moduletest.Version + ".info", kind: tar.TypeReg, content: `{"Version":"` + moduletest.Version + `"}`},
	}, true))
	fixture.tree.publish(t)
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "a module the tree needs isn't in its module cache, blob "+fixture.tree.index.Modules) {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	if _, err := os.Stat(filepath.Join(result.Workspace, "loom-out", "part-0.jsonl")); err == nil {
		t.Error("a test ran")
	}
}

// A go list that asks a module proxy (-u, -versions, -retracted) is refused like a build, and its red is Loom's.
func TestAProxyQueryIsRefused(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	result, events, _ := runUnit(t, fixture.unit("^TestListUpdates$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), `the first "go list -m -u all (`) {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
}

// The trees' GOMODCACHEs count toward the blob cache's bound, so a full module cache makes blobs go.
func TestTheModuleCachesCountTowardTheBound(t *testing.T) {
	contents := [][]byte{bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("b"), 100), bytes.Repeat([]byte("c"), 100)}
	served, sums := newBlobServer(t, contents...)
	root := t.TempDir()
	cache := blobCache{directory: filepath.Join(root, blobDirectoryName), limit: 350, store: served.server.URL, client: served.server.Client()}
	for index, sum := range sums {
		readBlob(t, cache, sum)
		at := time.Now().Add(time.Duration(index-10) * time.Minute)
		os.Chtimes(filepath.Join(cache.directory, sum), at, at)
	}
	moduleCache := newSourceCache(root).moduleCache(strings.Repeat("d", 64))
	os.MkdirAll(moduleCache, 0o755)
	os.WriteFile(filepath.Join(moduleCache, "module.zip"), bytes.Repeat([]byte("m"), 200), 0o644)
	if err := cache.trim(nil); err != nil {
		t.Fatal(err)
	}
	for index, sum := range sums {
		_, err := os.Stat(filepath.Join(cache.directory, sum))
		if (index < 2) != (err != nil) {
			t.Errorf("blob %d of 3, oldest first: present %v, with 200 bytes in a module cache and a 350 byte bound", index+1, err == nil)
		}
	}
}

// A removal a dead runner left holding a GOMODCACHE, read-only as go leaves it, is swept whole; and what is left in the
// sources' directory, a live unpacking among it, counts toward the blob cache's bound.
func TestASweepRemovesAReadOnlyLeftoverAndCountsWhatStays(t *testing.T) {
	root := t.TempDir()
	sources := newSourceCache(root)
	left := filepath.Join(sources.directory, sourceRemovingPrefix+"dead", "tree", moduletest.Path+"@"+moduletest.Version)
	os.MkdirAll(left, 0o755)
	os.WriteFile(filepath.Join(left, "go.mod"), []byte("module x\n"), 0o444)
	os.Chmod(left, 0o555)
	t.Cleanup(func() { removeDirectory(root) })
	sources.sweep()
	if _, err := os.Stat(filepath.Join(sources.directory, sourceRemovingPrefix+"dead")); err == nil {
		t.Fatal("a read-only leftover stayed")
	}
	contents := [][]byte{bytes.Repeat([]byte("a"), 100), bytes.Repeat([]byte("b"), 100), bytes.Repeat([]byte("c"), 100)}
	served, sums := newBlobServer(t, contents...)
	cache := blobCache{directory: filepath.Join(root, blobDirectoryName), limit: 350, store: served.server.URL, client: served.server.Client()}
	for index, sum := range sums {
		readBlob(t, cache, sum)
		at := time.Now().Add(time.Duration(index-10) * time.Minute)
		os.Chtimes(filepath.Join(cache.directory, sum), at, at)
	}
	live := filepath.Join(sources.directory, unpackingPrefix+"live")
	os.MkdirAll(live, 0o755)
	os.WriteFile(filepath.Join(live, "file"), bytes.Repeat([]byte("u"), 200), 0o644)
	if err := cache.trim(nil); err != nil {
		t.Fatal(err)
	}
	for index, sum := range sums {
		_, err := os.Stat(filepath.Join(cache.directory, sum))
		if (index < 2) != (err != nil) {
			t.Errorf("blob %d of 3, oldest first: present %v, with 200 bytes in an unpacking and a 350 byte bound", index+1, err == nil)
		}
	}
}

// A fetch waiting to make its partial while a sweep holds the cache's sweep lock waits only as long as its unit.
func TestAWaitToMakeAPartialIsBounded(t *testing.T) {
	served, sums := newBlobServer(t, []byte("a blob"))
	cache := testCache(t, served, 1<<30)
	os.MkdirAll(cache.directory, 0o755)
	sweep, err := lockDirectory(cache.directory, syscall.LOCK_EX)
	if err != nil {
		t.Fatal(err)
	}
	defer sweep.Close()
	waitContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := cache.open(waitContext, sums[0])
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
			t.Fatalf("the wait ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a wait for the sweep lock outlived the unit's time")
	}
}

// The copy of a tree's go.work names the tree's directories absolutely, so go finds them from outside the tree, and
// carries its go.work.sum.
func TestAWorkspaceCopyNamesTheTreesDirectories(t *testing.T) {
	prebuiltBinary(t)
	source, directory := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(source, "go.work"), []byte("go 1.27\n\nuse (\n\t.\n\t./sub\n)\n\nreplace example.com/x v1.0.0 => ./x\n\nreplace example.com/y => example.com/z v1.2.0\n"), 0o644)
	os.WriteFile(filepath.Join(source, "go.work.sum"), []byte("sums\n"), 0o644)
	copied, err := workspaceCopy(context.Background(), fixtureBinary.goBinary, append(os.Environ(), "GOTOOLCHAIN=local"), source, directory)
	if err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(copied)
	for _, want := range []string{"use " + strconv.Quote(source), "use " + strconv.Quote(filepath.Join(source, "sub")),
		"replace example.com/x v1.0.0 => " + strconv.Quote(filepath.Join(source, "x")), "replace example.com/y => example.com/z v1.2.0"} {
		if !strings.Contains(string(content), want+"\n") {
			t.Errorf("the copy lacks %q:\n%s", want, content)
		}
	}
	if sums, err := os.ReadFile(filepath.Join(directory, "go.work.sum")); err != nil || string(sums) != "sums\n" {
		t.Errorf("go.work.sum: %q %v", sums, err)
	}
}

// The tests' own GOFLAGS and GOTOOLCHAIN reach the runner's go as they set them, when allowed, so go env answers as
// it did on Workshop, where adamic's GoInputs keyed the products.
func TestTheTestsGoFlagsAndToolchainPassThrough(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	result, events, _ := runUnit(t, fixture.unit("^TestGoEnv$"), fixture.options(t))
	if result.Status != protocol.StatusPassed {
		t.Fatalf("%s; errors %q\n%s", result.Status, errorPhases(events), testLog(t, result))
	}
}

// A GOFLAGS flag off the allow list, or a toolchain neither the runner's nor the tree's, is refused and recorded, and
// the red it makes is Loom's.
func TestAFlagOrAToolchainOffTheListIsRefused(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	for test, refused := range map[string]string{"TestToolexec": "GOFLAGS=-toolexec=/bin/echo", "TestOtherToolchain": "GOTOOLCHAIN=go1.99.0"} {
		result, events, _ := runUnit(t, fixture.unit("^"+test+"$"), fixture.options(t))
		if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), refused) {
			t.Errorf("%s: %s; errors %q", test, result.Status, errorPhases(events))
		}
	}
}

// A test's go list in a module of its own, outside the tree, isn't forced into the tree's workspace.
func TestAModuleOutsideTheTreeIsntForcedIntoItsWorkspace(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	fixture.withGo(t, "")
	result, events, _ := runUnit(t, fixture.unit("^TestOutsideModule$"), fixture.options(t))
	if result.Status != protocol.StatusPassed {
		t.Fatalf("%s; errors %q\n%s", result.Status, errorPhases(events), testLog(t, result))
	}
}
