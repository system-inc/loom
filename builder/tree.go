package builder

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/system-inc/loom/planner"
)

// Kirk's shape (Oct 10 03:0xZ): Workshop builds everything for one future's tree, cold and per tree, and pushes it to
// the store; a Codex runner only downloads by hash and runs, with 30 s to be ready and 60 s to run. A tree's build is
// every test package's binary (go test -c, with the unit's env and flags), the products its tests use (built by its
// product tests into the tree's own buildcache), and the tree's source (tests read testdata), all in the action store.
//
// A tree key T names the build: sha256 of "loom-tree-v1", the tree hash, the Go version and the gate env. Each package
// P is one action, refs/action/<PackageKey(T, P)>: `test` (the binary), `source.tar` (the tree's files, one blob every
// package shares) and `cache/<buildcache key>/...` with `cache/<key>.inputs` for each product P's tests use. The tree's
// index is refs/action/T, one output `index.json` naming each package's key and binary.

// TreeKey is the key of one tree's build.
func TreeKey(treeHash, goVersion string, environment []string) string {
	sum := sha256.Sum256([]byte("loom-tree-v1\n" + treeHash + "\n" + goVersion + "\n" + strings.Join(environment, "\n")))
	return hex.EncodeToString(sum[:])
}

// PackageKey is the key of one package's action within a tree's build.
func PackageKey(treeKey, importPath string) string {
	sum := sha256.Sum256([]byte("loom-tree-v1\n" + treeKey + "\n" + importPath))
	return hex.EncodeToString(sum[:])
}

// A TreePackage is one test package of a tree and what its build made.
type TreePackage struct {
	Package   string   `json:"package"`
	Directory string   `json:"directory"`
	Key       string   `json:"key"`
	Binary    string   `json:"binary"` // sha256 of its test binary
	Bytes     int64    `json:"bytes"`
	Products  []string `json:"products"` // buildcache keys its product tests used
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
}

func (build TreeBuild) environment(extra ...string) []string {
	return append(append(append(os.Environ(), build.Environment...), "ADAMIC_BUILD_CACHE_DIR="+build.Cache, "ADAMIC_BUILD_STORE=off", "ADAMIC_BUILD_CACHE=on"), extra...)
}

