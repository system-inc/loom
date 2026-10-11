package main

import (
	"errors"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/resident"
)

// binaryClosures is how build-tree reads each test package's closure for its binary key: from the resident's keys
// when they came with the build (each Closures[package].Key is planner.Closure's on the same tree, read warm), and
// cold from the tree for a package they don't name. A closure the resident couldn't list is that package's error, so
// it compiles unkeyed, as a cold listing that failed would. No keys: nil, every closure cold.
func binaryClosures(keys *resident.Keys) builder.ClosureFunc {
	if keys == nil {
		return nil
	}
	return func(tree, importPath string) (string, error) {
		closure, named := keys.Closures[importPath]
		switch {
		case !named:
			return planner.Closure(tree, importPath)
		case closure.Error != "":
			return "", errors.New(closure.Error)
		}
		return closure.Key, nil
	}
}
