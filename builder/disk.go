package builder

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
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

// removingPrefix names a tree's directory on its way out: no tree hash, so TreeCache never takes it for a tree.
const removingPrefix = ".removing-"

// remove removes one file or empty directory; a variable so a test can stop a removal partway.
var remove = os.Remove

// RemoveTree removes one tree's working directory, base/<tree hash>, by name. It first renames the directory to
// base/.removing-<hash>, in one step, so a removal stopped partway (a kill, a full disk) never leaves a tree
// directory with some products' files gone and their directories still there, which buildcache would count as hits
// and PublishTree would archive hollow; TreeCache sweeps what such a removal left. Then every file and link goes, one
// at a time, then each directory once it is empty, deepest first. A link is removed as itself and never followed, and
// a directory that isn't a tree's under base is refused, so nothing outside the tree can go.
func RemoveTree(base, directory string) error {
	if filepath.Dir(filepath.Clean(directory)) != filepath.Clean(base) || !treeHashPattern.MatchString(filepath.Base(directory)) {
		return fmt.Errorf("%s isn't a tree's directory under %s", directory, base)
	}
	removing := filepath.Join(base, removingPrefix+filepath.Base(directory)+"-"+strconv.Itoa(os.Getpid()))
	if err := os.Rename(directory, removing); err != nil {
		return err
	}
	return removeRenamed(base, removing)
}

// SweepRemoving finishes every removal under base that stopped partway: each .removing- directory goes.
func SweepRemoving(base string) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), removingPrefix) {
			if err = removeRenamed(base, filepath.Join(base, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// removeRenamed removes a directory RemoveTree renamed, file by file, deepest directories last.
func removeRenamed(base, directory string) error {
	if filepath.Dir(filepath.Clean(directory)) != filepath.Clean(base) || !strings.HasPrefix(filepath.Base(directory), removingPrefix) {
		return fmt.Errorf("%s isn't a tree's directory being removed under %s", directory, base)
	}
	return removeFiles(directory)
}

// removeFiles removes directory and everything in it, each file and link by name, never following a link, then each
// directory once it is empty, deepest first.
func removeFiles(directory string) error {
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
		if err = remove(file); err != nil {
			return err
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err = remove(directories[index]); err != nil {
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

// recentlyUsed is how recently an entry of Go's build cache must have been used to be left alone by a trim: go
// marks an entry used by touching it at most hourly, so one used in the last day may be what a running go process is
// about to read.
const recentlyUsed = 24 * time.Hour

// TrimGoCache keeps Go's build cache under limit bytes: when its entries add up to more, the least recently used go
// first, oldest modification time first (go marks an entry used by touching it), until they are at three quarters
// of limit, so a trim isn't due again on the next build. Only cache entries are removed, never Go's own bookkeeping,
// and never one used in the last day, and go treats an entry whose action or output is gone as a miss. directory may
// be a link (GOCACHE often is), and "off" (GOCACHE=off) has nothing to trim. Two trims never run at once: each holds
// an exclusive lock on loom-trim.lock in the cache, and a trim that finds it held leaves the cache to the other. It
// returns the bytes removed.
func TrimGoCache(directory string, limit uint64) (uint64, error) {
	if directory == "off" || directory == "" {
		return 0, nil
	}
	directory, err := filepath.EvalSymlinks(directory)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	lock, err := os.OpenFile(filepath.Join(directory, "loom-trim.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return 0, nil
	}
	type entry struct {
		path     string
		size     uint64
		modified time.Time
	}
	entries := []entry{}
	total := uint64(0)
	err = filepath.WalkDir(directory, func(path string, found fs.DirEntry, err error) error {
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
		entries = append(entries, entry{path, uint64(info.Size()), info.ModTime()})
		total += uint64(info.Size())
		return nil
	})
	if err != nil || total <= limit {
		return 0, err
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].modified.Before(entries[right].modified) })
	target, removed := limit/4*3, uint64(0)
	for _, old := range entries {
		if total-removed <= target || time.Since(old.modified) < recentlyUsed {
			break
		}
		if err = os.Remove(old.path); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed += old.size
	}
	return removed, nil
}
