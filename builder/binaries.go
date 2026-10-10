package builder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/system-inc/loom/planner"
)

// A package's test binary is an action, as a product is (#s0cqqhk lever 3): its binary key names everything go test -c
// reads to make it, and refs/action/<binary key> in the store names the blob of the binary it made, gzipped. A tree
// asks the store for each package's key before compiling, and compiles only the packages whose key the store lacks; a
// held binary is named in the index by its stored blob, as a compiled one is, so a runner reads no difference. A branch
// that changed one package's file compiles that package and the packages whose closures hold it, and no other.
//
// A key that leaves out something a binary depends on serves a stale binary, so the key is the package's closure
// (planner.Closure: every source file it and its tests compile, the files they embed, and the module files go builds
// them under, the same hash a unit key holds) and the rest of what go test -c reads, BinaryInputs. go test -c isn't
// reproducible across directories: with no -trimpath (Workshop's GOFLAGS sets none) it writes the tree's directory, and
// GOROOT's and the module cache's, into the binary, so a key holds those too. Built in the same directory, the same
// key makes the same bytes (TestAStoredBinaryIsTheBytesACompileMakes); the tree builder checks every tree out into its
// one clone (build-trees' --clone), so in production every tree shares its directory.

// binaryKeyVersion is the binary key's prefix: a change to its shape is a new version, so no old key matches.
const binaryKeyVersion = "loom-binary-v1"

// testBinaryFlags are go test's flags that reach a test binary's bytes: Binaries builds with exactly these, and every
// binary key holds them, so a flag added to the build moves every key. -p and -o say only how many packages compile
// at once and where the binary is written, and reach no byte of it.
var testBinaryFlags = []string{"-c"}

// binaryGoEnvironment are go env's variables that change what go test -c makes of the same sources on the same
// release and platform: cgo's compilers and flags (the standard library's net and os/user are cgo when CGO_ENABLED is
// 1, so every binary linking them reads them), GOFLAGS (a -tags or -trimpath there reaches every package), the
// amd64 level and experiments, the FIPS module, and GOROOT and the module cache, whose paths go writes into a binary
// built without -trimpath.
var binaryGoEnvironment = []string{"AR", "CC", "CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_ENABLED", "CGO_FFLAGS", "CGO_LDFLAGS",
	"CXX", "FC", "GOAMD64", "GOEXPERIMENT", "GOFIPS140", "GOFLAGS", "GOMODCACHE", "GOROOT", "PKG_CONFIG"}

// BinaryInputs are what a test binary depends on beyond its package's closure, the same for every package of one
// build: the Go release and the platform (the standard library is the release's, which is why the closure leaves it
// out), the gate environment the build runs under, go test's flags, go env's variables that reach a binary
// (binaryGoEnvironment), the C compiler's own account of itself (CC --version: cgo's objects and the external link
// are its), and the tree's directory, which go writes into every binary built without -trimpath. A test binary embeds
// or links no product: products are built into the buildcache when a test runs, and the files a package embeds are
// in its closure.
type BinaryInputs struct {
	Go            string            `json:"go"`
	Goos          string            `json:"goos"`
	Goarch        string            `json:"goarch"`
	Environment   []string          `json:"environment"`
	Flags         []string          `json:"flags"`
	GoEnvironment map[string]string `json:"goEnvironment"`
	CCompiler     string            `json:"cCompiler"`
	Directory     string            `json:"directory"`
}

// BinaryKey is sha256("loom-binary-v1\n" + canonical({package, closure, inputs})), 64 lowercase hex: the key a test
// binary is stored under. It takes the package's closure as given, so a keyer that already holds the closure (the
// Resident's warm one) need not read the tree again; BinaryKeys reads it cold, with planner.Closure.
func BinaryKey(importPath, closure string, inputs BinaryInputs) (string, error) {
	if importPath == "" || !productKeyPattern.MatchString(closure) {
		return "", fmt.Errorf("a binary key needs a package and its closure's sha256, and has %q and %q", importPath, closure)
	}
	canonical, err := planner.Canonical(struct {
		Package string       `json:"package"`
		Closure string       `json:"closure"`
		Inputs  BinaryInputs `json:"inputs"`
	}{importPath, closure, inputs})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(binaryKeyVersion+"\n"), canonical...))
	return hex.EncodeToString(sum[:]), nil
}

