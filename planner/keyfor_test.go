package planner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func keyForFixture(t *testing.T) (tree, tools string) {
	t.Helper()
	tree, tools = t.TempDir(), t.TempDir()
	writeFiles(t, tree, map[string]string{
		"go.mod":              "module example.com/key\n\ngo 1.22\n",
		"a/a.go":              "package a\n\nconst Answer = 1\n",
		"a/a_test.go":         "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"a/testdata/case.txt": "case\n",
		"compiler/c.go":       "package compiler\n\nconst Version = 1\n",
		"shared/read.json":    "{}\n",
		"z/z.go":              "package z\n",
	})
	writeFiles(t, tools, map[string]string{"cloud/fast-gate/executors.txt": "reads a shared/read.json\n"})
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	return tree, tools
}

// The assembled key moves for its closure, its reads, a compiler package it builds with and its env, and for
// nothing else in the tree.
func TestKeyForMovesForEveryInputAndNothingElse(t *testing.T) {
	t.Parallel()
	tree, gateTools := keyForFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0", Clang: "20.1.0", Node: "v24.1.0", WasiSdk: "27"}
	unit := Unit{Kind: "test", Package: "example.com/key/a", Directory: "a", Run: "^TestA$", Environment: map[string]string{"ADAMIC_GATE_UNCACHED": "1"},
		Products: []string{}}
	key := func(unit Unit) string {
		parts, err := KeyFor(tree, gateTools, unit, tools, []string{"example.com/key/compiler"})
		if err != nil {
			t.Fatal(err)
		}
		value, err := UnitKey(parts)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := key(unit)
	for _, edit := range []struct {
		path  string
		moves bool
	}{{"a/a.go", true}, {"a/testdata/case.txt", true}, {"shared/read.json", true}, {"compiler/c.go", true}, {"z/z.go", false}} {
		path := filepath.Join(tree, filepath.FromSlash(edit.path))
		original, _ := os.ReadFile(path)
		os.WriteFile(path, append(append([]byte{}, original...), '\n'), 0o644)
		moved := key(unit) != base
		os.WriteFile(path, original, 0o644)
		if moved != edit.moves {
			t.Errorf("editing %s: key moved %v, want %v", edit.path, moved, edit.moves)
		}
	}
	switched := unit
	switched.Environment = map[string]string{"ADAMIC_GATE_UNCACHED": "0"}
	if key(switched) == base {
		t.Error("an env switch's value left the key unchanged")
	}
}

func TestKeyEnvIsClosedAndReadsTheChangedFile(t *testing.T) {
	t.Parallel()
	if _, err := KeyEnv(map[string]string{"ADAMIC_NEW_SWITCH": "1"}); err == nil {
		t.Fatal("a switch outside the closed set was keyed instead of refused")
	}
	directory := t.TempDir()
	first, second := filepath.Join(directory, "one"), filepath.Join(directory, "two")
	os.WriteFile(first, []byte("a/a.go\n"), 0o644)
	os.WriteFile(second, []byte("a/a.go\n"), 0o644)
	left, _ := KeyEnv(map[string]string{"ADAMIC_GATE_CHANGED": first, "PATH": "/bin"})
	right, _ := KeyEnv(map[string]string{"ADAMIC_GATE_CHANGED": second})
	if left["ADAMIC_GATE_CHANGED"] != right["ADAMIC_GATE_CHANGED"] || len(left) != 1 {
		t.Fatalf("the changed-paths file keyed by its path or kept a non-ADAMIC variable: %v %v", left, right)
	}
	os.WriteFile(second, []byte("b/b.go\n"), 0o644)
	if moved, _ := KeyEnv(map[string]string{"ADAMIC_GATE_CHANGED": second}); moved["ADAMIC_GATE_CHANGED"] == left["ADAMIC_GATE_CHANGED"] {
		t.Fatal("a changed-paths file with other contents keyed the same")
	}
}

// A test unit keyed without its products is refused, never keyed as if it had none.
func TestKeyForRefusesATestUnitWithoutItsProducts(t *testing.T) {
	t.Parallel()
	tree, gateTools := keyForFixture(t)
	unit := Unit{Kind: "test", Package: "example.com/key/a", Directory: "a"}
	if _, err := KeyFor(tree, gateTools, unit, Tools{}, nil); err == nil {
		t.Fatal("a test unit with nil products was keyed")
	}
}
