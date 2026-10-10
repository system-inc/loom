package runner

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/housecache"
	"github.com/system-inc/loom/protocol"
)

// Kirk's law (Oct 10): Workshop is the only builder, and every other machine downloads binaries and runs them, its
// disk holding bounded caches that clear themselves before they run out. A runner keeps every blob a prebuilt test
// unit reads from the action store (a test binary, a tree's source, a product's archive) in one content-addressed
// cache under its root, <root>/loom-blobs/<sha256>, and nothing else there is ever trusted:
//
//   - A fetch writes a partial file (.partial-<sha256>-<random>), holding an exclusive lock on it, hashing as it goes,
//     and only a whole file that hashes to its name is made read-only (0444) and renamed to it, atomically, so a
//     runner killed mid-fetch, a disk that filled mid-fetch, or two runners fetching the same blob at once never leave
//     a blob's name on anything but its whole bytes. A partial whose lock no one holds is a dead fetch's and goes
//     before the next unit; a live one counts toward the cache's size.
//   - A blob read from the cache is hashed again before it is used: a copy corrupted on disk is removed and fetched
//     again, never run.
//   - Every use touches the blob's time, and the cache is trimmed to its bound least recently used first; before
//     every unit, while the cache's own disk has under the floor free, it is trimmed further. A disk still under the
//     floor with the cache empty, or a short disk the cache isn't on, is unfit, and the unit is refused as Loom's.
//     Removing a blob another unit holds open takes only its name: that unit reads its whole bytes, and the room comes
//     back when it closes it.

// DefaultBlobCacheBytes bounds a runner's blob cache. Measured Oct 10: adamic's source is 450 MB gzipped in 836 chunks
// (98,459 files, 1.06 GB unpacked), a test binary about 8 MB gzipped (26 MB as built), and adamic has 74 test
// packages, so one tree's whole build is about 1.2 GB; 4 GiB holds three trees, a future and the mains it is stacked
// on. A tree assembled is kept whole beside the cache, so its chunks may go and a tree near it still needs only the
// chunks it changed.
const DefaultBlobCacheBytes int64 = 4 << 30

// DefaultFreeFloorBytes is the free room a prebuilt unit needs before it starts: the 1.06 GB its tree's source
// unpacks to, and the 1.5 GB a unit's tests have always been given after their checkout (prepare.sh's floor).
const DefaultFreeFloorBytes uint64 = 3 << 30

const blobDirectoryName = "loom-blobs"

// partialPrefix names a fetch in progress: never a sha256, so nothing reads it as a blob.
const partialPrefix = ".partial-"

// errUnfit is a disk under the floor with nothing left to evict: the instance's, never the change's.
var errUnfit = errors.New("unfit")

// errUnfitElsewhere is a short disk the cache isn't on, which nothing the runner keeps can free.
var errUnfitElsewhere = fmt.Errorf("%w on a disk the runner's caches aren't on", errUnfit)

// copyBlob copies a fetch's body into its partial file; a test plants a full disk through it.
var copyBlob = io.Copy

// A blobCache is a runner's one cache of the action store's blobs. Watched are the paths whose filesystems a unit
// writes (the cache's own and the workspace's), each kept at floor free.
type blobCache struct {
	directory string
	limit     int64
	floor     uint64
	watched   []string
	free      func(path string) (uint64, error)
	store     string
	client    *http.Client
	// house is the house cache's address (docs/house-cache.md), asked first for every blob through houseClient; empty
	// when there is none.
	house       string
	houseClient *http.Client
}

// newBlobCache is the runner's blob cache under root, watching root's disk and the workspace's. options have their
// defaults.
func newBlobCache(options Options, root string) blobCache {
	return blobCache{directory: filepath.Join(root, blobDirectoryName), limit: options.BlobCacheBytes, floor: options.FreeFloorBytes,
		watched: []string{root, options.WorkspaceParent}, free: options.free, store: strings.TrimSuffix(options.Store, "/"), client: options.Client,
		house: options.HouseCache, houseClient: options.houseClient}
}

// readyRoot readies a runner's root for a prebuilt unit: dead unpackings swept, the blob cache readied, and when its
// disk is still short, every unpacked source no unit holds removed before the unit is refused as unfit.
func readyRoot(options Options, root string) error {
	sources := newSourceCache(root)
	sources.sweep()
	cache := newBlobCache(options, root)
	err := cache.ready()
	if errors.Is(err, errUnfit) && !errors.Is(err, errUnfitElsewhere) {
		sources.trim(0)
		err = cache.ready()
	}
	return err
}