// BinaryInputs reads this build's inputs on its tree, for the release and platform the tree key names: go env asked in
// the tree under the build's own environment (the one Binaries compiles under, less the compile limit, which reaches
// no byte), the C compiler's --version (none, when it doesn't answer: a build with no C compiler links no C), and the
// tree's directory as go sees it, its symbolic links resolved.
func (build TreeBuild) BinaryInputs(goVersion, goos, goarch string) (BinaryInputs, error) {
	command := exec.Command("go", append([]string{"env", "-json"}, binaryGoEnvironment...)...)
	command.Dir = build.Tree
	command.Env = build.environment()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return BinaryInputs{}, fmt.Errorf("go env: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	goEnvironment := map[string]string{}
	if err = json.Unmarshal(output, &goEnvironment); err != nil {
		return BinaryInputs{}, fmt.Errorf("go env -json: %w", err)
	}
	cCompiler := "none"
	if compiler := strings.Fields(goEnvironment["CC"]); len(compiler) > 0 {
		version := exec.Command(compiler[0], append(compiler[1:], "--version")...)
		version.Dir = build.Tree
		version.Env = build.environment()
		if output, err := version.Output(); err == nil {
			cCompiler = strings.TrimSpace(string(output))
		}
	}
	directory, err := filepath.Abs(build.Tree)
	if err == nil {
		directory, err = filepath.EvalSymlinks(directory)
	}
	if err != nil {
		return BinaryInputs{}, err
	}
	environment := append([]string{}, build.Environment...)
	sort.Strings(environment)
	return BinaryInputs{Go: goVersion, Goos: goos, Goarch: goarch, Environment: environment, Flags: append([]string{}, testBinaryFlags...),
		GoEnvironment: goEnvironment, CCompiler: cCompiler, Directory: directory}, nil
}

// A ClosureFunc is a package's closure on a tree, as planner.Closure computes it: the Resident hands BinaryKeys its own
// warm one, and nil reads it cold from the tree.
type ClosureFunc func(tree, importPath string) (string, error)

// closureJobs is how many closures BinaryKeys reads at once: each is one go list of the package's whole import closure,
// a process mostly waiting on files.
const closureJobs = 16

// BinaryKeys keys each package's test binary on the tree, by import path. A package left out is compiled, never
// reused: one whose closure doesn't read (go list fails on it, and its own go test -c will say why), and one whose
// binary compiles the tree's own C (unkeyable), whose headers and libraries the closure doesn't hold. Each package it
// couldn't key is named, with why, in the notes.
func (build TreeBuild) BinaryKeys(packages []planner.ProductTest, inputs BinaryInputs, closure ClosureFunc) (map[string]string, []string) {
	if closure == nil {
		closure = planner.Closure
	}
	refused, err := build.unkeyable(packages)
	if err != nil {
		return map[string]string{}, []string{fmt.Sprintf("no binary is keyed, so every one compiles: %v", err)}
	}
	keys := make([]string, len(packages))
	errs := make([]error, len(packages))
	each(len(packages), closureJobs, func(index int) error {
		test := packages[index]
		if reason, found := refused[test.Package]; found {
			errs[index] = errors.New(reason)
			return nil
		}
		sum, err := closure(build.Tree, test.Package)
		if err == nil {
			keys[index], err = BinaryKey(test.Package, sum, inputs)
		}
		errs[index] = err
		return nil
	})
	keyed, notes := map[string]string{}, []string{}
	for index, test := range packages {
		if errs[index] != nil {
			notes = append(notes, fmt.Sprintf("%s compiles, unkeyed: %v", test.Package, errs[index]))
			continue
		}
		keyed[test.Package] = keys[index]
	}
	return keyed, notes
}

// unkeyable names the test packages whose binary no key can be trusted for, each with why: one whose binary compiles
// a cgo package of the tree's own (any but the standard library's, which the release and CC's version cover), since a
// cgo package's #cgo flags can reach headers outside its directory and link libraries that are in no package, and the
// closure holds neither; and one go list names no binary for. One go list of every package's test closure, under the
// build's environment, reads each binary's dependencies: its synthesized test main's (pkg.test's) Deps.
func (build TreeBuild) unkeyable(packages []planner.ProductTest) (map[string]string, error) {
	refused := map[string]string{}
	if len(packages) == 0 {
		return refused, nil
	}
	arguments := []string{"list", "-e", "-deps", "-test", "-f", "{{.ImportPath}}\t{{if and .CgoFiles (not .Standard)}}cgo{{end}}\t{{join .Deps \" \"}}"}
	for _, test := range packages {
		arguments = append(arguments, test.Package)
	}
	command := exec.Command("go", arguments...)
	command.Dir = build.Tree
	command.Env = build.environment()
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps -test: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	cgo, binaries := map[string]bool{}, map[string][]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 {
			continue
		}
		if fields[1] == "cgo" {
			cgo[fields[0]] = true
		}
		if binary, found := strings.CutSuffix(fields[0], ".test"); found && !strings.Contains(fields[0], " ") {
			binaries[binary] = strings.Fields(fields[2])
		}
	}
	for _, test := range packages {
		dependencies, listed := binaries[test.Package]
		if !listed {
			refused[test.Package] = "go list names no test binary for it"
		}
		for _, dependency := range dependencies {
			if cgo[dependency] {
				refused[test.Package] = fmt.Sprintf("its binary compiles %s, the tree's own C, whose headers and libraries its closure doesn't hold", dependency)
				break
			}
		}
	}
	return refused, nil
}

