package resident

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/system-inc/loom/planner"
)

// KeysFormat is the shape of keys file this release writes and reads.
const KeysFormat = 1

// Keys are what the resident hands `loom build-tree --keys` for one tree, so the build in its child process reads the
// tree's keys instead of asking go and the disk for them again: its test packages (builder.TestPackages's answer),
// each one's closure key (planner.Closure's), and how the resident came by them. Builder's binaries by closure (lever
// 3 of #s0cqqhk) read Closures here when a build has them, and key cold when it doesn't.
type Keys struct {
	Format int    `json:"format"`
	Commit string `json:"commit"`
	// Tree is the commit's git tree hash: build-tree refuses keys whose tree isn't the one it builds.
	Tree     string                `json:"tree"`
	Packages []planner.ProductTest `json:"packages"`
	Closures map[string]Closure    `json:"closures"`
	// From is the warm tree the resident read this one against (empty: read whole), Changed the paths apart, Relisted
	// the packages whose closures go listed again, and Seconds what keying it took.
	From     string   `json:"from,omitempty"`
	Changed  []string `json:"changed"`
	Relisted []string `json:"relisted"`
	Seconds  float64  `json:"seconds"`
}

// Keys are the tree's keys as a keys file holds them.
func (tree *Tree) Keys() Keys {
	return Keys{Format: KeysFormat, Commit: tree.Commit, Tree: tree.Hash, Packages: tree.Packages, Closures: tree.Closures,
		From: tree.From, Changed: tree.Changed, Relisted: tree.Relisted, Seconds: tree.Seconds}
}

// WriteKeys writes the tree's keys to file, mode 600.
func (tree *Tree) WriteKeys(file string) error {
	content, err := json.Marshal(tree.Keys())
	if err != nil {
		return err
	}
	return os.WriteFile(file, content, 0o600)
}

// ReadKeys reads a keys file for the tree whose git tree hash is hash, refusing one of another format or another tree.
func ReadKeys(file, hash string) (Keys, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return Keys{}, err
	}
	var keys Keys
	if err := json.Unmarshal(content, &keys); err != nil {
		return Keys{}, fmt.Errorf("keys %s: %w", file, err)
	}
	if keys.Format != KeysFormat {
		return Keys{}, fmt.Errorf("keys %s are in format %d, and this release reads %d", file, keys.Format, KeysFormat)
	}
	if keys.Tree != hash {
		return Keys{}, fmt.Errorf("keys %s are tree %s's, and this tree is %s", file, keys.Tree, hash)
	}
	return keys, nil
}