// A blobFetch is how one blob was had, for the unit's record.
type blobFetch struct {
	bytes   int64
	seconds float64
	cached  bool // read from the cache, not the store
	corrupt bool // the cache held a copy that didn't hash to its name, removed and fetched again
	house   bool // fetched from the house cache
	// unhoused is why the house cache didn't give the blob, which then came from the store; empty when it did, or
	// when there is none.
	unhoused string
}

// blobLocks holds one lock per blob, so two units of one runner wanting the same blob fetch it once; two runners
// sharing a root each fetch their own partial, and the rename keeps the name whole either way.
var blobLocks sync.Map

// lock waits for blob sum's lock until waitContext ends, so a fetch stalled in another unit holds no unit past its
// own time.
func (cache blobCache) lock(waitContext context.Context, sum string) (func(), error) {
	value, _ := blobLocks.LoadOrStore(cache.directory+"/"+sum, make(chan struct{}, 1))
	slot := value.(chan struct{})
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-waitContext.Done():
		return nil, fmt.Errorf("blobs/%s: waiting for another unit's fetch of it: %w", sum, waitContext.Err())
	}
}

// open returns blob sum open at its start, checked against its hash: the cache's copy when it holds it whole,
// otherwise fetched from the store. A blob the store doesn't hold is ErrNotStored. fetchContext bounds the wait and
// the fetch.
func (cache blobCache) open(fetchContext context.Context, sum string) (*os.File, blobFetch, error) {
	if !protocol.Sha256Pattern.MatchString(sum) {
		return nil, blobFetch{}, fmt.Errorf("%q isn't a blob's sha256: the store is poisoned", sum)
	}
	unlock, err := cache.lock(fetchContext, sum)
	if err != nil {
		return nil, blobFetch{}, err
	}
	defer unlock()
	started := time.Now()
	path := filepath.Join(cache.directory, sum)
	fetch := blobFetch{}
	if file, err := os.Open(path); err == nil {
		hash := sha256.New()
		size, err := io.Copy(hash, file)
		if err == nil && hex.EncodeToString(hash.Sum(nil)) == sum {
			if _, err = file.Seek(0, io.SeekStart); err == nil {
				now := time.Now()
				os.Chtimes(path, now, now)
				return file, blobFetch{bytes: size, seconds: time.Since(started).Seconds(), cached: true}, nil
			}
		}
		file.Close()
		// A copy that doesn't hash to its name (a disk that lost it, a crash before it was synced) is never used.
		os.Remove(path)
		fetch.corrupt = true
	}
	file, size, err := cache.fetch(fetchContext, sum, &fetch)
	if err != nil {
		return nil, blobFetch{}, err
	}
	fetch.bytes, fetch.seconds = size, time.Since(started).Seconds()
	return file, fetch, nil
}

// fetch has blob sum from the house cache when there is one, and otherwise, or when the house cache can't give it
// whole and hashing to its name (down, slow, missing it, corrupt), from the store. Either way it is checked the same.
func (cache blobCache) fetch(fetchContext context.Context, sum string, fetch *blobFetch) (*os.File, int64, error) {
	if through := housecache.Through(cache.house, cache.store+"/blobs/"+sum); through != "" {
		file, size, err := cache.fetchFrom(fetchContext, cache.houseClient, through, sum)
		if err == nil {
			fetch.house = true
			return file, size, nil
		}
		// A unit out of time, or a disk that filled, is no better from the store.
		if fetchContext.Err() != nil || errors.Is(err, syscall.ENOSPC) {
			return nil, 0, err
		}
		fetch.unhoused = err.Error()
	}
	return cache.fetchFrom(fetchContext, cache.client, cache.store+"/blobs/"+sum, sum)
}

