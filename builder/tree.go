package builder

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
// A tree key T names the build (planner.TreeKey, of planner.ReadTreeIdentity): sha256 of "loom-tree-v2", the tree
// hash, the Go version, the platform the binaries were built for (GOOS/GOARCH: a Linux binary is no use to a Mac
// runner) and the gate env. Its index is trees/<T>.json in the store (store.go): each package's test binary, gzipped,
// as a blob; the buildcache products its tests read, each its own archive under refs/action/<buildcache key>, so a
// product no tree changed is built and goes up once and later trees fetch it (held.go); and the tree's source archive,
// one blob every package shares. A runner reads the index, then only its own package's binary, products and the
// source, each by sha256.

// A package's build failure is the change's (ChangeFailure: go test -c exited normally with its diagnostics, a compile
// or vet error, which go test reports as the package's red) or Workshop's (WorkshopFailure: anything else, a kill, a
// signal, a full disk, memory, the network). A runner reports the change's as the package's red, with its output, and
// Workshop's as Loom's, broken. An index that names neither for a failed package is read as Workshop's.
const (
	ChangeFailure   = "change"
	WorkshopFailure = "workshop"
)

// diagnosticLine is a line of a compile, vet or package-loading failure as go 1.27's go test -c prints it (captured
// from real go in TestABuildFailureIsTheChangesOnlyWhenGoSaysWhy): a package's header (# <package>); a file:line:col:
// message diagnostic (a compile or vet error, a missing import); an import cycle's "package <path>" and its
// tab-indented "imports" chain, or any diagnostic's tab-indented continuation; "package <path>: build constraints
// exclude all Go files in <dir>"; "no Go files in <dir>"; and go's own ending, "FAIL\t<package> [setup failed]" and a
// bare "FAIL".
var diagnosticLine = regexp.MustCompile(`^(# \S.*|\S+:[0-9]+:[0-9]+: \S.*|\t.*|package \S+|package \S+: build constraints exclude all Go files in \S.*|no Go files in \S.*|FAIL\t\S+ \[setup failed\]|FAIL)$`)

// goEnding is a line go adds after a failure's diagnostics, which says nothing of its own.
var goEnding = regexp.MustCompile(`^(# \S.*|\t.*|FAIL\t\S+ \[setup failed\]|FAIL)$`)

