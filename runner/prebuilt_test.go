package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/builder"
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
//	a source unpacked straight at its name, or a dead runner's unpacking never swept: TestASourceUnpackedPartwayIsNeverTrusted
//	a source a unit holds removed, or sources kept oldest first: TestSourcesAreBoundedAndAHeldOneStays
//	prepare.sh's environment mode reaching the checkout, or taking an instance with no toolchain:
//	TestPrepareEnvironmentNeedsNoGitAndNoGo

const lowerPackage = protocol.AdamicModule + "/internal/lower"

// fixtureProductKey is the product the fixture's TestProduct reads (testdata/prebuilt/internal/lower).
const fixtureProductKey = "abababababababababababababababababababababababababababababababab"

// fixtureBinary is the fixture package's test binary, compiled once per test process: the test needs Go, the runner
// it drives never does.
var fixtureBinary struct {
	once    sync.Once
	content []byte
	err     error
}

func prebuiltBinary(t *testing.T) []byte {
	t.Helper()
	fixtureBinary.once.Do(func() {
		directory, err := os.MkdirTemp("", "loom-prebuilt-")
		if err != nil {
			fixtureBinary.err = err
			return
		}
		defer os.RemoveAll(directory)
		binary := filepath.Join(directory, "lower.test")
		command := exec.Command("go", "test", "-c", "-o", binary, "./internal/lower")
		command.Dir = filepath.Join("testdata", "prebuilt")
		command.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if output, err := command.CombinedOutput(); err != nil {
			fixtureBinary.err = fmt.Errorf("go test -c: %v: %s", err, output)
			return
		}
		fixtureBinary.content, fixtureBinary.err = os.ReadFile(binary)
	})
	if fixtureBinary.err != nil {
		t.Fatalf("compiling the fixture: %v", fixtureBinary.err)
	}
	return fixtureBinary.content
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
	key    string
	index  builder.TreeIndex
	binary string
	source string
	store  *prebuiltStore
}