// fetchFrom reads blob sum from url into a partial file it holds locked, hashing it as it comes, and renames it to the
// blob's name, read-only, only when it is whole and hashes to it. It returns the file open at its start.
func (cache blobCache) fetchFrom(fetchContext context.Context, client *http.Client, url, sum string) (*os.File, int64, error) {
	if err := os.MkdirAll(cache.directory, 0o755); err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(fetchContext, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, err
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("blobs/%s: %w", sum, err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, 0, fmt.Errorf("blobs/%s: %w", sum, builder.ErrNotStored)
	case response.StatusCode != http.StatusOK:
		return nil, 0, fmt.Errorf("%s answered %s", url, response.Status)
	}
	partial, err := lockedTemporary(fetchContext, cache.directory, partialPrefix+sum+"-", false)
	if err != nil {
		return nil, 0, err
	}
	keep := false
	defer func() {
		if !keep {
			os.Remove(partial.Name())
			partial.Close()
		}
	}()
	hash := sha256.New()
	size, err := copyBlob(io.MultiWriter(partial, hash), response.Body)
	if errors.Is(err, syscall.ENOSPC) {
		return nil, 0, fmt.Errorf("blobs/%s: the disk filled while it was fetched (%d bytes in): %w", sum, size, err)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("blobs/%s: %w", sum, err)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != sum {
		return nil, 0, fmt.Errorf("blob %s hashes to %s: the store is poisoned", sum, actual)
	}
	if err = partial.Chmod(0o444); err != nil {
		return nil, 0, err
	}
	if err = partial.Sync(); err != nil {
		return nil, 0, fmt.Errorf("blobs/%s: %w", sum, err)
	}
	if err = os.Rename(partial.Name(), filepath.Join(cache.directory, sum)); err != nil {
		return nil, 0, err
	}
	if _, err = partial.Seek(0, io.SeekStart); err != nil {
		return nil, 0, err
	}
	keep = true
	return partial, size, nil
}

// keptBytes is what the cache's bound counts beside the blobs: the trees' GOMODCACHEs beside the unpacked sources,
// and every unpacking or removal there, a live one's or one a sweep couldn't remove.
func (cache blobCache) keptBytes() int64 {
	sources := filepath.Join(filepath.Dir(cache.directory), sourceDirectoryName)
	kept := []string{}
	for _, pattern := range []string{"*" + moduleCacheSuffix, unpackingPrefix + "*", sourceRemovingPrefix + "*"} {
		matched, _ := filepath.Glob(filepath.Join(sources, pattern))
		kept = append(kept, matched...)
	}
	total := int64(0)
	for _, directory := range kept {
		filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
			if err == nil && entry.Type().IsRegular() {
				if info, err := entry.Info(); err == nil {
					total += info.Size()
				}
			}
			return nil
		})
	}
	return total
}

// A cachedBlob is one blob the cache holds.
type cachedBlob struct {
	sum  string
	size int64
	used time.Time
}

// blobs lists the cache's blobs, least recently used first, and the bytes of fetches in flight; each partial whose
// lock no one holds, a dead fetch's, is removed.
func (cache blobCache) blobs() ([]cachedBlob, int64, error) {
	if _, err := os.Stat(cache.directory); errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	sweep, err := lockDirectory(cache.directory, syscall.LOCK_EX)
	if err != nil {
		return nil, 0, err
	}
	defer sweep.Close()
	entries, err := os.ReadDir(cache.directory)
	if err != nil {
		return nil, 0, err
	}
	blobs := []cachedBlob{}
	inFlight := int64(0)
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(entry.Name(), partialPrefix):
			path := filepath.Join(cache.directory, entry.Name())
			if lockFree(path) {
				os.Remove(path)
			} else {
				inFlight += info.Size()
			}
		case protocol.Sha256Pattern.MatchString(entry.Name()) && info.Mode().IsRegular():
			blobs = append(blobs, cachedBlob{sum: entry.Name(), size: info.Size(), used: info.ModTime()})
		}
	}
	sort.Slice(blobs, func(left, right int) bool { return blobs[left].used.Before(blobs[right].used) })
	return blobs, inFlight, nil
}

// sweepLockName is a directory's sweep lock: its sweeper holds it exclusively, and whoever makes an in-progress name
// there holds it shared until that name's own lock is held, so a sweep never sees a name before its lock.
const sweepLockName = ".sweep.lock"

// lockDirectory holds directory's sweep lock, shared or exclusive (syscall.LOCK_SH or LOCK_EX), until Close.
func lockDirectory(directory string, how int) (*os.File, error) {
	lock, err := os.OpenFile(filepath.Join(directory, sweepLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), how); err != nil {
		lock.Close()
		return nil, err
	}
	return lock, nil
}

