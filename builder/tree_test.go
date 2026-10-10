package builder

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/r2/r2test"
)

func TestATreeBuildsItsBinariesAndProductsOnceAndARunnerNeedsNoCompiler(t *testing.T) {
	product := strings.Repeat("c", 64)
	tree := gitTree(t, map[string]string{
		"go.mod": "module example.com/tree\n\ngo 1.22\n",
		"a/a.go": "package a\n\nfunc Answer() int { return 42 }\n",
		"a/a_test.go": `package a

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProduct_Tool(t *testing.T) {
	directory := filepath.Join(os.Getenv("ADAMIC_BUILD_CACHE_DIR"), "` + product + `")
	os.MkdirAll(directory, 0o755)
	os.WriteFile(filepath.Join(directory, "tool"), []byte("built once"), 0o755)
	os.WriteFile(os.Getenv("ADAMIC_BUILD_LOG"), []byte("build tool ` + product[:12] + ` miss 0.10\n"), 0o644)
}

func TestUsesTheProductAndTestdata(t *testing.T) {
	if _, err := os.ReadFile("testdata/case.txt"); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(filepath.Join(os.Getenv("ADAMIC_BUILD_CACHE_DIR"), "` + product + `", "tool")); err != nil || string(content) != "built once" {
		t.Fatalf("the product: %q %v", content, err)
	}
	if Answer() != 42 {
		t.Fatal("answer")
	}
}
`,
		"a/testdata/case.txt": "case\n",
		"b/b_test.go":         "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n",
		"c/c.go":              "package c\n",
	})
	work := t.TempDir()
	build := TreeBuild{Tree: tree, Cache: filepath.Join(work, "cache"), Out: filepath.Join(work, "out"), Environment: GateEnvironment(), Jobs: 2}
	os.MkdirAll(build.Out, 0o755)
	packages, err := TestPackages(tree)
	if err != nil || len(packages) != 2 {
		t.Fatalf("test packages %+v %v", packages, err)
	}
	if err = build.Warm(packages); err != nil {
		t.Fatal(err)
	}
	built := build.Binaries(packages)
	for _, result := range built {
		if result.Error != "" || result.Bytes == 0 {
			t.Fatalf("%+v", result)
		}
	}
	products, failed := build.Products([]planner.ProductTest{{Package: "example.com/tree/a", Directory: "a", Test: "TestProduct_Tool"}}, t.TempDir())
	if len(failed) != 0 || len(products["example.com/tree/a"]) != 1 || products["example.com/tree/a"][0] != product {
		t.Fatalf("products %v, failed %v", products, failed)
	}
	source, err := SourceArchive(tree)
	if err != nil {
		t.Fatal(err)
	}

	index := TreeIndex{Tree: "0123456789abcdef0123456789abcdef01234567", Future: "f", Go: "go1.27.1", Packages: map[string]TreePackage{}}
	for _, result := range built {
		result.Products = products[result.Package]
		if result.Products == nil {
			result.Products = []string{}
		}
		index.Packages[result.Package] = result
	}
	fake, store := serve(t)
	treeKey, written, err := PublishTree(store, &index, build.Out, build.Cache, source, nil)
	if err != nil || !written || treeKey != TreeKey(index.Tree, index.Go, GateEnvironment()) {
		t.Fatal(treeKey, err)
	}
	// The layout: the tree's index, one ref (the product), and four blobs (the source, the product, two binaries).
	if trees, refs, blobs := fake.Keys("trees/"), fake.Keys("refs/"), fake.Keys("blobs/"); !slices.Equal(trees, []string{"trees/" + treeKey + ".json"}) ||
		!slices.Equal(refs, []string{"refs/action/" + product}) || len(blobs) != 4 {
		t.Fatalf("the store holds %v %v %v", trees, refs, blobs)
	}
	var stored TreeIndex
	content, _ := fake.Object("trees/" + treeKey + ".json")
	if err = json.Unmarshal(content, &stored); err != nil || stored.Source != digest(source) || stored.Products[product] == "" || stored.Packages["example.com/tree/a"].Binary == "" {
		t.Fatalf("the index %s %v", content, err)
	}
	if ref, _ := fake.Object("refs/action/" + product); string(ref) != stored.Products[product] {
		t.Fatalf("the ref holds %q, the index %q", ref, stored.Products[product])
	}

	// A runner: package a into a fresh unit directory, then the binary run from the source with no compiler.
	blobs := t.TempDir()
	runner := Store{Read: fake.Public(), Blobs: blobs}
	unit := t.TempDir()
	fetched, err := runner.FetchPackage(treeKey, "example.com/tree/a", unit)
	if err != nil || !slices.Equal(fetched.Products, []string{product}) {
		t.Fatal(fetched, err)
	}
	command := exec.Command(filepath.Join(unit, "test"), "-test.run", "^TestUsesTheProductAndTestdata$", "-test.v")
	command.Dir = filepath.Join(unit, "source", "a")
	command.Env = append(os.Environ(), "ADAMIC_BUILD_CACHE_DIR="+filepath.Join(unit, "cache"), "PATH=/nonexistent")
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "--- PASS: TestUsesTheProductAndTestdata") {
		t.Fatalf("the fetched binary: %v\n%s", err, output)
	}
	// It read the index and only package a's three blobs; package b's binary stayed where it was.
	if reads := fake.Count("PUBLIC", "blobs/"); reads != 3 || fake.Count("PUBLIC", "blobs/"+stored.Packages["example.com/tree/b"].Binary) != 0 {
		t.Fatalf("the runner read %v", fake.Requests())
	}
	// A second unit of the same tree on the same runner reads its blobs from the local cache.
	if _, err = runner.FetchPackage(treeKey, "example.com/tree/a", t.TempDir()); err != nil || fake.Count("PUBLIC", "blobs/") != 3 {
		t.Fatalf("a second unit: %v %v", err, fake.Requests())
	}
	if _, err = runner.FetchPackage(treeKey, "example.com/tree/a", unit); err == nil {
		t.Fatal("a fetch over a unit that holds the package")
	}

	// The same tree built again sends nothing it holds fresh: no blob, no ref, only the index again.
	fake.ResetRequests()
	again := TreeIndex{Tree: index.Tree, Future: "f", Go: index.Go, Packages: map[string]TreePackage{}}
	for name, result := range index.Packages {
		result.Binary = ""
		again.Packages[name] = result
	}
	if _, written, err = PublishTree(store, &again, build.Out, build.Cache, source, nil); err != nil || !written {
		t.Fatal(err)
	}
	if fake.Count("PUT", "blobs/") != 0 || fake.Count("PUT", "refs/") != 0 || fake.Count("PUT", "trees/") != 1 {
		t.Fatalf("an unchanged tree: %v", fake.Requests())
	}

	// A product whose ref names another archive fails each package that reads it, naming both, and the rest go up.
	other := tarGzip(t, entry{name: product + "/tool", body: "another build's tool"})
	fake.Set("blobs/"+digest(other), other, time.Now())
	fake.Set("refs/action/"+product, []byte(digest(other)), time.Now())
	conflicted := TreeIndex{Tree: index.Tree, Future: "f", Go: index.Go, Packages: map[string]TreePackage{}}
	for name, result := range index.Packages {
		result.Binary = ""
		conflicted.Packages[name] = result
	}
	if _, written, err = PublishTree(store, &conflicted, build.Out, build.Cache, source, nil); err != nil || written {
		t.Fatal(err)
	}
	if broke := conflicted.Packages["example.com/tree/a"].Error; !strings.Contains(broke, digest(other)) || !strings.Contains(broke, stored.Products[product]) {
		t.Fatalf("package a: %q", broke)
	}
	if conflicted.Packages["example.com/tree/b"].Error != "" || conflicted.Packages["example.com/tree/b"].Binary == "" {
		t.Fatalf("package b: %+v", conflicted.Packages["example.com/tree/b"])
	}
	if ref, _ := fake.Object("refs/action/" + product); string(ref) != digest(other) {
		t.Fatalf("the conflicting ref was overwritten with %q", ref)
	}
	// The index a better build wrote stays: package a, which built there, is still what a runner fetches.
	if _, err = (Store{Read: fake.Public()}).FetchPackage(treeKey, "example.com/tree/a", t.TempDir()); err != nil {
		t.Fatalf("the better index was replaced: %v", err)
	}
	// With no index yet, the worse build's is written, and a runner is told package a didn't build; the better build
	// then takes its place, over the ETag it read.
	fake.Delete("trees/" + treeKey + ".json")
	if _, written, err = PublishTree(store, &conflicted, build.Out, build.Cache, source, nil); err != nil || !written {
		t.Fatalf("a first index with a failure: %v %v", written, err)
	}
	if _, err = (Store{Read: fake.Public()}).FetchPackage(treeKey, "example.com/tree/a", t.TempDir()); err == nil || !strings.Contains(err.Error(), "didn't build") {
		t.Fatalf("a runner fetching a package that didn't build: %v", err)
	}
	fake.Set("refs/action/"+product, []byte(stored.Products[product]), time.Now())
	if _, written, err = PublishTree(store, &again, build.Out, build.Cache, source, nil); err != nil || !written {
		t.Fatalf("a better build after a worse one: %v %v", written, err)
	}
	if _, err = (Store{Read: fake.Public()}).FetchPackage(treeKey, "example.com/tree/a", t.TempDir()); err != nil {
		t.Fatalf("the better index didn't take the worse one's place: %v", err)
	}
}

