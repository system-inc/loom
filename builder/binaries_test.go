package builder

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/planner"
)

// binaryTree is a module of four test packages: a imports b and embeds a file, b imports nothing, c stands alone, and
// d imports a, so a change to b reaches a, b and d and never c. a's tests read a testdata file no binary holds.
func binaryTree(t *testing.T) string {
	t.Helper()
	return gitTree(t, map[string]string{
		"go.mod":              "module example.com/binaries\n\ngo 1.22\n",
		"README.md":           "a module of four test packages\n",
		"a/a.go":              "package a\n\nimport (\n\t_ \"embed\"\n\n\t\"example.com/binaries/b\"\n)\n\n//go:embed greeting.txt\nvar Greeting string\n\nfunc Answer() int { return b.Half() * 2 }\n",
		"a/greeting.txt":      "hello\n",
		"a/a_test.go":         "package a\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestA(t *testing.T) {\n\tif _, err := os.ReadFile(\"testdata/case.txt\"); err != nil || Answer() != 42 || Greeting != \"hello\\n\" {\n\t\tt.Fatal(err, Answer(), Greeting)\n\t}\n}\n",
		"a/testdata/case.txt": "case\n",
		"b/b.go":              "package b\n\nfunc Half() int { return 21 }\n",
		"b/b_test.go":         "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n",
		"c/c.go":              "package c\n\nfunc C() int { return 3 }\n",
		"c/c_test.go":         "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {\n\tif C() != 3 {\n\t\tt.Fatal(C())\n\t}\n}\n",
		"d/d_test.go":         "package d\n\nimport (\n\t\"testing\"\n\n\t\"example.com/binaries/a\"\n)\n\nfunc TestD(t *testing.T) {\n\tif a.Answer() != 42 {\n\t\tt.Fatal(a.Answer())\n\t}\n}\n",
	})
}

// binaryBuild is a tree build of tree, its binaries written under a directory of the test's own.
func binaryBuild(t *testing.T, tree string) TreeBuild {
	t.Helper()
	work := t.TempDir()
	build := TreeBuild{Tree: tree, Cache: filepath.Join(work, "cache"), Out: filepath.Join(work, "out"), Environment: planner.GateEnvironmentList(), Jobs: 2}
	os.MkdirAll(build.Out, 0o755)
	return build
}

// binaryInputs reads build's inputs for this machine, failing the test when it can't.
func binaryInputs(t *testing.T, build TreeBuild) BinaryInputs {
	t.Helper()
	inputs, err := build.BinaryInputs(runtime.Version(), runtime.GOOS, runtime.GOARCH)
	if err != nil {
		t.Fatal(err)
	}
	return inputs
}

// change writes content over a file of tree and commits it, as a branch's one-file change.
func change(t *testing.T, tree, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tree, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, tree, "commit", "-q", "-a", "-m", "change "+name)
}

