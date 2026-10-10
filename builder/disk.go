package builder

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
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
	return uint64(stat.Bavail) * blockSize(&stat), nil
}

// A Watch is a filesystem a build writes, by a path on it, and the free bytes it keeps.
type Watch struct {
	Path  string
	Floor uint64
}

// Gigabytes is n GB in bytes, refused when it doesn't fit.
func Gigabytes(n uint64) (uint64, error) {
	if n > math.MaxUint64/GB {
		return 0, fmt.Errorf("%d GB is more bytes than there are", n)
	}
	return n * GB, nil
}

// CheckFloor refuses when any watched filesystem, by name (the cache base, Go's build cache, the temporary
// directory), has under its floor free, naming which and how much it has. free nil means Free.
func CheckFloor(watched map[string]Watch, free func(path string) (uint64, error)) error {
	if free == nil {
		free = Free
	}
	names := make([]string, 0, len(watched))
	for name := range watched {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		watch := watched[name]
		available, err := free(watch.Path)
		if err != nil {
			return fmt.Errorf("%s (%s): %w", name, watch.Path, err)
		}
		if available < watch.Floor {
			return fmt.Errorf("%s (%s) has %.1f GB free, under its %d GB floor", name, watch.Path, float64(available)/float64(GB), watch.Floor/GB)
		}
	}
	return nil
}

// removingPrefix names a tree's directory on its way out: no tree hash, so TreeCache never takes it for a tree.
const removingPrefix = ".removing-"

// remove removes one file or empty directory; a variable so a test can stop a removal partway.
var remove = os.Remove

// ErrTreeInUse is a tree's directory another build-tree is building in, which is left alone.
var ErrTreeInUse = errors.New("another build holds the tree")

// A TreeLock is a build's hold on one tree: a shared flock on base/<tree hash>.lock for as long as the build runs.
// Builds of the same tree share it; a removal takes it exclusively and goes no further when it can't.
type TreeLock struct {
	file *os.File
}

// treeLockPath is the lock file beside a tree's directory.
func treeLockPath(directory string) string {
	return filepath.Clean(directory) + ".lock"
}

// LockTree holds the tree whose directory is directory for a build, until Close.
func LockTree(directory string) (*TreeLock, error) {
	file, err := os.OpenFile(treeLockPath(directory), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH); err != nil {
		file.Close()
		return nil, err
	}
	return &TreeLock{file: file}, nil
}

// Close lets the tree go.
func (lock *TreeLock) Close() error {
	return lock.file.Close()
}

// RemoveTree removes one tree's working directory, base/<tree hash>, by name, unless a build holds the tree
// (ErrTreeInUse). It first renames the directory to base/.removing-<hash>-<pid>, in one step, so a removal stopped
// partway (a kill, a full disk) never leaves a tree directory with some products' files gone and their directories
// still there, which buildcache would count as hits and PublishTree would archive hollow; SweepRemoving finishes what
// such a removal left. A name left by an earlier process with the same pid gets a fresh suffix. Then every file and
// link goes, one at a time, then each directory once it is empty, deepest first. A link is removed as itself and
// never followed, and a directory that isn't a tree's under base is refused, so nothing outside the tree can go.
func RemoveTree(base, directory string) error {
	if filepath.Dir(filepath.Clean(directory)) != filepath.Clean(base) || !treeHashPattern.MatchString(filepath.Base(directory)) {
		return fmt.Errorf("%s isn't a tree's directory under %s", directory, base)
	}
	lock, err := os.OpenFile(treeLockPath(directory), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return fmt.Errorf("%s: %w", directory, ErrTreeInUse)
	}
	stem := filepath.Join(base, removingPrefix+filepath.Base(directory)+"-"+strconv.Itoa(os.Getpid()))
	for attempt := 0; ; attempt++ {
		removing := stem
		if attempt > 0 {
			removing = stem + "." + strconv.Itoa(attempt)
		}
		err = os.Rename(directory, removing)
		if err == nil {
			return removeRenamed(base, removing)
		}
		if !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) || attempt >= 100 {
			return err
		}
	}
}