// A runner refuses a tree whose blobs don't check, and a product archive holding anything but that product's files,
// and leaves nothing of it behind.
func TestARunnerRefusesATreeThatDoesntCheck(t *testing.T) {
	product, other := keyOf("product"), keyOf("other")
	binary := mustGzip(t, []byte("a test binary"))
	honest := tarGzip(t, entry{name: product + "/tool", body: "tool"}, entry{name: product + ".inputs", body: "name tool"})
	// plant puts a one-package tree in a fresh bucket, its product's archive the one given.
	plant := func(t *testing.T, archive []byte) (*r2test.Fake, Store) {
		fake, _ := serve(t)
		source := tarGzip(t, entry{name: "p/case.txt", body: "case"})
		index := TreeIndex{Source: digest(source), Products: map[string]string{product: digest(archive)},
			Packages: map[string]TreePackage{"p": {Package: "p", Directory: "p", Binary: digest(binary), Products: []string{product}}}}
		encoded, _ := index.encode()
		for _, blob := range [][]byte{binary, archive, source} {
			fake.Set("blobs/"+digest(blob), blob, time.Now())
		}
		fake.Set("trees/t.json", encoded, time.Now())
		return fake, Store{Read: fake.Public()}
	}
	refused := func(t *testing.T, runner Store) {
		t.Helper()
		unit := t.TempDir()
		if _, err := runner.FetchPackage("t", "p", unit); err == nil || !strings.Contains(err.Error(), "poisoned") {
			t.Fatalf("%v", err)
		}
		if entries, _ := os.ReadDir(filepath.Join(unit, "cache")); len(entries) != 0 {
			t.Fatalf("a refused fetch left %s", entries[0].Name())
		}
		for _, name := range []string{"test", "source"} {
			if _, err := os.Lstat(filepath.Join(unit, name)); err == nil {
				t.Fatalf("a refused fetch left %s", name)
			}
		}
	}
	_, runner := plant(t, honest)
	if _, err := runner.FetchPackage("t", "p", t.TempDir()); err != nil {
		t.Fatalf("an honest tree: %v", err)
	}
	for name, archive := range map[string][]byte{
		"another product's files":    tarGzip(t, entry{name: other + "/tool", body: "tool"}),
		"a file outside the product": tarGzip(t, entry{name: product + "/../escaped", body: "x"}),
		"an archive that isn't gzip": []byte("not an archive"),
	} {
		t.Run(name, func(t *testing.T) {
			_, runner := plant(t, archive)
			refused(t, runner)
		})
	}
	t.Run("a binary that doesn't hash to its name", func(t *testing.T) {
		fake, runner := plant(t, honest)
		fake.Set("blobs/"+digest(binary), mustGzip(t, []byte("another binary")), time.Now())
		refused(t, runner)
	})
	t.Run("an index naming something that isn't a sha256", func(t *testing.T) {
		fake, runner := plant(t, honest)
		content, _ := fake.Object("trees/t.json")
		fake.Set("trees/t.json", []byte(strings.Replace(string(content), digest(binary), "latest", 1)), time.Now())
		refused(t, runner)
	})
}