// BuildFailure says whose a failed go test -c is, from its error and output: the change's only when go exited 1 and
// said nothing but package headers and diagnostics, whatever their words; anything else (a proxy's 502, a killed
// compiler or clang, memory, a full disk) is Workshop's.
func BuildFailure(err error, output []byte) string {
	var exit *exec.ExitError
	if !errors.As(err, &exit) || !exit.Exited() || exit.ExitCode() != 1 {
		return WorkshopFailure
	}
	diagnostics := 0
	for _, line := range strings.Split(strings.TrimRight(string(output), "\n"), "\n") {
		if !diagnosticLine.MatchString(line) {
			return WorkshopFailure
		}
		if !goEnding.MatchString(line) {
			diagnostics++
		}
	}
	if diagnostics == 0 {
		return WorkshopFailure
	}
	return ChangeFailure
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
	// Failure says whose Error is: ChangeFailure or WorkshopFailure (empty: Workshop's).
	Failure string `json:"failure,omitempty"`
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
	// Held is a HeldProducts server's address: the store's products, offered to buildcache before it builds one.
	Held string
	// Watched are the filesystems the build writes, by name (the cache base, Go's build cache, the temporary
	// directory), each with the free bytes it keeps: no job starts while one is under its floor (#ckv0pmg). None
	// watches nothing. Free reads a filesystem (nil means Free).
	Watched map[string]Watch
	Free    func(path string) (uint64, error)
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

// share is each later go process's share of compile: compile/Jobs, and never under 2 (or compile, when that is
// less), so no product build compiles alone at -p 1.
func (build TreeBuild) share() int {
	return max(min(2, build.compile()), build.compile()/max(1, build.Jobs))
}

// perJob is share as go's -p takes it.
func (build TreeBuild) perJob() string {
	return strconv.Itoa(build.share())
}

// jobs is how many later go processes run at once: Jobs, but never so many that their shares add up to more than
// compile (--jobs 32 at the floor of 2 would otherwise compile 64 at once on a limit of 60). Warm's build of every
// main package, not a larger share, is what keeps a product build from compiling a chain alone.
func (build TreeBuild) jobs() int {
	return min(max(1, build.Jobs), max(1, build.compile()/build.share()))
}

// ProductBuildFlags are the flags adamic's buildcache.GoBuild gives every Go product build (its reproducible(), in
// internal/buildcache/gobuild.go). Warm uses them so its builds are the products' own; measured on go1.27, none of
// them changes a package's compile in the build cache (the compiler always trims paths, and -ldflags and -buildvcs
// reach only the link), so what a product build was missing was the packages no test compiles: a main package's
// closure outside every test's (cohere's command/cohere, for the grain formatter), and plain builds of packages the
// tests compile only with their test files.
var ProductBuildFlags = []string{"-trimpath", "-ldflags=-buildid=", "-buildvcs=false"}

// mainPackages lists every main package of the tree's module and of the modules it replaces (cohere and its
// submodules, for adamic), which a product's go build builds: GoBuild's products are programs.
func (build TreeBuild) mainPackages() ([]string, error) {
	run := func(arguments ...string) ([]string, error) {
		command := exec.Command("go", arguments...)
		command.Dir = build.Tree
		command.Env = build.environment()
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("go %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
		}
		return strings.Fields(string(output)), nil
	}
	replaced, err := run("list", "-m", "-f", "{{if .Replace}}{{.Path}}{{end}}", "all")
	if err != nil {
		return nil, err
	}
	patterns := []string{"./..."}
	for _, module := range replaced {
		patterns = append(patterns, module+"/...")
	}
	listed, err := run(append([]string{"list", "-e", "-f", "{{if eq .Name \"main\"}}{{.ImportPath}}{{end}}"}, patterns...)...)
	if err != nil {
		return nil, err
	}
	seen, mains := map[string]bool{}, []string{}
	for _, pkg := range listed {
		if !seen[pkg] {
			seen[pkg] = true
			mains = append(mains, pkg)
		}
	}
	sort.Strings(mains)
	return mains, nil
}

// Warm compiles every package's tests once, in one go process, compile at a time, running none: each dependency
// compiles once for the whole tree, and the products and binaries after it start from a warm build cache instead
// of each compiling the tree's dependencies again at once. Then it compiles every main package with
// ProductBuildFlags, as a product's go build will, so a product test's own build compiles nothing either.
func (build TreeBuild) Warm(packages []planner.ProductTest) error {
	if err := build.warmTests(packages); err != nil {
		return err
	}
	return build.warmProducts()
}

// warmProducts compiles every main package with ProductBuildFlags, compile at a time, linking nothing.
func (build TreeBuild) warmProducts() error {
	mains, err := build.mainPackages()
	if err != nil || len(mains) == 0 {
		return err
	}
	arguments := append([]string{"build", "-p", strconv.Itoa(build.compile())}, ProductBuildFlags...)
	if len(mains) == 1 {
		// One main package would be linked into the tree; several are only compiled.
		arguments = append(arguments, "-o", os.DevNull)
	}
	arguments = append(arguments, mains...)
	command := exec.Command("go", arguments...)
	command.Dir = build.Tree
	command.Env = build.environment()
	if output, err := command.CombinedOutput(); err != nil {
		tail := output
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		return fmt.Errorf("go build %s: %v\n%s", strings.Join(ProductBuildFlags, " "), err, tail)
	}
	return nil
}

func (build TreeBuild) warmTests(packages []planner.ProductTest) error {
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

// environment is a go process's environment. With Held set, buildcache asks it for a product before building one,
// audits none of what it is given (a native product isn't reproducible yet, #tsn1wp8, so an audit's rebuild never
// matches), and holds no write credential, so it never publishes anywhere itself.
func (build TreeBuild) environment(extra ...string) []string {
	store := []string{"ADAMIC_BUILD_STORE=off"}
	if build.Held != "" {
		store = []string{"ADAMIC_BUILD_STORE=" + build.Held, "ADAMIC_BUILD_AUDIT=0", "ADAMIC_BUILD_STORE_TOKEN=" + os.DevNull}
	}
	return append(append(append(append(os.Environ(), build.Environment...), "ADAMIC_BUILD_CACHE_DIR="+build.Cache, "ADAMIC_BUILD_CACHE=on"), store...), extra...)
}

// Binaries compiles every package's test binary into Out, cold for this tree, Jobs at a time. Its blob is named when
// the tree is published.
func (build TreeBuild) Binaries(packages []planner.ProductTest) []TreePackage {
	results := make([]TreePackage, len(packages))
	refuse := func(index int, err error) {
		results[index] = TreePackage{Package: packages[index].Package, Directory: packages[index].Directory, Products: []string{}, Error: "not started: " + err.Error(), Failure: WorkshopFailure}
	}
	admitted(len(packages), build.jobs(), build.gauge(), build.busy(), build.disk(), func(index int) {
		test := packages[index]
		started := time.Now()
		result := TreePackage{Package: test.Package, Directory: test.Directory, Products: []string{}}
		binary := filepath.Join(build.Out, strings.ReplaceAll(test.Package, "/", "_")+".test")
		command := exec.Command("go", "test", "-c", "-p", build.perJob(), "-o", binary, "./"+filepath.ToSlash(filepath.Clean(test.Directory)))
		command.Dir = build.Tree
		command.Env = build.shared()
		if output, err := command.CombinedOutput(); err != nil {
			// The change's diagnostics are what its owner reads, so they keep more than a machine's failure.
			tail := output
			if len(tail) > 16000 {
				tail = tail[len(tail)-16000:]
			}
			result.Error = fmt.Sprintf("go test -c: %v\n%s", err, tail)
			result.Failure = BuildFailure(err, output)
		} else if info, err := os.Stat(binary); err != nil {
			result.Error, result.Failure = err.Error(), WorkshopFailure
		} else {
			result.Bytes = info.Size()
		}
		result.Seconds = time.Since(started).Seconds()
		results[index] = result
	}, refuse)
	return results
}

// Products runs every product test of the tree into its cache, Jobs at a time, and names the buildcache products
// each package's product tests used. A product test that fails is that package's error.
func (build TreeBuild) Products(tests []planner.ProductTest, logs string) (map[string][]string, map[string]string) {
	used := map[string]map[string]bool{}
	failed := map[string]string{}
	var mutex sync.Mutex
	refuse := func(index int, err error) {
		mutex.Lock()
		defer mutex.Unlock()
		failed[tests[index].Package] += fmt.Sprintf("%s: not started: %v\n", tests[index].Test, err)
	}
	admitted(len(tests), build.jobs(), build.gauge(), build.busy(), build.disk(), func(index int) {
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
	}, refuse)
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
	Tree   string `json:"tree"`
	Future string `json:"future"`
	Go     string `json:"go"`
	// Goos and Goarch are the platform the binaries were built for, go env GOOS and GOARCH on Workshop.
	Goos   string `json:"goos"`
	Goarch string `json:"goarch"`
	Source string `json:"source"`
	// Modules is the blob of the tree's module download cache (ModuleCacheArchive): the only place a runner's go
	// reads a module from. Empty: none published.
	Modules  string                 `json:"modules,omitempty"`
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

// admissionPoll is how long admitted waits before reading the disk again while it is under the floor.
var admissionPoll = time.Second

// admitted runs work for each index, at most jobs at once, and while gauge reads the machine at or over target busy,
// or under a fifth of its memory available, starts no more (Kirk, Oct 10: "target using like 80% of it"). It starts
// at most one job per reading, so it ramps instead of lunging, and always lets one run, so other load on the machine
// slows a build and never stops it. A nil gauge, or one that can't read, admits every job up to jobs.
//
// The disk is the third reading, and the one that never yields (#ckv0pmg): while disk says a filesystem the build
// writes is under its floor, no job starts at all. It waits for running jobs to finish (one may be what is filling
// the disk, and finishing may free it), and when none is left and the disk is still short, each job not yet started is
// refused with disk's reason, so a build fails loudly rather than filling the disk or waiting forever. nil reads none.
func admitted(count, jobs int, gauge Gauge, target float64, disk func() error, work func(index int), refuse func(index int, err error)) {
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
		short := error(nil)
		for disk != nil {
			if short = disk(); short == nil || running.Load() == 0 {
				break
			}
			time.Sleep(admissionPoll)
		}
		if short != nil {
			refuse(index, short)
			continue
		}
		running.Add(1)
		next <- index
	}
	close(next)
	group.Wait()
}

// disk is the build's disk reading for admitted: an error naming a watched filesystem under its floor, or nil.
func (build TreeBuild) disk() func() error {
	if len(build.Watched) == 0 {
		return nil
	}
	return func() error {
		return CheckFloor(build.Watched, build.Free)
	}
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
// up. A product in held (HeldProducts.Held) came from the store this build, its blob already fresh, so it is named
// in the index and nothing more. It fills in treeIndex's blobs as it goes, and reports whether it wrote the index
// (writeIndex keeps one with fewer failed packages).
func PublishTree(store Store, treeIndex *TreeIndex, binaries, cache string, source []byte, held map[string]string) (string, bool, error) {
	treeKey := planner.TreeKey(treeIndex.Tree, treeIndex.Go, treeIndex.Goos, treeIndex.Goarch)
	var err error
	if treeIndex.Source, err = store.PutBlob(source); err != nil {
		return "", false, fmt.Errorf("the source archive: %w", err)
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
		if sum, fetched := held[products[index]]; fetched {
			sums[index] = sum
			return nil
		}
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
		return "", false, err
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
				built.Failure = WorkshopFailure
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
		return "", false, err
	}
	for index, name := range names {
		treeIndex.Packages[name] = packages[index]
	}
	written, err := store.writeIndex(treeKey, treeIndex)
	if err != nil {
		return "", false, fmt.Errorf("the tree's index: %w", err)
	}
	return treeKey, written, nil
}

// failures counts an index's packages that didn't build.
func (index TreeIndex) failures() int {
	count := 0
	for _, built := range index.Packages {
		if built.Error != "" {
			count++
		}
	}
	return count
}

// TreeIndexed says whether the bucket holds trees/<treeKey>.json, read from the bucket itself, never an edge's cache:
// what Workshop's tree builder builds when it doesn't, and what the placer waits for before naming the build on a unit.
func (store Store) TreeIndexed(treeKey string) (bool, error) {
	if !productKeyPattern.MatchString(treeKey) {
		return false, fmt.Errorf("%q isn't a tree key, 64 lowercase hex digits", treeKey)
	}
	store.read()
	_, err := store.Bucket.Head("trees/" + treeKey + ".json")
	if errors.Is(err, r2.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// writeIndex writes trees/<treeKey>.json, once every blob and ref it names is up, and reports whether it did. An
// index the bucket already holds is replaced only by one with no more failed packages, and only over the very object
// read (If-Match on its ETag; If-None-Match: * when there is none), so a worse build never takes a better one's place
// and two builds racing are each held to what the other wrote. An index kept that way is kept fresh, it and every blob
// it names (keep), unless a blob it names is gone, when it can't be kept and the worse but whole one replaces it.
func (store Store) writeIndex(treeKey string, treeIndex *TreeIndex) (bool, error) {
	encoded, err := treeIndex.encode()
	if err != nil {
		return false, err
	}
	key := "trees/" + treeKey + ".json"
	for range 3 {
		store.read()
		content, object, err := store.Bucket.GetObject(key)
		options := r2.PutOptions{ContentType: "application/json", CacheControl: "no-cache"}
		switch {
		case errors.Is(err, r2.ErrNotFound):
			options.IfNoneMatch = true
		case err != nil:
			return false, err
		default:
			var held TreeIndex
			if json.Unmarshal(content, &held) == nil && treeIndex.failures() > held.failures() {
				kept, err := store.keep(key, held, content, object)
				if errors.Is(err, r2.ErrChanged) {
					continue
				}
				if err != nil || kept {
					return false, err
				}
			}
			options.IfMatch = object.ETag
		}
		store.wrote()
		err = store.Bucket.Put(key, encoded, options)
		if !errors.Is(err, r2.ErrExists) && !errors.Is(err, r2.ErrChanged) {
			return err == nil, err
		}
	}
	return false, fmt.Errorf("%s kept changing while it was written", key)
}

// blobs are every blob the index names: the source archive, each product's archive, each built package's binary.
func (index TreeIndex) blobs() []string {
	named := map[string]bool{index.Source: true, index.Modules: true}
	for _, sum := range index.Products {
		named[sum] = true
	}
	for _, built := range index.Packages {
		if built.Error == "" {
			named[built.Binary] = true
		}
	}
	sums := []string{}
	for sum := range named {
		if sum != "" {
			sums = append(sums, sum)
		}
	}
	sort.Strings(sums)
	return sums
}

// keep keeps a held index runnable when a worse build declines to replace it, rather than letting what it names
// expire under its runners near day 7: every blob it names that was uploaded more than FreshFor ago is read and put
// again, its own bytes, and so is the index itself, over the ETag read (r2.ErrChanged when another build wrote it
// meanwhile, for the caller to read it again). Refreshing beats calling an old index replaceable: that would hand
// runners the worse build, failed packages and all, when the better one only needed its blobs kept. An index naming a
// blob the store no longer holds can't be kept, and keep reports false, so the worse but whole index takes its place.
func (store Store) keep(key string, held TreeIndex, content []byte, object r2.Object) (bool, error) {
	for _, sum := range held.blobs() {
		store.read()
		blob, err := store.Bucket.Head("blobs/" + sum)
		if errors.Is(err, r2.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if store.stale(blob.Modified) {
			if _, err = store.heldBlob(sum); errors.Is(err, ErrNotStored) {
				return false, nil
			} else if err != nil {
				return false, err
			}
		}
	}
	if store.stale(object.Modified) {
		store.wrote()
		if err := store.Bucket.Put(key, content, r2.PutOptions{ContentType: "application/json", CacheControl: "no-cache", IfMatch: object.ETag}); err != nil {
			return false, err
		}
	}
	return true, nil
}

// Tree reads trees/<treeKey>.json from the public domain, as ParseTree reads it.
func (store Store) Tree(treeKey string) (TreeIndex, error) {
	content, err := store.get("trees/" + treeKey + ".json")
	if err != nil {
		return TreeIndex{}, err
	}
	return ParseTree(treeKey, content)
}

// ParseTree reads the index trees/<treeKey>.json holds, refusing one that names anything but a sha256 for a blob or a
// buildcache key for a product, a package that reads a product the index doesn't name, or a built package whose
// directory isn't a local path, where its tests would run outside the tree's source.
func ParseTree(treeKey string, content []byte) (TreeIndex, error) {
	var index TreeIndex
	if err := json.Unmarshal(content, &index); err != nil {
		return TreeIndex{}, fmt.Errorf("tree %s: %w", treeKey, err)
	}
	poisoned := func(format string, arguments ...any) (TreeIndex, error) {
		return TreeIndex{}, fmt.Errorf("tree %s: %s: the store is poisoned", treeKey, fmt.Sprintf(format, arguments...))
	}
	if !productKeyPattern.MatchString(index.Source) {
		return poisoned("its source archive is %q", index.Source)
	}
	if index.Modules != "" && !productKeyPattern.MatchString(index.Modules) {
		return poisoned("its module cache is %q", index.Modules)
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
		if built.Directory != "" && !filepath.IsLocal(filepath.FromSlash(built.Directory)) {
			return poisoned("package %s's directory is %q", name, built.Directory)
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
	if err = Unpack(bytes.NewReader(blob), filepath.Join(scratch, "source"), nil); err != nil {
		return TreePackage{}, fmt.Errorf("the source archive: %w: the store is poisoned", err)
	}
	for _, product := range built.Products {
		if blob, err = store.blob(index.Products[product]); err != nil {
			return TreePackage{}, fmt.Errorf("product %s: %w", product, err)
		}
		if err = Unpack(bytes.NewReader(blob), filepath.Join(scratch, "cache"), ProductEntries(product)); err != nil {
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
