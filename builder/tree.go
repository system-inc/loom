package builder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/r2"
)

// Kirk's shape (Oct 10 03:0xZ): Workshop builds everything for one future's tree, cold and per tree, and pushes it to
// the store; a Codex runner only downloads by hash and runs, with 30 s to be ready and 60 s to run. A tree's build is
// every test package's binary (go test -c, with the unit's env and flags), the products its tests use (built by its
// product tests into the tree's own buildcache), and the tree's source (tests read testdata), all in the action store.
//
// A tree key T names the build: sha256 of "loom-tree-v1", the tree hash, the Go version and the gate env. Its index is
// trees/<T>.json in the store (store.go): each package's test binary, gzipped, as a blob; the buildcache products its
// tests read, each its own archive under refs/action/<buildcache key>, so a product no tree changed goes up once; and
// the tree's source archive, one blob every package shares. A runner reads the index, then only its own package's
// binary, products and the source, each by sha256.

// TreeKey is the key of one tree's build.
func TreeKey(treeHash, goVersion string, environment []string) string {
	sum := sha256.Sum256([]byte("loom-tree-v1\n" + treeHash + "\n" + goVersion + "\n" + strings.Join(environment, "\n")))
	return hex.EncodeToString(sum[:])
}

// A TreePackage is one test package of a tree and what its build made.
type TreePackage struct {
	Package   string   `json:"package"`
	Directory string   `json:"directory"`
	Binary    string   `json:"binary,omitempty"` // the blob of its test binary, gzipped
	Bytes     int64    `json:"bytes"`            // the test binary's own size
	Products  []string `json:"products"`         // buildcache keys its tests read
	Seconds   float64  `json:"seconds"`
	Error     string   `json:"error,omitempty"`
}

// TestPackages lists every package of the tree with tests, for this platform, compiling nothing.
func TestPackages(tree string) ([]planner.ProductTest, error) {
	goMod, err := os.ReadFile(filepath.Join(tree, "go.mod"))
	if err != nil {
		return nil, err
	}
	module := ""
	for _, line := range strings.Split(string(goMod), "\n") {
		if name, found := strings.CutPrefix(strings.TrimSpace(line), "module "); found {
			module = strings.Trim(strings.TrimSpace(name), `"`)
			break
		}
	}
	command := exec.Command("go", "list", "-f", "{{.ImportPath}} {{len .TestGoFiles}} {{len .XTestGoFiles}}", "./...")
	command.Dir = tree
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go list ./...: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	packages := []planner.ProductTest{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || (fields[1] == "0" && fields[2] == "0") {
			continue
		}
		directory := strings.TrimPrefix(strings.TrimPrefix(fields[0], module), "/")
		packages = append(packages, planner.ProductTest{Package: fields[0], Directory: directory})
	}
	sort.Slice(packages, func(left, right int) bool { return packages[left].Package < packages[right].Package })
	return packages, nil
}

// A TreeBuild builds one tree: Environment is the unit's (the gate's switches), Cache the tree's own buildcache,
// Out where the binaries go.
type TreeBuild struct {
	Tree        string
	Cache       string
	Out         string
	Environment []string
	Jobs        int
	// Compile is how many packages Warm compiles at once in its one go process, and the most the later phases'
	// go processes compile between them (Workshop, Oct 10: 32 jobs of go test each compiling 64 at once ran 459
	// compilers on 64 threads, filled 110G of 125G, and starved sshd). Zero means every thread but four.
	Compile int
	// Busy is the fraction of the machine a build keeps busy, starting no job above it (zero means 0.8), and Gauge
	// reads it (nil means ProcGauge).
	Busy  float64
	Gauge Gauge
}

func (build TreeBuild) busy() float64 {
	if build.Busy > 0 {
		return build.Busy
	}
	return 0.8
}

func (build TreeBuild) gauge() Gauge {
	if build.Gauge != nil {
		return build.Gauge
	}
	return ProcGauge
}

// shared is the environment for a later phase's go processes: GOFLAGS carries each one's share of the compile limit
// into the go builds its tests run themselves (a product test's go build -buildmode=c-archive, Oct 10), which
// otherwise compile as many packages at once as the machine has threads.
func (build TreeBuild) shared(extra ...string) []string {
	flags := strings.TrimSpace(os.Getenv("GOFLAGS") + " -p=" + build.perJob())
	return build.environment(append([]string{"GOFLAGS=" + flags}, extra...)...)
}

