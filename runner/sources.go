package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/protocol"
)

// A tree's source unpacks to 98,381 files, 1.06 GB (adamic, Oct 10), which takes 8.7 s on a Mac (41 s before Unpack
// kept its directories open) after a 23 s fetch: too much of the 30 s a unit has to be ready to spend on every unit,
// so it isn't unpacked once a unit. A runner keeps each tree's source unpacked under its root,
// <root>/loom-sources/<source sha256>, for every unit of that tree, as the go test path keeps its checkout. A tree's
// source is assembled from its chunks (assemble.go) and named by builder.SourceSum; its module cache is one archive,
// named by its blob's sha256:
//
//   - A source is unpacked into .unpacking-<sha256>-<random>, which its unpacker holds locked, gets its completion
//     marker (sourceMarker, holding its sha256), is synced once, and only then is renamed to its name. A directory at a
//     source's name without its marker (a crash that lost what wasn't on disk) is never trusted: it is removed and
//     unpacked again. A .unpacking- or .removing- directory whose lock no one holds is a dead runner's, swept before
//     the next unit.
//   - A unit holds its source with a shared lock on <sha256>.lock for as long as it runs; only a source no unit holds
//     is removed, renamed away first, so a removal stopped partway never leaves half a tree at its name.
//   - At most keepSources are kept, least recently used going first, and under the free floor every source no unit
//     holds goes before a unit is refused as unfit.
//
// Tests run in the shared tree, as they ran in the shared checkout: one that writes into its own source tree changes
// it for the later units of that tree.

const sourceDirectoryName = "loom-sources"

// keepSources is how many unpacked archives a runner keeps: a future's source and module cache, and the main's it
// stacks on.
var keepSources = 4

const unpackingPrefix = ".unpacking-"
const sourceRemovingPrefix = ".removing-"

// sourceMarker is the file a whole unpacked source holds at its top, naming its sha256.
const sourceMarker = ".loom-source"

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
// unpacked whole. A used source becomes the most recently used.
func (cache sourceCache) hold(sum string) (*heldSource, error) {
	if !protocol.Sha256Pattern.MatchString(sum) {
		return nil, fmt.Errorf("%q isn't a source's sha256", sum)
	}
	if err := os.MkdirAll(cache.directory, 0o755); err != nil {
		return nil, err
	}
	lock, err := cache.lockFile(sum)
	if err != nil {
		return nil, err
	}
	held := &heldSource{sum: sum, directory: filepath.Join(cache.directory, sum), lock: lock}
	if held.whole() {
		held.ready = true
		now := time.Now()
		os.Chtimes(held.directory, now, now)
	}
	return held, nil
}