func newPrebuiltTree(t *testing.T, store *prebuiltStore) *prebuiltTree {
	t.Helper()
	files := []tarEntry{}
	for _, name := range []string{"go.mod", "internal/lower/lower_test.go", "internal/lower/testdata/fixture.txt"} {
		content, err := os.ReadFile(filepath.Join("testdata", "prebuilt", filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, tarEntry{name: name, kind: tar.TypeReg, content: string(content)})
	}
	tree := &prebuiltTree{store: store}
	tree.source = store.putBlob(makeTar(t, files, true))
	tree.binary = store.putBlob(gzipped(t, prebuiltBinary(t)))
	product := store.putBlob(makeTar(t, []tarEntry{
		{name: fixtureProductKey + ".inputs", kind: tar.TypeReg, content: "{}\n"},
		{name: fixtureProductKey + "/tool", kind: tar.TypeReg, content: "the product\n", mode: 0o755},
	}, true))
	tree.index = builder.TreeIndex{Tree: strings.Repeat("7", 40), Go: "go1.27.1", Source: tree.source,
		Products: map[string]string{fixtureProductKey: product},
		Packages: map[string]builder.TreePackage{lowerPackage: {Package: lowerPackage, Directory: "internal/lower", Binary: tree.binary, Products: []string{fixtureProductKey}}}}
	tree.key = builder.TreeKey(tree.index.Tree, tree.index.Go, builder.GateEnvironment())
	tree.publish(t)
	return tree
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
	fixture.bin = filepath.Join(fixture.directory, "bin")
	os.MkdirAll(fixture.bin, 0o755)
	if err := os.Symlink(bash, filepath.Join(fixture.bin, "bash")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fixture.bin)
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
	if !strings.Contains(runner, "the tests' PATH holds no go") || strings.Contains(runner, "a test ran go") {
		t.Errorf("the record doesn't say the runner had no go:\n%s", runner)
	}
	if _, err := os.Stat(filepath.Join(fixture.directory, "checkout")); err == nil {
		t.Error("a prebuilt unit prepared a checkout")
	}
	// Every fetch is on the record, with its bytes and seconds.
	for _, blob := range []string{fixture.tree.source, fixture.tree.binary, fixture.tree.index.Products[fixtureProductKey]} {
		if !strings.Contains(runner, "blob "+blob+", ") || !strings.Contains(runner, " from the store") {
			t.Errorf("no fetch of %s on the record:\n%s", blob, runner)
		}
	}
	if !strings.Contains(runner, "fetched trees/"+fixture.tree.key+".json, ") || !strings.Contains(runner, "ready in ") {
		t.Errorf("the index's fetch and the time to be ready aren't on the record:\n%s", runner)
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
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "a test tried to build") {
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
		"the source": {func(t *testing.T, tree *prebuiltTree) { tree.store.remove("blobs/" + tree.source) }, "the tree's source, blob "},
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
		}, "didn't build on Workshop: go test -c: undefined: x"},
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
	if err := os.WriteFile(filepath.Join(fixture.blobs(), fixture.tree.binary), honest[:len(honest)/2], 0o644); err != nil {
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
		if entries, _ = os.ReadDir(other.directory); len(entries) > 0 {
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
// blob is fetched again, a dead partial goes before the next unit, and a live one, another runner's fetch, stays.
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
	os.Chtimes(dead, old, old)
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
// git nor go on its PATH, and refuses an instance without adamic's toolchain as the instance's (exit 2).
func TestPrepareEnvironmentNeedsNoGitAndNoGo(t *testing.T) {
	directory := t.TempDir()
	bin, home, tree := filepath.Join(directory, "bin"), filepath.Join(directory, "home"), filepath.Join(directory, "source")
	for _, path := range []string{bin, home, tree} {
		os.MkdirAll(path, 0o755)
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
}

// A source refused partway (a poisoned archive, a full disk, a runner killed) leaves nothing at the source's name, so
// the next unit of the tree unpacks it again, and what a dead runner's unpacking left is swept, never used.
func TestASourceUnpackedPartwayIsNeverTrusted(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	files := []tarEntry{}
	for _, name := range []string{"go.mod", "internal/lower/lower_test.go", "internal/lower/testdata/fixture.txt"} {
		content, _ := os.ReadFile(filepath.Join("testdata", "prebuilt", filepath.FromSlash(name)))
		files = append(files, tarEntry{name: name, kind: tar.TypeReg, content: string(content)})
	}
	files = append(files, tarEntry{name: "../outside", kind: tar.TypeReg, content: "x"})
	fixture.tree.index.Source = fixture.store.putBlob(makeTar(t, files, true))
	fixture.tree.publish(t)
	sources := filepath.Join(fixture.directory, "root", sourceDirectoryName)
	dead := exec.Command(filepath.Join(fixture.bin, "bash"), "-c", "exit 0")
	dead.Run()
	leftover := filepath.Join(sources, unpackingPrefix+fixture.tree.index.Source+"-"+fmt.Sprint(dead.Process.Pid)+"-x")
	os.MkdirAll(leftover, 0o755)
	for range 2 {
		result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
		if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "unpacking the tree's source") {
			t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
		}
		if _, err := os.Stat(filepath.Join(sources, fixture.tree.index.Source)); err == nil {
			t.Fatal("a source refused partway is at its name")
		}
	}
	entries, _ := os.ReadDir(sources)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), unpackingPrefix) {
			t.Errorf("%s is left", entry.Name())
		}
	}
}

// A runner keeps keepSources trees' sources, least recently used going first, and never one a unit holds.
func TestSourcesAreBoundedAndAHeldOneStays(t *testing.T) {
	cache := newSourceCache(t.TempDir())
	held := []*heldSource{}
	for index := range 3 {
		archive := makeTar(t, []tarEntry{{name: "file", kind: tar.TypeReg, content: fmt.Sprint(index)}}, true)
		source, err := cache.hold(hashOf(archive))
		if err != nil {
			t.Fatal(err)
		}
		if err = cache.unpack(source, bytes.NewReader(archive)); err != nil || !source.ready {
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
		if _, err := os.Stat(filepath.Join(source.directory, "file")); err == nil {
			present += fmt.Sprint(index)
		}
	}
	// 0 is the oldest but held; 2 is the newest; 1 goes.
	if present != "02" {
		t.Fatalf("sources %q are left, not the held 0 and the newest 2", present)
	}
	again, err := cache.hold(held[2].sum)
	if err != nil || !again.ready {
		t.Errorf("a kept source isn't found again: %v", err)
	}
	again.release()
	held[0].release()
}
