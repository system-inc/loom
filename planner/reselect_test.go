package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// A tree where each way a unit holds an input is one edit away: a's own source (closure) and testdata (reads), b's
// import of lib (closure), c's reads line over a's testdata (reads), c's declared compiler package's source (reads,
// since c's tests build it) and the compiler's product test's own testdata (products: only the compiler's product key
// holds it), a declared compiler with no product test, as all of adamic's are (reads alone), and a file no unit holds.
func reselectFixture(t *testing.T) (tree, gateTools string) {
	t.Helper()
	tree, gateTools = t.TempDir(), t.TempDir()
	writeFiles(t, tree, map[string]string{
		"go.mod":                     "module example.com/reselect\n\ngo 1.22\n",
		"a/a.go":                     "package a\n",
		"a/a_test.go":                "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"a/testdata/case.txt":        "case\n",
		"lib/lib.go":                 "package lib\n",
		"b/b.go":                     "package b\n\nimport _ \"example.com/reselect/lib\"\n",
		"b/b_test.go":                "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n",
		"c/c.go":                     "package c\n",
		"c/c_test.go":                "package c\n\nimport \"testing\"\n\nfunc TestC(t *testing.T) {}\n",
		"compiler/compile.go":        "package compiler\n",
		"compiler/compile_test.go":   "package compiler\n\nimport \"testing\"\n\nfunc TestProduct_Stage0(t *testing.T) {}\n",
		"compiler/testdata/seed.txt": "seed\n",
		"plain/plain.go":             "package plain\n",
		"notes/readme.txt":           "no unit reads this\n",
		"cloud/fast-gate/compiler-dependencies.json": `{"version": 1, "packages": {"c": ["compiler", "plain"]}}`,
	})
	writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": "reads c a/testdata/*\n"})
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	return tree, gateTools
}

// reselectEdits are the edits and, for each, exactly the units that hold the edited file.
var reselectEdits = []struct {
	path  string
	units []string
}{
	{"a/a.go", []string{"a"}},
	{"a/testdata/case.txt", []string{"a", "c"}},
	{"lib/lib.go", []string{"b"}},
	{"compiler/compile.go", []string{"c", "compiler"}},
	{"compiler/testdata/seed.txt", []string{"c", "compiler"}},
	{"plain/plain.go", []string{"c"}},
	{"notes/readme.txt", nil},
}

// reselectFailures plans the fixture with keyFor, records every key as passed, then makes each edit in turn and
// returns every way the plan after it differs from "exactly the units that hold the file run, the rest reuse".
func reselectFailures(t *testing.T, keyFor keyFunction) []string {
	t.Helper()
	tree, gateTools := reselectFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	plan := func(index VerdictIndex) []PlannedResult {
		results, err := planTree(tree, gateTools, tools, index, false, keyFor, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		return results
	}
	index := MemoryIndex{}
	for _, result := range plan(MemoryIndex{}) {
		index[result.UnitKey] = Verdict{UnitKey: result.UnitKey, Status: "passed", Run: "run-1"}
	}
	failures := []string{}
	for _, edit := range reselectEdits {
		path := filepath.Join(tree, filepath.FromSlash(edit.path))
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(path, append(append([]byte{}, original...), "\n// moved\n"...), 0o644)
		ran := []string{}
		for _, result := range plan(index) {
			if result.Decision == "run" {
				ran = append(ran, strings.TrimPrefix(result.Name, "example.com/reselect/"))
			}
		}
		os.WriteFile(path, original, 0o644)
		sort.Strings(ran)
		if fmt.Sprint(ran) != fmt.Sprint(edit.units) {
			failures = append(failures, fmt.Sprintf("editing %s ran %v, want %v", edit.path, ran, edit.units))
		}
	}
	return failures
}

// An edit reselects exactly the units that hold the edited file, through every way a unit holds one: its closure,
// its declared reads and its products. Everything else reuses its passed verdict.
func TestAnInputChangeReselectsExactlyItsUnits(t *testing.T) {
	t.Parallel()
	if failures := reselectFailures(t, KeyFor); len(failures) > 0 {
		t.Fatalf("selection by key:\n%s", strings.Join(failures, "\n"))
	}
}

// Each mutant is a selector that's wrong in one way: three keys that drop a part a unit holds its inputs through,
// so an edit there reuses a stale verdict (a silent false green), and one key over the whole tree, so every edit
// reruns everything. The test above must catch each one.
func TestTheSelectorsMutantsAreCaught(t *testing.T) {
	t.Parallel()
	mutants := map[string]func(parts KeyParts, tree string) KeyParts{
		"closure dropped":  func(parts KeyParts, _ string) KeyParts { parts.Closure = ""; return parts },
		"reads dropped":    func(parts KeyParts, _ string) KeyParts { parts.Reads = ""; return parts },
		"products dropped": func(parts KeyParts, _ string) KeyParts { parts.Products = nil; return parts },
		"the whole tree": func(parts KeyParts, tree string) KeyParts {
			parts.GateInputs = wholeTreeHash(t, tree)
			return parts
		},
	}
	// A KeyFor that keys a test unit without its declared compilers: their sources reach the key only through its reads.
	compilersDropped := func(tree, gateTools string, unit Unit, tools Tools, _ []string) (KeyParts, error) {
		return KeyFor(tree, gateTools, unit, tools, nil)
	}
	t.Run("compilers dropped", func(t *testing.T) {
		t.Parallel()
		failures := reselectFailures(t, compilersDropped)
		if len(failures) == 0 {
			t.Fatal("the selector without its compilers passed the reselection test")
		}
		t.Logf("caught:\n%s", strings.Join(failures, "\n"))
	})
	for name, mutate := range mutants {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mutant := func(tree, gateTools string, unit Unit, tools Tools, compilerPackages []string) (KeyParts, error) {
				parts, err := KeyFor(tree, gateTools, unit, tools, compilerPackages)
				return mutate(parts, tree), err
			}
			failures := reselectFailures(t, mutant)
			if len(failures) == 0 {
				t.Fatalf("the selector with its %s passed the reselection test", name)
			}
			t.Logf("caught:\n%s", strings.Join(failures, "\n"))
		})
	}
}

func wholeTreeHash(t *testing.T, tree string) string {
	t.Helper()
	output, err := exec.Command("git", "-C", tree, "ls-files", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.New()
	for _, file := range strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00") {
		content, err := os.ReadFile(filepath.Join(tree, file))
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(hash, "%s %x\n", file, sha256.Sum256(content))
	}
	return hex.EncodeToString(hash.Sum(nil))
}
