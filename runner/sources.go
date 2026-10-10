package runner

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/protocol"
)

// A tree's source unpacks to 98,381 files, 1.06 GB (adamic, Oct 10), which took 41 s on a Mac: far over the 30 s a
// unit has to be ready, so it isn't unpacked once a unit. A runner keeps each tree's source unpacked under its root,
// <root>/loom-sources/<source sha256>, for every unit of that tree, as the go test path keeps its checkout:
//
//   - A source is unpacked into .unpacking-<sha256>-<pid>-<random> and renamed to its name only once it is whole, so a
//     runner killed mid-unpack, a disk that filled, or an archive refused partway never leaves anything at a source's
//     name; a .unpacking- or .removing- directory whose process is gone is removed before the next unit.
//   - A unit holds its source with a shared lock on <sha256>.lock for as long as it runs; only a source no unit holds
//     is removed, renamed away first, so a removal stopped partway never leaves half a tree at its name.
//   - At most keepSources are kept, least recently used going first, and under the free floor every source no unit
//     holds goes before a unit is refused as unfit.
//
// Tests run in the shared tree, as they ran in the shared checkout: one that writes into its own source tree changes
// it for the later units of that tree.

const sourceDirectoryName = "loom-sources"

// keepSources is how many trees' sources a runner keeps unpacked: a future's and the main it stacks on.
var keepSources = 2

const unpackingPrefix = ".unpacking-"
const sourceRemovingPrefix = ".removing-"

// A sourceCache is a runner's unpacked sources, under its root.
type sourceCache struct {
	directory string
}

// A heldSource is one tree's source a unit holds: its directory, whether it is unpacked there yet, and the lock that
// keeps it until release.
type heldSource struct {
	sum       string
	directory string
	ready     bool
	lock      *os.File
}

func newSourceCache(root string) sourceCache {
	return sourceCache{directory: filepath.Join(root, sourceDirectoryName)}
}

// sourceLocks holds one lock per source, so two units of one runner unpack it once.
var sourceLocks sync.Map

// hold takes a shared hold on source sum, which keeps it from removal until release, and says whether it is already
// unpacked. A used source becomes the most recently used.
func (cache sourceCache) hold(sum string) (*heldSource, error) {
	if !protocol.Sha256Pattern.MatchString(sum) {
		return nil, fmt.Errorf("%q isn't a source's sha256", sum)
	}
	if err := os.MkdirAll(cache.directory, 0o755); err != nil {
		return nil, err
	}
	cache.sweep()
	lock, err := cache.lockFile(sum)
	if err != nil {
		return nil, err
	}
	held := &heldSource{sum: sum, directory: filepath.Join(cache.directory, sum), lock: lock}
	if info, err := os.Lstat(held.directory); err == nil && info.IsDir() {
		held.ready = true
		now := time.Now()
		os.Chtimes(held.directory, now, now)
	}
	return held, nil
}

// lockFile holds a shared lock on source sum's lock file, the one at its path once the lock is held: a removal unlinks
// the file it locked, so a lock taken on a file no longer at the path holds nothing, and is taken again.
func (cache sourceCache) lockFile(sum string) (*os.File, error) {
	path := filepath.Join(cache.directory, sum+".lock")
	for {
		lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			return nil, err
		}
		if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); err != nil {
			lock.Close()
			return nil, err
		}
		locked, lockedErr := lock.Stat()
		named, namedErr := os.Stat(path)
		if lockedErr == nil && namedErr == nil && os.SameFile(locked, named) {
			return lock, nil
		}
		lock.Close()
		if lockedErr != nil {
			return nil, lockedErr
		}
	}
}

// release lets the source go.
func (held *heldSource) release() {
	if held != nil && held.lock != nil {
		held.lock.Close()
		held.lock = nil
	}
}

// unpack unpacks archive into a directory of its own beside the source's name and renames it there only when the
// whole archive is in. A source another unit unpacked meanwhile is taken as it is.
func (cache sourceCache) unpack(held *heldSource, archive io.Reader) error {
	value, _ := sourceLocks.LoadOrStore(held.directory, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	defer mutex.Unlock()
	if info, err := os.Lstat(held.directory); err == nil && info.IsDir() {
		held.ready = true
		return nil
	}
	scratch, err := os.MkdirTemp(cache.directory, unpackingPrefix+held.sum+"-"+strconv.Itoa(os.Getpid())+"-")
	if err != nil {
		return err
	}
	if err = builder.Unpack(archive, scratch, nil); err != nil {
		os.RemoveAll(scratch)
		return err
	}
	if err = os.Rename(scratch, held.directory); err != nil {
		os.RemoveAll(scratch)
		if info, statErr := os.Lstat(held.directory); statErr == nil && info.IsDir() {
			held.ready = true
			return nil
		}
		return err
	}
	held.ready = true
	return nil
}

// sweep removes each .unpacking- and .removing- directory whose process is gone: what a killed runner left.
func (cache sourceCache) sweep() {
	entries, err := os.ReadDir(cache.directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		rest, unpacking := strings.CutPrefix(name, unpackingPrefix)
		if !unpacking {
			if rest, _ = strings.CutPrefix(name, sourceRemovingPrefix); rest == name {
				continue
			}
		}
		// <sha256>-<pid>-<random>
		fields := strings.Split(rest, "-")
		if len(fields) < 3 {
			continue
		}
		if pid, err := strconv.Atoi(fields[1]); err == nil && !processRunning(pid) {
			os.RemoveAll(filepath.Join(cache.directory, name))
		}
	}
}

// processRunning reports whether a process with this pid exists.
func processRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// trim removes sources no unit holds, least recently used first, until at most keep remain.
func (cache sourceCache) trim(keep int) {
	entries, err := os.ReadDir(cache.directory)
	if err != nil {
		return
	}
	type source struct {
		sum  string
		used time.Time
	}
	sources := []source{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.IsDir() || !protocol.Sha256Pattern.MatchString(entry.Name()) {
			continue
		}
		sources = append(sources, source{sum: entry.Name(), used: info.ModTime()})
	}
	sort.Slice(sources, func(left, right int) bool { return sources[left].used.After(sources[right].used) })
	for index, old := range sources {
		if index < keep {
			continue
		}
		cache.remove(old.sum)
	}
}

// remove removes one source unless a unit holds it: renamed away whole first, then emptied.
func (cache sourceCache) remove(sum string) {
	lockPath := filepath.Join(cache.directory, sum+".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return
	}
	// A lock file another removal unlinked meanwhile holds nothing: the source's holders lock the one at the path.
	if locked, err := lock.Stat(); err != nil {
		return
	} else if named, err := os.Stat(lockPath); err != nil || !os.SameFile(locked, named) {
		return
	}
	away, err := os.MkdirTemp(cache.directory, sourceRemovingPrefix+sum+"-"+strconv.Itoa(os.Getpid())+"-")
	if err != nil {
		return
	}
	if err = os.Rename(filepath.Join(cache.directory, sum), filepath.Join(away, sum)); err != nil {
		os.Remove(away)
		return
	}
	os.RemoveAll(away)
	os.Remove(lockPath)
}
