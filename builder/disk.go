package builder

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
)

// Workshop is the house's only builder, so it keeps its disk (Kirk, #ckv0pmg): at least 200 GB free on every
// filesystem a build writes, its Go build cache under a cap, and each tree's working directory gone once the tree is
// in the store.

// GB is a gigabyte, as the floor and the cap are given.
const GB = uint64(1) << 30

// Free is how many bytes the filesystem holding path has free for this user.
func Free(path string) (uint64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Bavail) * uint64(stat.Bsize), nil
}

// CheckFloor refuses when any of paths, by name (a cache base, Go's build cache, the temporary directory), sits on a
// filesystem with under floor bytes free, naming which and how much it has. free nil means Free.
func CheckFloor(paths map[string]string, floor uint64, free func(path string) (uint64, error)) error {
	if free == nil {
		free = Free
	}
	names := make([]string, 0, len(paths))
	for name := range paths {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		available, err := free(paths[name])
		if err != nil {
			return fmt.Errorf("%s (%s): %w", name, paths[name], err)
		}
		if available < floor {
			return fmt.Errorf("%s (%s) has %.1f GB free, under the %d GB floor", name, paths[name], float64(available)/float64(GB), floor/GB)
		}
	}
	return nil
}

// RemoveTree removes one tree's working directory, base/<tree hash>, by name: every file and link it holds, one at a
// time, then each directory once it is empty, deepest first. A link is removed as itself and never followed, and a
// directory that isn't a tree's under base is refused, so nothing outside the tree can go.
func RemoveTree(base, directory string) error {
	if filepath.Dir(filepath.Clean(directory)) != filepath.Clean(base) || !treeHashPattern.MatchString(filepath.Base(directory)) {
		return fmt.Errorf("%s isn't a tree's directory under %s", directory, base)
	}
	files, directories := []string{}, []string{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			directories = append(directories, path)
		} else {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, file := range files {
		if err = os.Remove(file); err != nil {
			return err
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err = os.Remove(directories[index]); err != nil {
			return err
		}
	}
	return nil
}

// TreeDone is a tree's working directory once the build is over: removed when its index went up, since everything a
// runner needs is in the store, and kept when it didn't, so a retry needn't build it all again (TreeCache keeps the
// newest few).
func TreeDone(base, directory string, published bool) error {
	if !published {
		return nil
	}
	return RemoveTree(base, directory)
}

// goCacheEntry is a file of Go's build cache: an action or an output, <two hex>/<64 hex>-a or -d.
var goCacheEntry = regexp.MustCompile(`^[0-9a-f]{64}-[ad]$`)

// TrimGoCache keeps Go's build cache under limit bytes: when its entries add up to more, the least recently used go
// first, oldest modification time first (go marks an entry used by touching it), until they are at three quarters
// of limit, so a trim isn't due again on the next build. Only cache entries are removed, never Go's own bookkeeping,
// and go treats an entry whose action or output is gone as a miss. It returns the bytes removed.
func TrimGoCache(directory string, limit uint64) (uint64, error) {
	type entry struct {
		path     string
		size     uint64
		modified int64
	}
	entries := []entry{}
	total := uint64(0)
	err := filepath.WalkDir(directory, func(path string, found fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if found.IsDir() || !found.Type().IsRegular() || !goCacheEntry.MatchString(found.Name()) {
			return nil
		}
		info, err := found.Info()
		if err != nil {
			return err
		}
		entries = append(entries, entry{path, uint64(info.Size()), info.ModTime().UnixNano()})
		total += uint64(info.Size())
		return nil
	})
	if err != nil || total <= limit {
		return 0, err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].modified < entries[right].modified })
	target, removed := limit/4*3, uint64(0)
	for _, old := range entries {
		if total-removed <= target {
			break
		}
		if err = os.Remove(old.path); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed += old.size
	}
	return removed, nil
}