// compile is Compile, or every thread but four, so the machine can always answer.
func (build TreeBuild) compile() int {
	if build.Compile > 0 {
		return build.Compile
	}
	return max(1, runtime.NumCPU()-4)
}

// perJob is each later go process's share of compile, so Jobs of them never compile more than compile at once.
func (build TreeBuild) perJob() string {
	return strconv.Itoa(max(1, build.compile()/max(1, build.Jobs)))
}

// Warm compiles every package's tests once, in one go process, compile at a time, running none: each dependency
// compiles once for the whole tree, and the products and binaries after it start from a warm build cache instead
// of each compiling the tree's dependencies again at once.
func (build TreeBuild) Warm(packages []planner.ProductTest) error {
	if len(packages) == 0 {
		return nil
	}
	nothing, err := exec.LookPath("true")
	if err != nil {
		return err
	}
	arguments := []string{"test", "-count=1", "-run", "^$", "-exec", nothing, "-p", strconv.Itoa(build.compile())}
	for _, test := range packages {
		arguments = append(arguments, "./"+filepath.ToSlash(filepath.Clean(test.Directory)))
	}
	command := exec.Command("go", arguments...)
	command.Dir = build.Tree
	command.Env = build.environment()
	if output, err := command.CombinedOutput(); err != nil {
		tail := output
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		return fmt.Errorf("go test -exec true: %v\n%s", err, tail)
	}
	return nil
}

func (build TreeBuild) environment(extra ...string) []string {
	return append(append(append(os.Environ(), build.Environment...), "ADAMIC_BUILD_CACHE_DIR="+build.Cache, "ADAMIC_BUILD_STORE=off", "ADAMIC_BUILD_CACHE=on"), extra...)
}

// Binaries compiles every package's test binary into Out, cold for this tree, Jobs at a time. Its blob is named when
// the tree is published.
func (build TreeBuild) Binaries(packages []planner.ProductTest) []TreePackage {
	results := make([]TreePackage, len(packages))
	admitted(len(packages), build.Jobs, build.gauge(), build.busy(), func(index int) {
		test := packages[index]
		started := time.Now()
		result := TreePackage{Package: test.Package, Directory: test.Directory, Products: []string{}}
		binary := filepath.Join(build.Out, strings.ReplaceAll(test.Package, "/", "_")+".test")
		command := exec.Command("go", "test", "-c", "-p", build.perJob(), "-o", binary, "./"+filepath.ToSlash(filepath.Clean(test.Directory)))
		command.Dir = build.Tree
		command.Env = build.shared()
		if output, err := command.CombinedOutput(); err != nil {
			tail := output
			if len(tail) > 2000 {
				tail = tail[len(tail)-2000:]
			}
			result.Error = fmt.Sprintf("go test -c: %v\n%s", err, tail)
		} else if info, err := os.Stat(binary); err != nil {
			result.Error = err.Error()
		} else {
			result.Bytes = info.Size()
		}
		result.Seconds = time.Since(started).Seconds()
		results[index] = result
	})
	return results
}

// Products runs every product test of the tree into its cache, Jobs at a time, and names the buildcache products
// each package's product tests used. A product test that fails is that package's error.
func (build TreeBuild) Products(tests []planner.ProductTest, logs string) (map[string][]string, map[string]string) {
	used := map[string]map[string]bool{}
	failed := map[string]string{}
	var mutex sync.Mutex
	admitted(len(tests), build.Jobs, build.gauge(), build.busy(), func(index int) {
		test := tests[index]
		log := filepath.Join(logs, fmt.Sprintf("product-%d.log", index))
		command := exec.Command("go", "test", "-count=1", "-p", build.perJob(), "-run", "^"+test.Test+"$", "./"+filepath.ToSlash(filepath.Clean(test.Directory)))
		command.Dir = build.Tree
		command.Env = build.shared("ADAMIC_BUILD_LOG=" + log)
		output, err := command.CombinedOutput()
		products, touchedErr := Touched(log, build.Cache)
		mutex.Lock()
		defer mutex.Unlock()
		if err != nil || touchedErr != nil {
			tail := output
			if len(tail) > 2000 {
				tail = tail[len(tail)-2000:]
			}
			failed[test.Package] += fmt.Sprintf("%s: %v %v\n%s\n", test.Test, err, touchedErr, tail)
			return
		}
		if used[test.Package] == nil {
			used[test.Package] = map[string]bool{}
		}
		for _, product := range products {
			used[test.Package][product] = true
		}
	})
	byPackage := map[string][]string{}
	for pkg, set := range used {
		for product := range set {
			byPackage[pkg] = append(byPackage[pkg], product)
		}
		sort.Strings(byPackage[pkg])
	}
	return byPackage, failed
}