// Warm compiles every package in one go process and fails, naming the package, when one doesn't compile; a
// compile limit of one still finishes.
func TestWarmCompilesEveryPackageOnceAndNamesOneThatDoesNotCompile(t *testing.T) {
	tree := gitTree(t, map[string]string{
		"go.mod":         "module example.com/warm\n\ngo 1.22\n",
		"good/g.go":      "package good\n\nfunc Answer() int { return 42 }\n",
		"good/g_test.go": "package good\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(\"answer\")\n\t}\n}\n",
		"bad/b_test.go":  "package bad\n\nimport \"testing\"\n\nfunc TestBroken(t *testing.T) { undefinedThing() }\n",
	})
	build := TreeBuild{Tree: tree, Cache: t.TempDir(), Environment: GateEnvironment(), Jobs: 1, Compile: 1}
	if err := build.Warm([]planner.ProductTest{{Package: "example.com/warm/good", Directory: "good"}}); err != nil {
		t.Fatalf("a package that compiles: %v", err)
	}
	err := build.Warm([]planner.ProductTest{{Package: "example.com/warm/good", Directory: "good"}, {Package: "example.com/warm/bad", Directory: "bad"}})
	if err == nil || !strings.Contains(err.Error(), "undefinedThing") {
		t.Fatalf("a package that doesn't compile: %v", err)
	}
	// A job's share never drops under 2 (or the whole limit, when that is less), and the jobs running at once never
	// add up to more than the limit: --jobs 32 at the floor would otherwise compile 64 at once on 60 threads.
	for _, share := range []struct {
		build      TreeBuild
		share      string
		concurrent int
	}{{build, "1", 1}, {TreeBuild{Compile: 60, Jobs: 8}, "7", 8}, {TreeBuild{Compile: 4, Jobs: 8}, "2", 2}, {TreeBuild{Compile: 60, Jobs: 32}, "2", 30},
		{TreeBuild{Compile: 60, Jobs: 64}, "2", 30}, {TreeBuild{Compile: 60, Jobs: 2}, "30", 2}, {TreeBuild{Compile: 3, Jobs: 8}, "2", 1}} {
		if got := share.build.perJob(); got != share.share || share.build.jobs() != share.concurrent {
			t.Errorf("compile %d, jobs %d: a share of %s, %d at once; want %s, %d", share.build.Compile, share.build.Jobs, got, share.build.jobs(), share.share, share.concurrent)
		}
	}
	for compile := 1; compile <= 128; compile++ {
		for jobs := 1; jobs <= 128; jobs++ {
			build := TreeBuild{Compile: compile, Jobs: jobs}
			if build.share() < min(2, compile) || build.jobs()*build.share() > compile {
				t.Fatalf("compile %d, jobs %d: %d jobs of %d compile %d at once", compile, jobs, build.jobs(), build.share(), build.jobs()*build.share())
			}
		}
	}
}

