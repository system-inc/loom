package resident

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A keys file is read back as written, and refused for another tree or in another format, so a build never takes
// another tree's keys for its own. Mutants: the tree's check, the format's check.
func TestKeysAreReadOnlyForTheirOwnTree(t *testing.T) {
	root, _, first := fixture(t)
	tree, err := New().Key(root, first)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "keys.json")
	if err := tree.WriteKeys(file); err != nil {
		t.Fatal(err)
	}
	keys, err := ReadKeys(file, tree.Hash)
	if err != nil {
		t.Fatal(err)
	}
	if keys.Commit != first || len(keys.Packages) != len(tree.Packages) || keys.Closures["example.com/resident/a"].Key != tree.Closures["example.com/resident/a"].Key {
		t.Fatalf("read back %+v", keys)
	}
	if _, err := ReadKeys(file, strings.Repeat("0", 40)); err == nil || !strings.Contains(err.Error(), "this tree is") {
		t.Fatalf("another tree's keys: %v", err)
	}
	keys.Format = KeysFormat + 1
	content, _ := json.Marshal(keys)
	if err := os.WriteFile(file, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadKeys(file, tree.Hash); err == nil || !strings.Contains(err.Error(), "format") {
		t.Fatalf("keys of another format: %v", err)
	}
}
