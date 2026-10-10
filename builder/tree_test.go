package builder

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
