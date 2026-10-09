package planner

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// closureModule writes a small module: a imports b, a's test imports t and embeds data.txt, c stands alone.
func closureModule(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for name, text := range map[string]string{
		"go.mod":      "module example.com/closure\n\ngo 1.22\n",
		"go.sum":      "",
		"go.work":     "go 1.22\n\nuse .\n",
		"a/a.go":      "package a\n\nimport \"example.com/closure/b\"\n\nvar Answer = b.Answer\n",
		"a/a_test.go": "package a\n\nimport (\n\t_ \"embed\"\n\t\"testing\"\n\n\t\"example.com/closure/t\"\n)\n\n//go:embed data.txt\nvar data string\n\nfunc TestA(test *testing.T) { _ = t.Helper }\n",
		"a/data.txt":  "one\n",
		"b/b.go":      "package b\n\nconst Answer = 1\n",
		"t/t.go":      "package t\n\nconst Helper = 1\n",
		"c/c.go":      "package c\n\nconst Unrelated = 1\n",
	} {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func closureOf(t *testing.T, root string) string {
	t.Helper()
	closure, err := Closure(root, "example.com/closure/a")
	if err != nil {
		t.Fatal(err)
	}
	return closure
}

// Every file the package's tests compile or embed moves the closure; a file outside it doesn't.
func TestClosureCoversImportsTestsAndEmbeds(t *testing.T) {
	t.Parallel()
	root := closureModule(t)
	base := closureOf(t, root)
	if base != closureOf(t, root) {
		t.Fatal("the closure isn't stable across two reads of the same tree")
	}
	for _, edit := range []struct {
		path  string
		moves bool
	}{
		{"b/b.go", true},      // an import
		{"a/a_test.go", true}, // a test file
		{"t/t.go", true},      // a test-only import
		{"a/data.txt", true},  // a file a test embeds
		{"c/c.go", false},     // outside the closure
		{"go.sum", true},      // the module's checksums
		{"go.work", true},     // the workspace the go command builds in
	} {
		path := filepath.Join(root, filepath.FromSlash(edit.path))
		original, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(append([]byte{}, original...), '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		moved := closureOf(t, root) != base
		if err := os.WriteFile(path, original, 0o644); err != nil {
			t.Fatal(err)
		}
		if moved != edit.moves {
			t.Errorf("editing %s: closure moved %v, want %v", edit.path, moved, edit.moves)
		}
	}
}

// The mutant Loom asked for: a go line moved with every source and dependency file identical moves every package's
// closure, so no verdict decided under the old go line is reused.
func TestAGoLineChangeMovesEveryClosure(t *testing.T) {
	t.Parallel()
	root := closureModule(t)
	packages := []string{"example.com/closure/a", "example.com/closure/b", "example.com/closure/c"}
	closures := func() map[string]string {
		result := map[string]string{}
		for _, importPath := range packages {
			closure, err := Closure(root, importPath)
			if err != nil {
				t.Fatal(err)
			}
			result[importPath] = closure
		}
		return result
	}
	base := closures()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/closure\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for importPath, closure := range closures() {
		if closure == base[importPath] {
			t.Errorf("the go line moved and %s kept its closure", importPath)
		}
	}
	files, err := ClosureFiles(root, "example.com/closure/c")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"c/c.go", "go.mod", "go.sum", "go.work"}; fmt.Sprint(files) != fmt.Sprint(want) {
		t.Errorf("c's closure files %v, want %v", files, want)
	}
}