// fileSum is a file's sha256.
func fileSum(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// A binary key holds every part of what go test -c reads: the package, its closure, and each of BinaryInputs, so each
// one changed alone moves the key, and the same parts key the same. A closure that isn't a sha256 is refused, and
// BinaryInputs reads the parts a binary's bytes hold (the tree's directory as go sees it, GOROOT, the module cache, go
// test's flags) from the machine. Mutants: the closure left out of BinaryKey's canonical form; go test's flags left
// out of BinaryInputs; GOROOT left out of binaryGoEnvironment; the tree's directory not resolved through its links.
func TestABinaryKeyHoldsEverythingABinaryReads(t *testing.T) {
	base := BinaryInputs{Go: "go1.27.1", Goos: "linux", Goarch: "amd64", Environment: []string{"ADAMIC_GATE_COHERE=1"}, Flags: []string{"-c"},
		GoEnvironment: map[string]string{"CC": "gcc", "GOFLAGS": "", "GOROOT": "/go"}, CCompiler: "gcc 13.2", Directory: "/home/ahra/clone"}
	closure := strings.Repeat("a", 64)
	key := func(importPath, closure string, inputs BinaryInputs) string {
		t.Helper()
		sum, err := BinaryKey(importPath, closure, inputs)
		if err != nil {
			t.Fatal(err)
		}
		return sum
	}
	held := key("example.com/p", closure, base)
	if again := key("example.com/p", closure, base); again != held || !productKeyPattern.MatchString(held) {
		t.Fatalf("the same parts keyed %s and %s", held, again)
	}
	changed := map[string]func(inputs *BinaryInputs){
		"the Go release":       func(inputs *BinaryInputs) { inputs.Go = "go1.27.2" },
		"the GOOS":             func(inputs *BinaryInputs) { inputs.Goos = "darwin" },
		"the GOARCH":           func(inputs *BinaryInputs) { inputs.Goarch = "arm64" },
		"the gate environment": func(inputs *BinaryInputs) { inputs.Environment = []string{"ADAMIC_GATE_COHERE=0"} },
		"go test's flags":      func(inputs *BinaryInputs) { inputs.Flags = []string{"-c", "-race"} },
		"GOFLAGS": func(inputs *BinaryInputs) {
			inputs.GoEnvironment = map[string]string{"CC": "gcc", "GOFLAGS": "-trimpath", "GOROOT": "/go"}
		},
		"the C compiler": func(inputs *BinaryInputs) {
			inputs.GoEnvironment = map[string]string{"CC": "clang", "GOFLAGS": "", "GOROOT": "/go"}
		},
		"the C compiler's version": func(inputs *BinaryInputs) { inputs.CCompiler = "gcc 14.1" },
		"the tree's directory":     func(inputs *BinaryInputs) { inputs.Directory = "/home/ahra/other" },
		"GOROOT, written in by path": func(inputs *BinaryInputs) {
			inputs.GoEnvironment = map[string]string{"CC": "gcc", "GOFLAGS": "", "GOROOT": "/usr/local/go"}
		},
	}
	for name, change := range changed {
		inputs := base
		change(&inputs)
		if key("example.com/p", closure, inputs) == held {
			t.Errorf("%s changed and the key didn't move", name)
		}
	}
	if key("example.com/q", closure, base) == held || key("example.com/p", strings.Repeat("b", 64), base) == held {
		t.Error("another package, or another closure, keyed the same")
	}
	if _, err := BinaryKey("example.com/p", "not a closure", base); err == nil {
		t.Error("a closure that isn't a sha256 was keyed")
	}

	// Read from a tree reached through a link: the directory is the one go builds in, every link resolved.
	tree := binaryTree(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(tree, linked); err != nil {
		t.Fatal(err)
	}
	inputs := binaryInputs(t, binaryBuild(t, linked))
	resolved, _ := filepath.EvalSymlinks(tree)
	if inputs.Directory != resolved || inputs.GoEnvironment["GOROOT"] == "" || inputs.GoEnvironment["GOMODCACHE"] == "" ||
		!slices.Equal(inputs.Flags, testBinaryFlags) || inputs.CCompiler == "" || len(inputs.Environment) == 0 {
		t.Fatalf("the inputs read %+v", inputs)
	}
}

// The mutant that matters most: a package's key moves when a file in its closure changes (a file of a package it
// imports, a file it embeds, one of its tests or of a package it imports, its module's go.mod) and stays when one outside it does (a package it
// doesn't import, a testdata file its tests read at run time, a file no package holds). Mutants: BinaryKeys keying the
// closure of another package than the one it keys; the closure left out of BinaryKey's canonical form.
func TestABinaryKeyMovesWithItsClosureAndNoOtherFile(t *testing.T) {
	tree := binaryTree(t)
	build := binaryBuild(t, tree)
	inputs := binaryInputs(t, build)
	packages, err := TestPackages(tree)
	if err != nil || len(packages) != 4 {
		t.Fatalf("test packages %+v %v", packages, err)
	}
	keys := func() map[string]string {
		t.Helper()
		keyed, notes := build.BinaryKeys(packages, inputs, nil)
		if len(keyed) != 4 || len(notes) != 0 {
			t.Fatalf("keyed %v, notes %v", keyed, notes)
		}
		return keyed
	}
	held := keys()
	if held["example.com/binaries/a"] == held["example.com/binaries/c"] {
		t.Fatal("two packages keyed the same")
	}
	cases := []struct {
		file  string
		moves []string
	}{
		{"b/b.go", []string{"a", "b", "d"}},
		{"a/greeting.txt", []string{"a", "d"}},
		// The closure holds the test files of every package in it, so d, which imports a, moves with a's tests too:
		// more than d's binary reads, never less.
		{"a/a_test.go", []string{"a", "d"}},
		{"go.mod", []string{"a", "b", "c", "d"}},
		{"c/c.go", []string{"c"}},
		{"a/testdata/case.txt", nil},
		{"README.md", nil},
	}
	for _, each := range cases {
		path := filepath.Join(tree, each.file)
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		changed := append(append([]byte{}, original...), []byte("\n// a branch's change\n")...)
		if err = os.WriteFile(path, changed, 0o644); err != nil {
			t.Fatal(err)
		}
		now := keys()
		for _, name := range []string{"a", "b", "c", "d"} {
			moved := now["example.com/binaries/"+name] != held["example.com/binaries/"+name]
			if moved != slices.Contains(each.moves, name) {
				t.Errorf("%s changed: %s's key moved %v, and should have %v", each.file, name, moved, !moved)
			}
		}
		if err = os.WriteFile(path, original, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if again := keys(); !maps(again, held) {
		t.Fatalf("the tree restored keyed %v, not %v", again, held)
	}
}

// Byte identity: a binary compiled for a key is the bytes a stored one under that key is. The same package compiled
// in the same directory on two trees (the second a branch's change to a package outside its closure) keys the same and
// is the same bytes; compiled in another directory, its bytes differ (go writes the directory into a binary built
// without -trimpath), and so does its key, by BinaryInputs.Directory. Mutant: the directory left out of BinaryInputs.
func TestAStoredBinaryIsTheBytesACompileMakes(t *testing.T) {
	tree := binaryTree(t)
	clone := filepath.Join(t.TempDir(), "clone")
	if output, err := exec.Command("git", "clone", "-q", tree, clone).CombinedOutput(); err != nil {
		t.Fatalf("git clone: %v %s", err, output)
	}
	compiled := func(tree string) (string, string) {
		t.Helper()
		build := binaryBuild(t, tree)
		packages := []planner.ProductTest{{Package: "example.com/binaries/a", Directory: "a"}}
		keyed, notes := build.BinaryKeys(packages, binaryInputs(t, build), nil)
		built := build.Binaries(packages)
		if len(notes) != 0 || built[0].Error != "" {
			t.Fatalf("notes %v, built %+v", notes, built[0])
		}
		return keyed["example.com/binaries/a"], fileSum(t, filepath.Join(build.Out, "example.com_binaries_a.test"))
	}
	key, bytes := compiled(tree)
	change(t, tree, "c/c.go", "package c\n\nfunc C() int { return 4 }\n")
	if againKey, againBytes := compiled(tree); againKey != key || againBytes != bytes {
		t.Fatalf("a branch outside a's closure: key %s then %s, bytes %s then %s", key, againKey, bytes, againBytes)
	}
	elsewhereKey, elsewhereBytes := compiled(clone)
	if elsewhereBytes == bytes {
		t.Log("compiled in another directory, the binary is the same bytes: this go writes no directory into it")
	}
	if elsewhereKey == key {
		t.Fatalf("compiled in another directory, the key %s didn't move", key)
	}
}

// A branch compiles only the packages its change reaches: the first tree compiles every binary and stores each under
// its key; a branch changing b compiles a, b and d, and names c's stored blob in its index, which a runner fetches and
// runs as it would a compiled one. Asked again, the same tree compiles nothing. Mutants: publishBinary storing a keyed
// binary as a blob alone (no ref, so the branch compiles all four); KeyedBinaries ignoring the store's answer; a held
// result not naming its stored blob; a compiled result not carrying its key to PublishTree.
func TestABranchCompilesOnlyThePackagesItsChangeReaches(t *testing.T) {
	tree := binaryTree(t)
	fake, store := serve(t)
	built := func() (TreeIndex, BinaryReuse, string) {
		t.Helper()
		build := binaryBuild(t, tree)
		packages, err := TestPackages(tree)
		if err != nil {
			t.Fatal(err)
		}
		results, reuse := build.KeyedBinaries(store, packages, binaryInputs(t, build), nil)
		source, err := SourceChunks(tree, nil)
		if err != nil {
			t.Fatal(err)
		}
		hash, err := planner.TreeHash(tree)
		if err != nil {
			t.Fatal(err)
		}
		index := TreeIndex{Tree: hash, Future: "f", Go: "go1.27.1", Packages: map[string]TreePackage{}}
		for _, result := range results {
			index.Packages[result.Package] = result
		}
		treeKey, written, err := PublishTree(store, &index, build.Out, build.Cache, &source, nil)
		if err != nil || !written || index.failures() != 0 {
			t.Fatalf("publishing: %v %v %+v", written, err, index.Packages)
		}
		return index, reuse, treeKey
	}
	first, reuse, _ := built()
	if reuse.Keyed != 4 || reuse.Held != 0 || reuse.Compiled != 4 || len(fake.Keys("refs/action/")) != 4 {
		t.Fatalf("the first tree: %+v, refs %v", reuse, fake.Keys("refs/action/"))
	}
	for name, result := range first.Packages {
		if ref, _ := fake.Object("refs/action/" + result.BinaryKey); string(ref) != result.Binary || result.BinaryHeld {
			t.Fatalf("%s: its ref holds %q, its binary is %+v", name, ref, result)
		}
	}
	change(t, tree, "b/b.go", "package b\n\n// Half is half the answer.\nfunc Half() int { return 21 }\n")
	branch, reuse, treeKey := built()
	if reuse.Held != 1 || reuse.Compiled != 3 || !branch.Packages["example.com/binaries/c"].BinaryHeld {
		t.Fatalf("the branch: %+v", reuse)
	}
	if held := branch.Packages["example.com/binaries/c"]; held.Binary != first.Packages["example.com/binaries/c"].Binary || held.BinaryKey != first.Packages["example.com/binaries/c"].BinaryKey {
		t.Fatalf("c, held: %+v, first %+v", held, first.Packages["example.com/binaries/c"])
	}
	for _, name := range []string{"a", "b", "d"} {
		if result := branch.Packages["example.com/binaries/"+name]; result.BinaryHeld || result.BinaryKey == first.Packages["example.com/binaries/"+name].BinaryKey {
			t.Fatalf("%s, reached by the change, wasn't compiled: %+v", name, result)
		}
	}
	unit := t.TempDir()
	if _, err := (Store{Read: fake.Public()}).FetchPackage(context.Background(), treeKey, "example.com/binaries/c", unit); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(filepath.Join(unit, "test"), "-test.v")
	command.Dir = filepath.Join(unit, "source", "c")
	if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "--- PASS: TestC") {
		t.Fatalf("the held binary, fetched: %v\n%s", err, output)
	}
	if _, reuse, _ = built(); reuse.Held != 4 || reuse.Compiled != 0 {
		t.Fatalf("the same tree again: %+v", reuse)
	}
}

// A binary that compiles the tree's own C is never keyed, whatever its closure: a cgo package's flags reach a header
// outside its directory here, which no closure holds. A package importing it is refused too, and one that doesn't is
// keyed. Mutant: unkeyable reading no cgo dependency of a binary.
func TestABinaryThatCompilesTheTreesOwnCIsNeverKeyed(t *testing.T) {
	if _, err := exec.LookPath("cc"); err != nil {
		t.Skip("no C compiler")
	}
	tree := gitTree(t, map[string]string{
		"go.mod":                "module example.com/withc\n\ngo 1.22\n",
		"include/answer.h":      "#define ANSWER 42\n",
		"native/native.go":      "package native\n\n// #cgo CFLAGS: -I${SRCDIR}/../include\n// #include \"answer.h\"\nimport \"C\"\n\nfunc Answer() int { return int(C.ANSWER) }\n",
		"native/native_test.go": "package native\n\nimport \"testing\"\n\nfunc TestNative(t *testing.T) {}\n",
		"user/user_test.go":     "package user\n\nimport (\n\t\"testing\"\n\n\t\"example.com/withc/native\"\n)\n\nfunc TestUser(t *testing.T) { _ = native.Answer() }\n",
		"plain/plain_test.go":   "package plain\n\nimport \"testing\"\n\nfunc TestPlain(t *testing.T) {}\n",
	})
	build := binaryBuild(t, tree)
	build.Environment = append(build.Environment, "CGO_ENABLED=1")
	packages, err := TestPackages(tree)
	if err != nil || len(packages) != 3 {
		t.Fatalf("test packages %+v %v", packages, err)
	}
	keyed, notes := build.BinaryKeys(packages, binaryInputs(t, build), nil)
	if _, found := keyed["example.com/withc/plain"]; len(keyed) != 1 || !found || len(notes) != 2 {
		t.Fatalf("keyed %v, notes %v", keyed, notes)
	}
	for _, note := range notes {
		if !strings.Contains(note, "example.com/withc/native") || !strings.Contains(note, "the tree's own C") {
			t.Fatalf("note %q", note)
		}
	}
}

// A compiled binary whose key's ref names other bytes fails its package as Workshop's, naming both, and the ref is
// never overwritten: one key, two binaries, so the key isn't honest or the build isn't reproducible. Mutants:
// publishBinary sending a keyed binary as a blob alone; PublishTree swallowing the binary's ConflictError.
func TestABinaryWhoseRefNamesOtherBytesFailsItsPackage(t *testing.T) {
	tree := binaryTree(t)
	fake, store := serve(t)
	build := binaryBuild(t, tree)
	packages := []planner.ProductTest{{Package: "example.com/binaries/c", Directory: "c"}}
	results, reuse := build.KeyedBinaries(store, packages, binaryInputs(t, build), nil)
	if reuse.Compiled != 1 || results[0].BinaryKey == "" {
		t.Fatalf("%+v %+v", reuse, results)
	}
	other := mustGzip(t, []byte("another build's binary"))
	fake.Set("blobs/"+digest(other), other, time.Now())
	fake.Set("refs/action/"+results[0].BinaryKey, []byte(digest(other)), time.Now())
	source, err := SourceChunks(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	index := TreeIndex{Tree: "0123456789abcdef0123456789abcdef01234567", Go: "go1.27.1", Packages: map[string]TreePackage{results[0].Package: results[0]}}
	if _, _, err = PublishTree(store, &index, build.Out, build.Cache, &source, nil); err != nil {
		t.Fatal(err)
	}
	if broke := index.Packages[results[0].Package]; broke.Failure != WorkshopFailure || !strings.Contains(broke.Error, digest(other)) {
		t.Fatalf("the package: %+v", broke)
	}
	if ref, _ := fake.Object("refs/action/" + results[0].BinaryKey); string(ref) != digest(other) {
		t.Fatalf("the ref was overwritten with %q", ref)
	}
}

// A runner refuses an index naming a binary key that isn't a sha256. Mutant: ParseTree not reading a package's binary
// key.
func TestAnIndexNamingABinaryKeyThatIsntOneIsPoisoned(t *testing.T) {
	binary := strings.Repeat("b", 64)
	source := []byte("source")
	chunks := []SourceChunk{{Blob: digest(source), First: "a", Last: "a", Files: 1, Bytes: int64(len(source))}}
	for key, poisoned := range map[string]bool{"": false, strings.Repeat("c", 64): false, "../refs/action/x": true} {
		index := TreeIndex{Format: TreeIndexFormat, Tree: "t", Source: chunks, Products: map[string]string{},
			Packages: map[string]TreePackage{"p": {Package: "p", Directory: "p", Binary: binary, BinaryKey: key, Products: []string{}}}}
		encoded, _ := index.encode()
		if _, err := ParseTree(strings.Repeat("e", 64), encoded); (err != nil) != poisoned {
			t.Errorf("binary key %q: %v", key, err)
		}
	}
}
