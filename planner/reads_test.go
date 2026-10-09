package planner

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFiles(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// A tree with package p, its testdata, files other packages hold, and executors.txt in the tools and in the tree.
func readsFixture(t *testing.T) (tree, tools string) {
	t.Helper()
	tree, tools = t.TempDir(), t.TempDir()
	writeFiles(t, tree, map[string]string{
		"p/p.go":                        "package p\n",
		"p/testdata/case.txt":           "case\n",
		"q/top.json":                    "{}\n",
		"q/deep/nested.json":            "[]\n",
		"q/other.txt":                   "no\n",
		"r/extra.txt":                   "extra\n",
		"z/unrelated.txt":               "no\n",
		"cloud/fast-gate/executors.txt": "reads p r/extra.txt\n",
	})
	writeFiles(t, tools, map[string]string{"cloud/fast-gate/executors.txt": "# the gate tools\nreads p q/*.json\nreads other z/*\n"})
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	return tree, tools
}

// Own testdata, the tools' globs (whose * spans directories, as the gate's fnmatch does) and the tree's own lines.
func TestDeclaredReadsAreOwnTestdataAndReadsLines(t *testing.T) {
	t.Parallel()
	tree, tools := readsFixture(t)
	reads, err := DeclaredReads(tree, tools, "p")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"p/testdata/case.txt", "q/deep/nested.json", "q/top.json", "r/extra.txt"}
	if !reflect.DeepEqual(reads, want) {
		t.Fatalf("declared reads %v, want %v", reads, want)
	}
	if _, err := DeclaredReads(tree, "", "p"); err == nil {
		t.Fatal("declared reads without the gate tools named none and passed")
	}
}

// The reads part moves for an edit to a file the unit reads and for nothing else.
func TestReadsHashMovesOnlyForReadFiles(t *testing.T) {
	t.Parallel()
	tree, tools := readsFixture(t)
	hash := func() string {
		reads, err := DeclaredReads(tree, tools, "p")
		if err != nil {
			t.Fatal(err)
		}
		value, err := ReadsHash(tree, reads)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	base := hash()
	for _, edit := range []struct {
		path  string
		moves bool
	}{{"p/testdata/case.txt", true}, {"q/deep/nested.json", true}, {"r/extra.txt", true}, {"q/other.txt", false}, {"z/unrelated.txt", false}} {
		path := filepath.Join(tree, filepath.FromSlash(edit.path))
		original, _ := os.ReadFile(path)
		os.WriteFile(path, append(append([]byte{}, original...), '!'), 0o644)
		moved := hash() != base
		os.WriteFile(path, original, 0o644)
		if moved != edit.moves {
			t.Errorf("editing %s: reads moved %v, want %v", edit.path, moved, edit.moves)
		}
	}
}

// A tracked symlink in a unit's testdata keys as its target, never followed: a dangling one keys, and retargeting it
// moves the reads part.
func TestASymlinkReadKeysAsItsTarget(t *testing.T) {
	t.Parallel()
	tree, tools := readsFixture(t)
	link := filepath.Join(tree, "p/testdata/dangling")
	if err := os.Symlink("nowhere", link); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command("git", "-C", tree, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, output)
	}
	hash := func() string {
		reads, err := DeclaredReads(tree, tools, "p")
		if err != nil {
			t.Fatal(err)
		}
		value, err := ReadsHash(tree, reads)
		if err != nil {
			t.Fatalf("a dangling symlink in testdata: %v", err)
		}
		return value
	}
	base := hash()
	os.Remove(link)
	os.Symlink("elsewhere", link)
	if hash() == base {
		t.Error("retargeting the symlink left the reads part where it was")
	}
}