// most runs count jobs of work, each taking a few milliseconds, under gauge, and returns how many ran at once at most.
func most(t *testing.T, gauge Gauge) int64 {
	t.Helper()
	var now, peak atomic.Int64
	admitted(12, 4, gauge, 0.8, nil, func(int) {
		value := now.Add(1)
		for {
			old := peak.Load()
			if value <= old || peak.CompareAndSwap(old, value) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		now.Add(-1)
	}, nil)
	return peak.Load()
}

func reading(busy, available float64, ok bool) Gauge {
	return func() (float64, float64, bool) {
		time.Sleep(time.Millisecond)
		return busy, available, ok
	}
}

// A build starts no job while the machine reads at or over its target, or short of memory, and always lets one run.
func TestABuildKeepsTheMachineUnderItsTargetAndAlwaysMovesOneJob(t *testing.T) {
	if got := most(t, reading(0.1, 0.9, true)); got != 4 {
		t.Fatalf("a calm machine ran %d at once, not its ceiling of 4", got)
	}
	if got := most(t, reading(0.9, 0.9, true)); got != 1 {
		t.Fatalf("a machine over its target ran %d at once, not 1", got)
	}
	if got := most(t, reading(0.1, 0.1, true)); got != 1 {
		t.Fatalf("a machine short of memory ran %d at once, not 1", got)
	}
	if got := most(t, reading(0.9, 0.1, false)); got != 4 {
		t.Fatalf("a gauge that can't read ran %d at once, not the ceiling of 4", got)
	}
	if got := most(t, nil); got != 4 {
		t.Fatalf("no gauge ran %d at once, not the ceiling of 4", got)
	}
}

// ProcGauge reads Linux's own counters as fractions; off Linux it says it can't.
func TestProcGaugeReadsTheMachine(t *testing.T) {
	busy, available, ok := ProcGauge()
	if runtime.GOOS != "linux" {
		if ok {
			t.Fatal("read /proc off Linux")
		}
		return
	}
	if !ok || busy < 0 || busy > 1 || available <= 0 || available > 1 {
		t.Fatalf("busy %v, available %v, ok %v", busy, available, ok)
	}
}

// The tests' own go builds get the build's share of the compile limit through GOFLAGS.
func TestATestsOwnGoBuildsGetTheirShareOfTheCompileLimit(t *testing.T) {
	environment := TreeBuild{Compile: 60, Jobs: 8}.shared()
	if !slices.ContainsFunc(environment, func(entry string) bool {
		return strings.HasPrefix(entry, "GOFLAGS=") && strings.HasSuffix(entry, "-p=7")
	}) {
		t.Fatal("no GOFLAGS -p=7 in a later phase's environment")
	}
}

// adamicGoBuildFlags are the flags adamic's buildcache.GoBuild puts on every product build (reproducible(), in
// internal/buildcache/gobuild.go), written out here so the test holds Warm to adamic, not to itself.
var adamicGoBuildFlags = []string{"-trimpath", "-ldflags=-buildid=", "-buildvcs=false"}

// compiles is what a product's go build compiles after a warm: it runs go build -x with buildcache.GoBuild's flags
// and returns the packages it compiled (each compile line's -p).
func compiles(t *testing.T, build TreeBuild, pkg string) []string {
	t.Helper()
	arguments := append(append([]string{"build", "-x"}, adamicGoBuildFlags...), "-o", filepath.Join(t.TempDir(), "product"), pkg)
	command := exec.Command("go", arguments...)
	command.Dir = build.Tree
	command.Env = build.shared()
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}
	compiled := []string{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || filepath.Base(fields[0]) != "compile" {
			continue
		}
		for index, field := range fields {
			if field == "-p" && index+1 < len(fields) {
				compiled = append(compiled, fields[index+1])
			}
		}
	}
	return compiled
}