// Binaries compiles every package's test binary into Out, cold for this tree, Jobs at a time.
func (build TreeBuild) Binaries(packages []planner.ProductTest) []TreePackage {
	results := make([]TreePackage, len(packages))
	parallel(len(packages), build.Jobs, func(index int) {
		test := packages[index]
		started := time.Now()
		result := TreePackage{Package: test.Package, Directory: test.Directory, Products: []string{}}
		binary := filepath.Join(build.Out, strings.ReplaceAll(test.Package, "/", "_")+".test")
		command := exec.Command("go", "test", "-c", "-o", binary, "./"+filepath.ToSlash(filepath.Clean(test.Directory)))
		command.Dir = build.Tree
		command.Env = build.environment()
		if output, err := command.CombinedOutput(); err != nil {
			tail := output
			if len(tail) > 2000 {
				tail = tail[len(tail)-2000:]
			}
			result.Error = fmt.Sprintf("go test -c: %v\n%s", err, tail)
		} else if content, err := os.ReadFile(binary); err != nil {
			result.Error = err.Error()
		} else {
			sum := sha256.Sum256(content)
			result.Binary, result.Bytes = hex.EncodeToString(sum[:]), int64(len(content))
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
	parallel(len(tests), build.Jobs, func(index int) {
		test := tests[index]
		log := filepath.Join(logs, fmt.Sprintf("product-%d.log", index))
		command := exec.Command("go", "test", "-count=1", "-run", "^"+test.Test+"$", "./"+filepath.ToSlash(filepath.Clean(test.Directory)))
		command.Dir = build.Tree
		command.Env = build.environment("ADAMIC_BUILD_LOG=" + log)
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

// SourceArchive writes the tree's tracked files, submodules included (git ls-files --recurse-submodules), as one tar
// whose bytes depend only on the files: names sorted, no times or owners, mode 0644 or 0755 by the executable bit. A
// tracked symbolic link goes in as a link.
func SourceArchive(tree, path string) error {
	command := exec.Command("git", "ls-files", "--recurse-submodules", "-z")
	command.Dir = tree
	listing, err := command.Output()
	if err != nil {
		return fmt.Errorf("git ls-files in %s: %w", tree, err)
	}
	names := []string{}
	for _, name := range strings.Split(strings.TrimRight(string(listing), "\x00"), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	writer := tar.NewWriter(file)
	for _, name := range names {
		full := filepath.Join(tree, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		header := &tar.Header{Name: name, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return err
			}
			header.Typeflag, header.Linkname, header.Mode = tar.TypeSymlink, target, 0o777
		case info.Mode().IsRegular():
			header.Typeflag, header.Size, header.Mode = tar.TypeReg, info.Size(), 0o644
			if info.Mode()&0o111 != 0 {
				header.Mode = 0o755
			}
		default:
			continue
		}
		if err = writer.WriteHeader(header); err != nil {
			return err
		}
		if header.Typeflag == tar.TypeReg {
			content, err := os.ReadFile(full)
			if err != nil {
				return err
			}
			if _, err = writer.Write(content); err != nil {
				return err
			}
		}
	}
	if err = writer.Close(); err != nil {
		return err
	}
	return file.Close()
}

// A TreeIndex is index.json, the tree's own action: each package's key and binary.
type TreeIndex struct {
	Tree     string                 `json:"tree"`
	Future   string                 `json:"future"`
	Go       string                 `json:"go"`
	Source   string                 `json:"source"`
	Seconds  float64                `json:"seconds"`
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
func parallel(count, jobs int, work func(index int)) {
	next := make(chan int)
	var group sync.WaitGroup
	for range max(1, jobs) {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range next {
				work(index)
			}
		}()
	}
	for index := range count {
		next <- index
	}
	close(next)
	group.Wait()
}

// PublishTree uploads a tree's build: each package that built, as its own action (its binary, the shared source
// archive and its products under cache/), then the tree's index. A package the index already holds goes up no
// second time. It returns the index's manifest sha256.
func PublishTree(store Store, index *Index, treeIndex TreeIndex, binaries, cache, source string) (string, error) {
	treeKey := TreeKey(treeIndex.Tree, treeIndex.Go, GateEnvironment())
	sourceContent, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	sourceSum := sha256.Sum256(sourceContent)
	treeIndex.Source = hex.EncodeToString(sourceSum[:])
	sourceOutput := Output{Bytes: int64(len(sourceContent)), Path: "source.tar", Sha256: treeIndex.Source}
	for name, built := range treeIndex.Packages {
		if built.Error != "" {
			continue
		}
		built.Key = PackageKey(treeKey, built.Package)
		treeIndex.Packages[name] = built
		if index != nil {
			if _, held := index.Ref(built.Key); held {
				continue
			}
		}
		binary := filepath.Join(binaries, strings.ReplaceAll(built.Package, "/", "_")+".test")
		outputs := []Output{{Bytes: built.Bytes, Executable: true, Path: "test", Sha256: built.Binary}, sourceOutput}
		files := map[string]string{"test": binary, "source.tar": source}
		products, productFiles, err := OutputsOf(cache, built.Products)
		if err != nil {
			return "", err
		}
		for _, product := range products {
			files["cache/"+product.Path] = productFiles[product.Path]
			product.Path = "cache/" + product.Path
			outputs = append(outputs, product)
		}
		sort.Slice(outputs, func(left, right int) bool { return outputs[left].Path < outputs[right].Path })
		if _, err = store.UploadWith(built.Key, outputs, files, index); err != nil {
			return "", fmt.Errorf("package %s: %w", built.Package, err)
		}
	}
	encoded, err := treeIndex.encode()
	if err != nil {
		return "", err
	}
	indexFile := filepath.Join(binaries, "index.json")
	if err = os.WriteFile(indexFile, encoded, 0o644); err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return store.UploadWith(treeKey, []Output{{Bytes: int64(len(encoded)), Path: "index.json", Sha256: hex.EncodeToString(sum[:])}}, map[string]string{"index.json": indexFile}, index)
}