// A TreeIndex is trees/<treeKey>.json, a tree's build: the source archive's blob, each product's archive by its key,
// and each package with its binary's blob and the products its tests read.
type TreeIndex struct {
	Tree     string                 `json:"tree"`
	Future   string                 `json:"future"`
	Go       string                 `json:"go"`
	Source   string                 `json:"source"`
	Seconds  float64                `json:"seconds"`
	Products map[string]string      `json:"products"`
	Packages map[string]TreePackage `json:"packages"`
}

func (index TreeIndex) encode() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	err := encoder.Encode(index)
	return buffer.Bytes(), err
}

// parallel runs work for 0..count-1, jobs at a time.
// A Gauge reads the machine: the fraction of its CPU busy and of its memory available, ok false when it can't.
type Gauge func() (busy, available float64, ok bool)

// admitted runs work for each index, at most jobs at once, and while gauge reads the machine at or over target busy,
// or under a fifth of its memory available, starts no more (Kirk, Oct 10: "target using like 80% of it"). It starts
// at most one job per reading, so it ramps instead of lunging, and always lets one run, so other load on the machine
// slows a build and never stops it. A nil gauge, or one that can't read, admits every job up to jobs.
func admitted(count, jobs int, gauge Gauge, target float64, work func(index int)) {
	next := make(chan int)
	var group sync.WaitGroup
	var running atomic.Int64
	for range max(1, jobs) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range next {
				work(index)
				running.Add(-1)
			}
		}()
	}
	for index := range count {
		for gauge != nil && running.Load() > 0 {
			busy, available, ok := gauge()
			if !ok || (busy < target && available >= 0.2) {
				break
			}
		}
		running.Add(1)
		next <- index
	}
	close(next)
	group.Wait()
}

// ProcGauge reads Linux's /proc/stat twice, half a second apart, for the CPU busy between, and /proc/meminfo for the
// memory available. Off Linux it can't read, so a build there admits every job up to its ceiling.
func ProcGauge() (busy, available float64, ok bool) {
	first, ok := cpuTimes()
	if !ok {
		return 0, 0, false
	}
	time.Sleep(500 * time.Millisecond)
	second, ok := cpuTimes()
	if !ok {
		return 0, 0, false
	}
	total, idle := second[0]-first[0], second[1]-first[1]
	if total <= 0 {
		return 0, 0, false
	}
	content, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var memoryTotal, memoryAvailable float64
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		value, _ := strconv.ParseFloat(fields[1], 64)
		switch fields[0] {
		case "MemTotal:":
			memoryTotal = value
		case "MemAvailable:":
			memoryAvailable = value
		}
	}
	if memoryTotal <= 0 {
		return 0, 0, false
	}
	return 1 - idle/total, memoryAvailable / memoryTotal, true
}

// cpuTimes is /proc/stat's first line as {total, idle plus iowait}, in clock ticks.
func cpuTimes() ([2]float64, bool) {
	content, err := os.ReadFile("/proc/stat")
	if err != nil {
		return [2]float64{}, false
	}
	line, _, _ := strings.Cut(string(content), "\n")
	fields := strings.Fields(line)
	if len(fields) < 6 || fields[0] != "cpu" {
		return [2]float64{}, false
	}
	var times [2]float64
	for index, field := range fields[1:] {
		value, err := strconv.ParseFloat(field, 64)
		if err != nil {
			return [2]float64{}, false
		}
		times[0] += value
		if index == 3 || index == 4 {
			times[1] += value
		}
	}
	return times, true
}

// publishJobs is how many products, or binaries, a tree sends at once: each is a few round trips to R2.
const publishJobs = 8

