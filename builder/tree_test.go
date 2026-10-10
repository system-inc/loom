package builder

import (
	"archive/tar"
	"bytes"
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
		if result.Error != "" || result.Binary == "" || result.Bytes == 0 {
			t.Fatalf("%+v", result)
		}
	}
	products, failed := build.Products([]planner.ProductTest{{Package: "example.com/tree/a", Directory: "a", Test: "TestProduct_Tool"}}, t.TempDir())
	if len(failed) != 0 || len(products["example.com/tree/a"]) != 1 || products["example.com/tree/a"][0] != product {
		t.Fatalf("products %v, failed %v", products, failed)
	}
	source := filepath.Join(work, "source.tar")
	if err = SourceArchive(tree, source); err != nil {
		t.Fatal(err)
	}
	again := filepath.Join(work, "again.tar")
	SourceArchive(tree, again)
	first, _ := os.ReadFile(source)
	second, _ := os.ReadFile(again)
	if !bytes.Equal(first, second) {
		t.Fatal("two archives of one tree differ")
	}

	index := TreeIndex{Tree: "0123456789abcdef0123456789abcdef01234567", Future: "f", Go: "go1.27.1", Packages: map[string]TreePackage{}}
	for _, result := range built {
		result.Products = products[result.Package]
		if result.Products == nil {
			result.Products = []string{}
		}
		index.Packages[result.Package] = result
	}
	store := newFakeStore()
	served := serve(t, store, "workshop")
	if _, err = PublishTree(served, nil, index, build.Out, build.Cache, source); err != nil {
		t.Fatal(err)
	}

	// A runner: the index, then package a's action into a fresh unit directory, and the binary run from the source.
	runner := Store{Read: served.Read}
	treeKey := TreeKey(index.Tree, index.Go, GateEnvironment())
	indexDirectory := t.TempDir()
	if err = runner.FetchAction(treeKey, indexDirectory); err != nil {
		t.Fatal(err)
	}
	var read TreeIndex
	content, _ := os.ReadFile(filepath.Join(indexDirectory, "index.json"))
	if err = json.Unmarshal(content, &read); err != nil || read.Packages["example.com/tree/a"].Key != PackageKey(treeKey, "example.com/tree/a") {
		t.Fatalf("index %s %v", content, err)
	}
	unit := t.TempDir()
	if err = runner.FetchAction(read.Packages["example.com/tree/a"].Key, unit); err != nil {
		t.Fatal(err)
	}
	if err = untar(filepath.Join(unit, "source.tar"), filepath.Join(unit, "source")); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(unit, "test"), "-test.run", "^TestUsesTheProductAndTestdata$", "-test.v")
	command.Dir = filepath.Join(unit, "source", "a")
	command.Env = append(os.Environ(), "ADAMIC_BUILD_CACHE_DIR="+filepath.Join(unit, "cache"), "PATH=/nonexistent")
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "--- PASS: TestUsesTheProductAndTestdata") {
		t.Fatalf("the fetched binary: %v\n%s", err, output)
	}
}

func untar(archive, directory string) error {
	content, err := os.ReadFile(archive)
	if err != nil {
		return err
	}
	reader := tar.NewReader(bytes.NewReader(content))
	for {
		header, err := reader.Next()
		if err != nil {
			if err.Error() == "EOF" {
				return nil
			}
			return err
		}
		path := filepath.Join(directory, filepath.FromSlash(header.Name))
		os.MkdirAll(filepath.Dir(path), 0o755)
		if header.Typeflag == tar.TypeReg {
			body := make([]byte, header.Size)
			if _, err := reader.Read(body); err != nil && header.Size > 0 && err.Error() != "EOF" {
				return err
			}
			os.WriteFile(path, body, os.FileMode(header.Mode))
		}
	}
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
	if build.perJob() != "1" || (TreeBuild{Compile: 60, Jobs: 8}).perJob() != "7" || (TreeBuild{Compile: 4, Jobs: 8}).perJob() != "1" {
		t.Fatal("each job's share of the compile limit")
	}
}

// most runs count jobs of work, each taking a few milliseconds, under gauge, and returns how many ran at once at most.
func most(t *testing.T, gauge Gauge) int64 {
	t.Helper()
	var now, peak atomic.Int64
	admitted(12, 4, gauge, 0.8, func(int) {
		value := now.Add(1)
		for {
			old := peak.Load()
			if value <= old || peak.CompareAndSwap(old, value) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		now.Add(-1)
	})
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
