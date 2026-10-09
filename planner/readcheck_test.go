package planner

import (
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Every form a traced read takes, and the ones that aren't reads: a failed open, a write-only open, an O_PATH open.
func TestTracedReadsParsesStraceForms(t *testing.T) {
	t.Parallel()
	trace := strings.Join([]string{
		`101 openat(AT_FDCWD</work/p>, "testdata/case.txt", O_RDONLY|O_CLOEXEC) = 3</work/p/testdata/case.txt>`,
		`[pid   102] openat(AT_FDCWD, "relative.txt", O_RDONLY) = 4`,
		`103 openat(7</work/q>, "data.txt", O_RDWR|O_CREAT, 0644) = 5`,
		`104 openat(AT_FDCWD</work/p>, "missing.txt", O_RDONLY) = -1 ENOENT (No such file or directory)`,
		`105 openat(AT_FDCWD</work/p>, "out.txt", O_WRONLY|O_CREAT|O_TRUNC, 0644) = 6`,
		`106 openat(AT_FDCWD</work/p>, "probe", O_RDONLY|O_PATH) = 7`,
		`107 openat(AT_FDCWD</work/p>, "split.txt", O_RDONLY <unfinished ...>`,
		`108 open("/work/r/old.txt", O_RDONLY) = 8`,
		`107 <... openat resumed>) = 9</work/p/split.txt>`,
		`109 execve("/work/tools/run.sh", ["run.sh"], 0x7ffd /* 12 vars */) = 0`,
		`110 openat2(AT_FDCWD</work>, "s/new.txt", {flags=O_RDONLY|O_CLOEXEC, resolve=0}, 24) = 10`,
		`111 openat(AT_FDCWD</work>, "esc\x61ped\n.txt", O_RDONLY) = 11`,
		`112 +++ exited with 0 +++`,
	}, "\n")
	reads, err := TracedReads(strings.NewReader(trace), "/work/default")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/work/default/relative.txt", "/work/escaped\n.txt", "/work/p/split.txt", "/work/p/testdata/case.txt",
		"/work/q/data.txt", "/work/r/old.txt", "/work/s/new.txt", "/work/tools/run.sh"}
	if !reflect.DeepEqual(reads, want) {
		t.Fatalf("traced reads\n%q\nwant\n%q", reads, want)
	}
}

// A tree where p's run reads through every declared route (its closure, a dependency's source, its testdata, a
// reads line, its declared compiler package) and also reads one tracked file nothing declares.
func readCheckFixture(t *testing.T, executors string) (tree, gateTools string) {
	t.Helper()
	tree, gateTools = t.TempDir(), t.TempDir()
	writeFiles(t, tree, map[string]string{
		"go.mod":              "module example.com/readcheck\n\ngo 1.22\n",
		"p/p.go":              "package p\n\nimport _ \"example.com/readcheck/lib\"\n",
		"p/p_test.go":         "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n",
		"p/testdata/case.txt": "case\n",
		"lib/lib.go":          "package lib\n",
		"compiler/compile.go": "package compiler\n",
		"q/declared.txt":      "declared\n",
		"q/undeclared.txt":    "nobody declared this\n",
	})
	writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": executors})
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	return tree, gateTools
}

func findingPaths(t *testing.T, tree, gateTools string, compilers []string, traced []string) []string {
	t.Helper()
	unit := Unit{Kind: "test", Package: packageOf(t, tree), Directory: "p"}
	findings, err := CheckReads(tree, gateTools, unit, compilers, traced)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{}
	for _, finding := range findings {
		// A file's is its sha256; a gitlink's, its recorded commit.
		if !(Sha256Hex(finding.Sha256) || len(finding.Sha256) == 40 && Sha256Hex(finding.Sha256+strings.Repeat("0", 24))) || finding.Package != unit.Package {
			t.Errorf("finding %+v lacks its content hash or package", finding)
		}
		paths = append(paths, finding.Path)
	}
	return paths
}