// each runs work for 0..count-1, jobs at a time, and returns every error it met.
func each(count, jobs int, work func(index int) error) error {
	errs := make([]error, count)
	next := make(chan int)
	var group sync.WaitGroup
	for range max(1, jobs) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range next {
				errs[index] = work(index)
			}
		}()
	}
	for index := range count {
		next <- index
	}
	close(next)
	group.Wait()
	return errors.Join(errs...)
}

// PublishTree uploads a tree's build and returns its tree key: the source archive, then each product a built
// package's tests read, once each, as its own archive under its buildcache key, then each built package's binary,
// gzipped, and the index last, so no index names what the store lacks. A product whose ref names another archive (a
// ConflictError) fails every package that reads it, named in each one's error, and the rest of the tree still goes
// up. It fills in treeIndex's blobs as it goes.
func PublishTree(store Store, treeIndex *TreeIndex, binaries, cache string, source []byte) (string, error) {
	treeKey := TreeKey(treeIndex.Tree, treeIndex.Go, GateEnvironment())
	var err error
	if treeIndex.Source, err = store.PutBlob(source); err != nil {
		return "", fmt.Errorf("the source archive: %w", err)
	}
	names := make([]string, 0, len(treeIndex.Packages))
	read := map[string]bool{}
	for name, built := range treeIndex.Packages {
		names = append(names, name)
		if built.Error == "" {
			for _, product := range built.Products {
				read[product] = true
			}
		}
	}
	sort.Strings(names)
	products := make([]string, 0, len(read))
	for product := range read {
		products = append(products, product)
	}
	sort.Strings(products)
	sums, conflicts := make([]string, len(products)), make([]error, len(products))
	err = each(len(products), publishJobs, func(index int) error {
		archive, _, err := ProductArchive(cache, []string{products[index]})
		if err == nil {
			sums[index], err = store.Publish(products[index], archive)
		}
		if errors.As(err, &ConflictError{}) {
			conflicts[index], err = err, nil
		}
		if err != nil {
			return fmt.Errorf("product %s: %w", products[index], err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	treeIndex.Products = map[string]string{}
	conflicted := map[string]error{}
	for index, product := range products {
		if conflicts[index] != nil {
			conflicted[product] = conflicts[index]
		} else {
			treeIndex.Products[product] = sums[index]
		}
	}
	packages := make([]TreePackage, len(names))
	err = each(len(names), publishJobs, func(index int) error {
		built := treeIndex.Packages[names[index]]
		packages[index] = built
		if built.Error != "" {
			return nil
		}
		for _, product := range built.Products {
			if conflict, broke := conflicted[product]; broke {
				built.Error += conflict.Error() + "\n"
			}
		}
		if built.Error == "" {
			content, err := os.ReadFile(filepath.Join(binaries, strings.ReplaceAll(built.Package, "/", "_")+".test"))
			if err != nil {
				return fmt.Errorf("package %s: %w", built.Package, err)
			}
			blob, err := gzipped(content)
			if err != nil {
				return err
			}
			if built.Binary, err = store.PutBlob(blob); err != nil {
				return fmt.Errorf("package %s: %w", built.Package, err)
			}
		}
		packages[index] = built
		return nil
	})
	if err != nil {
		return "", err
	}
	for index, name := range names {
		treeIndex.Packages[name] = packages[index]
	}
	encoded, err := treeIndex.encode()
	if err != nil {
		return "", err
	}
	store.wrote()
	if err = store.Bucket.Put("trees/"+treeKey+".json", encoded, r2.PutOptions{ContentType: "application/json", CacheControl: "no-cache"}); err != nil {
		return "", fmt.Errorf("the tree's index: %w", err)
	}
	return treeKey, nil
}

// Tree reads trees/<treeKey>.json from the public domain, refusing an index that names anything but a sha256 for a
// blob or a buildcache key for a product, or a package that reads a product the index doesn't name.
func (store Store) Tree(treeKey string) (TreeIndex, error) {
	content, err := store.get("trees/" + treeKey + ".json")
	if err != nil {
		return TreeIndex{}, err
	}
	var index TreeIndex
	if err = json.Unmarshal(content, &index); err != nil {
		return TreeIndex{}, fmt.Errorf("tree %s: %w", treeKey, err)
	}
	poisoned := func(format string, arguments ...any) (TreeIndex, error) {
		return TreeIndex{}, fmt.Errorf("tree %s: %s: the store is poisoned", treeKey, fmt.Sprintf(format, arguments...))
	}
	if !productKeyPattern.MatchString(index.Source) {
		return poisoned("its source archive is %q", index.Source)
	}
	for product, sum := range index.Products {
		if !productKeyPattern.MatchString(product) || !productKeyPattern.MatchString(sum) {
			return poisoned("product %q is %q", product, sum)
		}
	}
	for name, built := range index.Packages {
		if built.Error != "" {
			continue
		}
		if !productKeyPattern.MatchString(built.Binary) {
			return poisoned("package %s's binary is %q", name, built.Binary)
		}
		for _, product := range built.Products {
			if _, named := index.Products[product]; !named {
				return poisoned("package %s reads product %q, which the index doesn't name", name, product)
			}
		}
	}
	return index, nil
}

// FetchPackage readies one package of a tree's build under directory, which must not hold it yet: test, its binary;
// source/, the tree's files; and cache/, a buildcache directory (ADAMIC_BUILD_CACHE_DIR) holding each product its
// tests read. It reads the tree's index, then only those blobs, each checked against its hash (and kept in Blobs
// when set), unpacked into a scratch directory, refusing any entry that would land outside it, and a product's entry
// that isn't that product's. Only when all of it checks does any of it move into place; a product already in cache/
// is left as it is.
func (store Store) FetchPackage(treeKey, importPath, directory string) (TreePackage, error) {
	index, err := store.Tree(treeKey)
	if err != nil {
		return TreePackage{}, err
	}
	built, found := index.Packages[importPath]
	if !found {
		return TreePackage{}, fmt.Errorf("tree %s has no package %s", treeKey, importPath)
	}
	if built.Error != "" {
		return TreePackage{}, fmt.Errorf("package %s didn't build in tree %s: %s", importPath, treeKey, built.Error)
	}
	for _, name := range []string{"test", "source"} {
		if _, err := os.Lstat(filepath.Join(directory, name)); err == nil {
			return TreePackage{}, fmt.Errorf("%s already holds %s", directory, name)
		}
	}
	if err = os.MkdirAll(filepath.Join(directory, "cache"), 0o755); err != nil {
		return TreePackage{}, err
	}
	scratch, err := os.MkdirTemp(directory, ".fetching-")
	if err != nil {
		return TreePackage{}, err
	}
	defer os.RemoveAll(scratch)
	blob, err := store.blob(built.Binary)
	if err != nil {
		return TreePackage{}, fmt.Errorf("package %s's binary: %w", importPath, err)
	}
	binary, err := gunzipped(blob)
	if err != nil {
		return TreePackage{}, fmt.Errorf("package %s's binary: %w: the store is poisoned", importPath, err)
	}
	if err = os.WriteFile(filepath.Join(scratch, "test"), binary, 0o755); err != nil {
		return TreePackage{}, err
	}
	if blob, err = store.blob(index.Source); err != nil {
		return TreePackage{}, fmt.Errorf("the source archive: %w", err)
	}
	if err = Unpack(blob, filepath.Join(scratch, "source"), nil); err != nil {
		return TreePackage{}, fmt.Errorf("the source archive: %w: the store is poisoned", err)
	}
	for _, product := range built.Products {
		if blob, err = store.blob(index.Products[product]); err != nil {
			return TreePackage{}, fmt.Errorf("product %s: %w", product, err)
		}
		own := func(name string) bool {
			return name == product+".inputs" || (strings.HasPrefix(name, product+"/") && len(name) > len(product)+1)
		}
		if err = Unpack(blob, filepath.Join(scratch, "cache"), own); err != nil {
			return TreePackage{}, fmt.Errorf("product %s: %w: the store is poisoned", product, err)
		}
	}
	if err = os.MkdirAll(filepath.Join(scratch, "cache"), 0o755); err != nil {
		return TreePackage{}, err
	}
	if err = (Store{}).place(filepath.Join(scratch, "cache"), filepath.Join(directory, "cache")); err != nil {
		return TreePackage{}, err
	}
	for _, name := range []string{"test", "source"} {
		if err = os.Rename(filepath.Join(scratch, name), filepath.Join(directory, name)); err != nil {
			return TreePackage{}, err
		}
	}
	return built, nil
}