// After Warm, a product's own go build, with the flags adamic's buildcache gives it, compiles nothing: Warm compiled
// every main package's closure with those flags too. Warming the tests alone leaves it the whole chain to compile,
// the straggler of Oct 10.
func TestAProductBuildAfterWarmCompilesNothing(t *testing.T) {
	tree := gitTree(t, map[string]string{
		"go.mod":           "module example.com/warmed\n\ngo 1.22\n",
		"lib/lib.go":       "package lib\n\nfunc Answer() int { return 42 }\n",
		"lib/lib_test.go":  "package lib\n\nimport \"testing\"\n\nfunc TestAnswer(t *testing.T) {\n\tif Answer() != 42 {\n\t\tt.Fatal(\"answer\")\n\t}\n}\n",
		"cmd/tool/main.go": "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/warmed/lib\"\n)\n\nfunc main() { fmt.Println(lib.Answer()) }\n",
	})
	packages := []planner.ProductTest{{Package: "example.com/warmed/lib", Directory: "lib"}}
	warmed := func(full bool) TreeBuild {
		build := TreeBuild{Tree: tree, Cache: t.TempDir(), Environment: append(GateEnvironment(), "GOCACHE="+t.TempDir()), Jobs: 1}
		var err error
		if full {
			err = build.Warm(packages)
		} else {
			err = build.warmTests(packages)
		}
		if err != nil {
			t.Fatal(err)
		}
		return build
	}
	if compiled := compiles(t, warmed(false), "./cmd/tool"); !slices.Contains(compiled, "example.com/warmed/lib") {
		t.Fatalf("after warming only the tests, the product build compiled %v; the test can't tell a warm cache from a cold one", compiled)
	}
	build := warmed(true)
	if compiled := compiles(t, build, "./cmd/tool"); len(compiled) != 0 {
		t.Fatalf("after Warm, the product build compiled %v", compiled)
	}
	if status, _ := exec.Command("git", "-C", tree, "status", "--porcelain").Output(); len(status) != 0 {
		t.Fatalf("Warm left files in the tree: %s", status)
	}
}