// Only the tracked read nothing declares is a finding. Mutants: a reader whose reads line is gone, and a unit whose
// compiler declaration is gone, each turn a declared read into a finding, so the check consults both.
func TestCheckReadsFindsOnlyReadsOutsideTheDeclaredInputs(t *testing.T) {
	t.Parallel()
	traced := func(tree string) []string {
		paths := []string{"/usr/local/go/src/fmt/print.go", "/tmp/go-build123/b001/p.test", filepath.Join(tree, "p/out.log")}
		for _, file := range []string{"p/p.go", "p/p_test.go", "lib/lib.go", "p/testdata/case.txt", "q/declared.txt",
			"compiler/compile.go", "q/undeclared.txt"} {
			paths = append(paths, filepath.Join(tree, filepath.FromSlash(file)))
		}
		return paths
	}
	compilers := []string{"example.com/readcheck/compiler"}
	for _, check := range []struct {
		name      string
		executors string
		compilers []string
		want      []string
	}{
		{"declared", "reads p q/declared.txt\n", compilers, []string{"q/undeclared.txt"}},
		{"mutant: the reader's reads line gone", "# none\n", compilers, []string{"q/declared.txt", "q/undeclared.txt"}},
		{"mutant: the compiler declaration gone", "reads p q/declared.txt\n", nil, []string{"compiler/compile.go", "q/undeclared.txt"}},
	} {
		t.Run(check.name, func(t *testing.T) {
			t.Parallel()
			tree, gateTools := readCheckFixture(t, check.executors)
			if got := findingPaths(t, tree, gateTools, check.compilers, traced(tree)); !reflect.DeepEqual(got, check.want) {
				t.Fatalf("findings %v, want %v", got, check.want)
			}
		})
	}
}

// A submodule is read as its gitlink: a reads line naming the gitlink hashes its recorded commit (not the directory),
// a read inside it with that line is declared, without it is a finding on the gitlink, and a closure file inside it
// stays declared by its closure either way.
func TestASubmoduleIsReadAsItsGitlink(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	root := t.TempDir()
	git := func(directory string, arguments ...string) {
		t.Helper()
		if output, err := exec.Command("git", append([]string{"-C", directory, "-c", "user.name=t", "-c", "user.email=t@t"}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	sub, tree, gateTools := filepath.Join(root, "sub"), filepath.Join(root, "tree"), filepath.Join(root, "tools")
	writeFiles(t, sub, map[string]string{"go.mod": "module example.com/shim\n\ngo 1.22\n", "shim.go": "package shim\n", "data.txt": "data\n"})
	git(root, "init", "-q", sub)
	git(sub, "add", ".")
	git(sub, "commit", "-q", "-m", "one")
	writeFiles(t, tree, map[string]string{
		"go.mod":      "module example.com/gitlink\n\ngo 1.22\n\nrequire example.com/shim v0.0.0\n\nreplace example.com/shim => ./sub\n",
		"p/p.go":      "package p\n\nimport _ \"example.com/shim\"\n",
		"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n",
	})
	git(root, "init", "-q", tree)
	git(tree, "submodule", "add", "-q", sub, "sub")
	git(tree, "add", ".")
	for _, check := range []struct {
		executors string
		want      []string
	}{{"reads p sub\n", []string{}}, {"# none\n", []string{"sub"}}} {
		writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": check.executors})
		traced := []string{filepath.Join(tree, "sub/shim.go"), filepath.Join(tree, "sub/data.txt")}
		if got := findingPaths(t, tree, gateTools, nil, traced); !reflect.DeepEqual(got, check.want) {
			t.Errorf("with %q: findings %v, want %v", check.executors, got, check.want)
		}
	}
	writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": "reads p sub\n"})
	reads, err := DeclaredReads(tree, gateTools, "p")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReadsHash(tree, reads); err != nil || !reflect.DeepEqual(reads, []string{"sub"}) {
		t.Fatalf("reads %v hashed with %v, want the gitlink hashed by its commit", reads, err)
	}
}

// packageOf is the import path of the fixture's package p.
func packageOf(t *testing.T, tree string) string {
	t.Helper()
	module, err := modulePath(tree)
	if err != nil {
		t.Fatal(err)
	}
	return module + "/p"
}