// waitForLock takes a lock on file (syscall.LOCK_SH or LOCK_EX), waiting no longer than waitContext.
func waitForLock(waitContext context.Context, file *os.File, how int) error {
	for {
		err := syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		select {
		case <-waitContext.Done():
			return fmt.Errorf("waiting for %s: %w", file.Name(), waitContext.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// lockedTemporary makes an in-progress file (or, with isDirectory, a directory) in directory named pattern plus a
// random suffix, and returns it open with an exclusive lock held for as long as it stays open: a fetch's partial, a
// source's unpacking, a removal. Its name appears only under the directory's shared sweep lock, waited for no longer
// than waitContext.
func lockedTemporary(waitContext context.Context, directory, pattern string, isDirectory bool) (*os.File, error) {
	sweep, err := os.OpenFile(filepath.Join(directory, sweepLockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	defer sweep.Close()
	if err = waitForLock(waitContext, sweep, syscall.LOCK_SH); err != nil {
		return nil, err
	}
	defer sweep.Close()
	var made *os.File
	if isDirectory {
		name, err := os.MkdirTemp(directory, pattern)
		if err != nil {
			return nil, err
		}
		if made, err = os.Open(name); err != nil {
			os.Remove(name)
			return nil, err
		}
	} else if made, err = os.CreateTemp(directory, pattern); err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(made.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		os.Remove(made.Name())
		made.Close()
		return nil, err
	}
	return made, nil
}

// lockFree reports whether no one holds a lock on the file or directory at path.
func lockFree(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil
}

// trim removes blobs, least recently used first, until the cache, fetches in flight counted, holds at most its limit,
// leaving those in keep.
func (cache blobCache) trim(keep map[string]bool) error {
	blobs, inFlight, err := cache.blobs()
	if err != nil {
		return err
	}
	total := inFlight + cache.keptBytes()
	for _, blob := range blobs {
		total += blob.size
	}
	for _, blob := range blobs {
		if total <= cache.limit {
			break
		}
		if keep[blob.sum] {
			continue
		}
		if err := os.Remove(filepath.Join(cache.directory, blob.sum)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total -= blob.size
	}
	return nil
}

// ready readies the cache for a unit: dead partial fetches gone, the cache trimmed to its limit, and then, while a
// watched disk has under the floor free, blobs removed least recently used first, but only when that disk is the
// cache's own, since removing them frees nothing elsewhere. A disk still short is errUnfit, named.
func (cache blobCache) ready() error {
	if err := cache.trim(nil); err != nil {
		return err
	}
	own, ownErr := deviceOf(nearestDirectory(cache.directory))
	blobs, _, err := cache.blobs()
	if err != nil {
		return err
	}
	for {
		path, available, err := cache.short()
		if err != nil {
			return fmt.Errorf("reading the free room on %s: %w", path, err)
		}
		if path == "" {
			return nil
		}
		device, deviceErr := deviceOf(nearestDirectory(path))
		if ownErr != nil || deviceErr != nil || device != own {
			return fmt.Errorf("%w: %s has %.2f GB free, under the runner's %.2f GB floor: the instance's, never the change's",
				errUnfitElsewhere, path, float64(available)/float64(builder.GB), float64(cache.floor)/float64(builder.GB))
		}
		if len(blobs) == 0 {
			return fmt.Errorf("%w: %s has %.2f GB free with the blob cache empty, under the runner's %.2f GB floor: the instance's, never the change's",
				errUnfit, path, float64(available)/float64(builder.GB), float64(cache.floor)/float64(builder.GB))
		}
		if err := os.Remove(filepath.Join(cache.directory, blobs[0].sum)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		blobs = blobs[1:]
	}
}

// short is the first watched path whose disk has under the floor free, and how much it has; "" when none.
func (cache blobCache) short() (string, uint64, error) {
	for _, path := range cache.watched {
		available, err := cache.free(nearestDirectory(path))
		if err != nil {
			return path, 0, err
		}
		if available < cache.floor {
			return path, available, nil
		}
	}
	return "", 0, nil
}

// deviceOf is the filesystem holding path.
func deviceOf(path string) (uint64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: no device", path)
	}
	return uint64(stat.Dev), nil
}

// nearestDirectory is path, or its nearest existing parent: a fresh instance makes its root and workspace only with
// its first unit.
func nearestDirectory(path string) string {
	for {
		if _, err := os.Stat(path); err == nil || filepath.Dir(path) == path {
			return path
		}
		path = filepath.Dir(path)
	}
}
