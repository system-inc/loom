// Command adamicprobe makes and reads products with adamic's own internal/buildcache, as a tree's build and a runner's
// unit do, for loom's tests of the layout buildcache keeps (builder/local_test.go). It is built inside a copy of
// adamic's module, at internal/buildcache/cmd/adamicprobe, since buildcache is internal to it.
//
//	adamicprobe make   builds two products untraced into ADAMIC_BUILD_CACHE_DIR: inner, and outer, whose bytes
//	                   name inner only as <build cache>/<inner's name key>, and resolves outer once
//	adamicprobe read   finds outer, building nothing, and reads inner through the name in its bytes (Absolute)
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/adamic/internal/buildcache"
)

var inner = buildcache.Inputs{Name: "loom probe inner", Files: []string{"inner.txt"}}
var outer = buildcache.Inputs{Name: "loom probe outer", Files: []string{"outer.txt"}}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "adamicprobe:", err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	if len(arguments) != 1 {
		return errors.New("usage: adamicprobe make|read")
	}
	switch arguments[0] {
	case "make":
		innerDirectory, err := buildcache.Get(inner, func(directory string) error {
			return os.WriteFile(filepath.Join(directory, "inner.txt"), []byte("the inner product\n"), 0o644)
		})
		if err != nil {
			return err
		}
		outerDirectory, err := buildcache.Get(outer, func(directory string) error {
			return os.WriteFile(filepath.Join(directory, "names"), []byte(buildcache.Relative(filepath.Join(innerDirectory, "inner.txt"))+"\n"), 0o644)
		})
		if err != nil {
			return err
		}
		if _, err = buildcache.Resolved(outerDirectory); err != nil {
			return err
		}
		fmt.Println(outerDirectory)
		return nil
	case "read":
		outerDirectory, err := buildcache.Get(outer, func(string) error {
			return errors.New("outer isn't in the cache, and a reader never builds it")
		})
		if err != nil {
			return err
		}
		names, err := os.ReadFile(filepath.Join(outerDirectory, "names"))
		if err != nil {
			return err
		}
		path := buildcache.Absolute(strings.TrimSpace(string(names)))
		content, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("outer names %s, read as %s: %w", strings.TrimSpace(string(names)), path, err)
		}
		fmt.Printf("%s\n%s", path, content)
		return nil
	}
	return fmt.Errorf("%q is make or read", arguments[0])
}