// lookupJobs is how many binary keys KeyedBinaries asks the store for at once: each is a ref's read, and a blob's head.
const lookupJobs = 16

// A BinaryReuse is what KeyedBinaries did: how many packages it keyed, found held in the store, and compiled, the
// seconds keying and asking took, and a note for each package it couldn't key or ask about.
type BinaryReuse struct {
	Keyed         int      `json:"keyed"`
	Held          int      `json:"held"`
	Compiled      int      `json:"compiled"`
	KeySeconds    float64  `json:"keySeconds"`
	LookupSeconds float64  `json:"lookupSeconds"`
	Notes         []string `json:"notes,omitempty"`
}

// KeyedBinaries is Binaries for only the packages whose binary the store lacks: it keys each package's binary
// (BinaryKeys), asks the store for each key (Store.Stored, which keeps a held binary and its ref fresh), and compiles
// the rest, Jobs at a time. A held binary's result names its stored blob (Binary) and BinaryHeld, and every keyed
// result its BinaryKey, under which PublishTree stores what was compiled. A key the store can't be asked about is a
// package compiled, never a build failed.
func (build TreeBuild) KeyedBinaries(store Store, packages []planner.ProductTest, inputs BinaryInputs, closure ClosureFunc) ([]TreePackage, BinaryReuse) {
	started := time.Now()
	keys, notes := build.BinaryKeys(packages, inputs, closure)
	reuse := BinaryReuse{Keyed: len(keys), KeySeconds: time.Since(started).Seconds()}
	started = time.Now()
	held := make([]string, len(packages))
	var mutex sync.Mutex
	each(len(packages), lookupJobs, func(index int) error {
		key, keyed := keys[packages[index].Package]
		if !keyed {
			return nil
		}
		sum, stored, err := store.Stored(key)
		if err != nil {
			mutex.Lock()
			notes = append(notes, fmt.Sprintf("%s compiles: asking the store for its binary %s: %v", packages[index].Package, key, err))
			mutex.Unlock()
			return nil
		}
		if stored {
			held[index] = sum
		}
		return nil
	})
	reuse.LookupSeconds = time.Since(started).Seconds()
	results := make([]TreePackage, len(packages))
	compile := []planner.ProductTest{}
	for index, test := range packages {
		if held[index] != "" {
			results[index] = TreePackage{Package: test.Package, Directory: test.Directory, Products: []string{}, Binary: held[index], BinaryKey: keys[test.Package], BinaryHeld: true}
			reuse.Held++
			continue
		}
		compile = append(compile, test)
	}
	compiled := build.Binaries(compile)
	next := 0
	for index := range packages {
		if held[index] != "" {
			continue
		}
		results[index] = compiled[next]
		results[index].BinaryKey = keys[packages[index].Package]
		next++
	}
	reuse.Compiled = len(compile)
	sort.Strings(notes)
	reuse.Notes = notes
	return results, reuse
}

// publishBinary stores a compiled binary's blob, gzipped, and returns the sha256 the store names for it: under its
// binary key as an action (Store.Publish: the blob, then its ref, a ConflictError when the ref names other bytes),
// or, unkeyed, as a blob alone.
func publishBinary(store Store, binaryKey string, blob []byte) (string, error) {
	if binaryKey == "" {
		return store.PutBlob(blob)
	}
	return store.Publish(binaryKey, blob)
}