// whole reports whether the source's directory holds its completion marker, naming it.
func (held *heldSource) whole() bool {
	content, err := os.ReadFile(filepath.Join(held.directory, sourceMarker))
	return err == nil && strings.TrimSpace(string(content)) == held.sum
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

// moduleCache is the GOMODCACHE the tests' go queries use for the module cache unpacked as sum: one per tree, kept and
// removed with it, and counted toward the blob cache's bound.
func (cache sourceCache) moduleCache(sum string) string {
	return filepath.Join(cache.directory, sum+moduleCacheSuffix)
}

// moduleCacheSuffix names a tree's GOMODCACHE beside its unpacked module cache.
const moduleCacheSuffix = "-modcache"

// release lets the source go.
func (held *heldSource) release() {
	if held != nil && held.lock != nil {
		held.lock.Close()
		held.lock = nil
	}
}

// A contextReader stops reading once its context ends, so an unpack is held to the unit's time.
type contextReader struct {
	readContext context.Context
	reader      io.Reader
}

func (reader contextReader) Read(buffer []byte) (int, error) {
	if err := reader.readContext.Err(); err != nil {
		return 0, err
	}
	return reader.reader.Read(buffer)
}

// unpack unpacks archive into a locked directory of its own beside the source's name, marks it whole, syncs once, and
// renames it there. A source another unit unpacked meanwhile is taken as it is; a directory at the name without its
// marker is removed first. unpackContext bounds the wait for another unit's unpack and the unpack itself.
func (cache sourceCache) unpack(unpackContext context.Context, held *heldSource, archive io.Reader) error {
	value, _ := sourceLocks.LoadOrStore(held.directory, make(chan struct{}, 1))
	slot := value.(chan struct{})
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-unpackContext.Done():
		return fmt.Errorf("waiting for another unit's unpack of it: %w", unpackContext.Err())
	}
	if held.whole() {
		held.ready = true
		return nil
	}
	if _, err := os.Lstat(held.directory); err == nil {
		// Again, at the last moment: another runner may have just put a whole one there.
		if held.whole() {
			held.ready = true
			return nil
		}
		if err := cache.moveAway(unpackContext, held.directory); err != nil {
			return fmt.Errorf("removing an unmarked source at %s: %w", held.directory, err)
		}
	}
	// Held while it unpacks: an unpacking whose lock is free is a dead runner's.
	lock, err := lockedTemporary(unpackContext, cache.directory, unpackingPrefix+held.sum+"-", true)
	if err != nil {
		return err
	}
	defer lock.Close()
	scratch := lock.Name()
	if err = builder.Unpack(contextReader{unpackContext, archive}, scratch, nil); err == nil {
		err = os.WriteFile(filepath.Join(scratch, sourceMarker), []byte(held.sum+"\n"), 0o444)
	}
	if err != nil {
		os.RemoveAll(scratch)
		return err
	}
	// Everything under the name is on disk before the name is.
	syscall.Sync()
	if err = os.Rename(scratch, held.directory); err != nil {
		os.RemoveAll(scratch)
		if held.whole() {
			held.ready = true
			return nil
		}
		return err
	}
	held.ready = true
	return nil
}

// moveAway renames directory to a locked .removing- directory and removes it, waiting no longer than waitContext.
func (cache sourceCache) moveAway(waitContext context.Context, directory string) error {
	lock, err := lockedTemporary(waitContext, cache.directory, sourceRemovingPrefix+filepath.Base(directory)+"-", true)
	if err != nil {
		return err
	}
	defer lock.Close()
	away := lock.Name()
	if err = os.Rename(directory, filepath.Join(away, "tree")); err != nil {
		os.Remove(away)
		return err
	}
	// A GOMODCACHE is read-only, as go leaves it.
	return removeDirectory(away)
}

// sweep removes each .unpacking- and .removing- directory whose lock no one holds: what a killed runner left.
func (cache sourceCache) sweep() {
	if _, err := os.Stat(cache.directory); err != nil {
		return
	}
	sweep, err := lockDirectory(cache.directory, syscall.LOCK_EX)
	if err != nil {
		return
	}
	defer sweep.Close()
	entries, err := os.ReadDir(cache.directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasPrefix(name, unpackingPrefix) && !strings.HasPrefix(name, sourceRemovingPrefix) {
			continue
		}
		path := filepath.Join(cache.directory, name)
		if lockFree(path) {
			// Once, and with what a GOMODCACHE made read-only made writable first: what still won't go stays, counted
			// toward the blob cache's bound (keptBytes), for the next sweep, never retried here under the lock.
			removeDirectory(path)
		}
	}
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

// removalWait bounds a removal's wait for the sources' sweep lock: a trim belongs to no unit's time.
var removalWait = time.Minute

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
	waitContext, cancel := context.WithTimeout(context.Background(), removalWait)
	defer cancel()
	// Its state first, so no state outlives the tree it describes.
	if err := os.Remove(cache.statePath(sum)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	if cache.moveAway(waitContext, filepath.Join(cache.directory, sum)) == nil {
		if _, err := os.Lstat(cache.moduleCache(sum)); err == nil {
			cache.moveAway(waitContext, cache.moduleCache(sum))
		}
		os.Remove(lockPath)
	}
}