// removingPid is the pid a .removing- directory's name carries, .removing-<hash>-<pid>[.<n>], or 0.
func removingPid(name string) int {
	rest := strings.TrimPrefix(name, removingPrefix)
	dash := strings.LastIndex(rest, "-")
	if dash < 0 {
		return 0
	}
	digits, _, _ := strings.Cut(rest[dash+1:], ".")
	pid, err := strconv.Atoi(digits)
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

// running reports whether a process with pid is alive (signal 0 reaches it, or it exists and isn't ours to signal).
func running(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// SweepRemoving finishes every removal under base that stopped partway: each .removing- directory whose process
// is no longer running goes. One whose process still runs is that process's to finish.
func SweepRemoving(base string) error {
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), removingPrefix) {
			continue
		}
		if pid := removingPid(entry.Name()); pid > 0 && running(pid) {
			continue
		}
		if err = removeRenamed(base, filepath.Join(base, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Ready readies the cache base for a build: it first sweeps what killed removals left, which can be tens of GB, and
// only then checks the floors, so a stopped cleanup never keeps every later build from starting.
func Ready(base string, watched map[string]Watch, free func(path string) (uint64, error)) error {
	if err := SweepRemoving(base); err != nil {
		return err
	}
	return CheckFloor(watched, free)
}

// removeRenamed removes a directory RemoveTree renamed, file by file, deepest directories last.
func removeRenamed(base, directory string) error {
	if filepath.Dir(filepath.Clean(directory)) != filepath.Clean(base) || !strings.HasPrefix(filepath.Base(directory), removingPrefix) {
		return fmt.Errorf("%s isn't a tree's directory being removed under %s", directory, base)
	}
	return removeFiles(directory)
}

// removeFiles removes directory and everything in it, each file and link by name, never following a link, then each
// directory once it is empty, deepest first. Anything already gone is fine: another removal of the same directory
// (a sweep beside its owner) may have taken it first.
func removeFiles(directory string) error {
	files, directories := []string{}, []string{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
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
		if err = remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for index := len(directories) - 1; index >= 0; index-- {
		if err = remove(directories[index]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// TreeDone is a tree's working directory once the build is over: removed when its index went up, since everything a
// runner needs is in the store, and kept when it didn't, so a retry needn't build it all again (TreeCache keeps the
// newest few). The build lets its own hold on the tree go first; a tree another build still holds stays.
func TreeDone(base, directory string, published bool, lock *TreeLock) error {
	if lock != nil {
		lock.Close()
	}
	if !published {
		return nil
	}
	return RemoveTree(base, directory)
}

// goCacheEntry is a file of Go's build cache: an action or an output, <two hex>/<64 hex>-a or -d.
var goCacheEntry = regexp.MustCompile(`^[0-9a-f]{64}-[ad]$`)

// trimWalked, when set, runs between a trim's walk and its removals, so a test can use an entry in between.
var trimWalked func()

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
	// Only a directory go itself made is trimmed: a GOCACHE pointed somewhere else by mistake is refused whole.
	if readme, err := os.ReadFile(filepath.Join(directory, "README")); err != nil || !strings.Contains(string(readme), "Go build system") {
		return 0, fmt.Errorf("%s holds no Go build cache README, so it isn't trimmed", directory)
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
	if trimWalked != nil {
		trimWalked()
	}
	target, removed := limit/4*3, uint64(0)
	for _, old := range entries {
		if total-removed <= target || time.Since(old.modified) < recentlyUsed {
			break
		}
		// One go used since the walk is in use again: it stays.
		if info, err := os.Stat(old.path); err != nil || !info.ModTime().Equal(old.modified) {
			continue
		}
		if err = os.Remove(old.path); err != nil && !os.IsNotExist(err) {
			return removed, err
		}
		removed += old.size
	}
	return removed, nil
}
