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
	"github.com/system-inc/loom/protocol"
)

// Kirk's law (Oct 10): Workshop is the only builder, and every other machine downloads binaries and runs them, its
// disk holding bounded caches that clear themselves before they run out. A runner keeps every blob a prebuilt test
// unit reads from the action store (a test binary, a tree's source, a product's archive) in one content-addressed
// cache under its root, <root>/loom-blobs/<sha256>, and nothing else there is ever trusted:
//
//   - A fetch writes a partial file (.partial-<sha256>-<random>), hashing as it goes, and only a whole file that
//     hashes to its name is renamed to it, atomically, so a runner killed mid-fetch, a disk that filled mid-fetch,
//     or two runners fetching the same blob at once never leave a blob's name on anything but its whole bytes. A
//     partial no one has written for partialStale is a dead fetch's, and goes before the next unit.
//   - A blob read from the cache is hashed again before it is used: a copy corrupted on disk is removed and fetched
//     again, never run.
//   - Every use touches the blob's time, and the cache is trimmed to its bound least recently used first; before
//     every unit it is also trimmed until the disks the unit writes have the floor free. A disk still under the floor
//     with the cache empty is unfit, and the unit is refused as Loom's, never failed.

// DefaultBlobCacheBytes bounds a runner's blob cache. Measured Oct 10: adamic's source archive is 449 MB gzipped
// (98,381 files, 1.06 GB unpacked), a test binary about 8 MB gzipped (26 MB as built), and adamic has 74 test
// packages, so one tree's whole build is about 1.2 GB; 4 GiB holds three trees, a future and the mains it is stacked
// on, so a runner fetches a tree's source once, not once a unit.
const DefaultBlobCacheBytes int64 = 4 << 30

// DefaultFreeFloorBytes is the free room a prebuilt unit needs before it starts: the 1.06 GB its tree's source
// unpacks to, and the 1.5 GB a unit's tests have always been given after their checkout (prepare.sh's floor).
const DefaultFreeFloorBytes uint64 = 3 << 30

const blobDirectoryName = "loom-blobs"

// partialPrefix names a fetch in progress: never a sha256, so nothing reads it as a blob.
const partialPrefix = ".partial-"

// partialStale is how long a partial fetch may go unwritten before it is taken for a dead fetch's.
var partialStale = 15 * time.Minute

// errUnfit is a disk under the floor with nothing left to evict: the instance's, never the change's.
var errUnfit = errors.New("unfit")

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
}

// newBlobCache is the runner's blob cache under root, watching root's disk and the workspace's. options have their
// defaults.
func newBlobCache(options Options, root string) blobCache {
	return blobCache{directory: filepath.Join(root, blobDirectoryName), limit: options.BlobCacheBytes, floor: options.FreeFloorBytes,
		watched: []string{root, options.WorkspaceParent}, free: options.free, store: strings.TrimSuffix(options.Store, "/"), client: options.Client}
}

// A blobFetch is how one blob was had, for the unit's record.
type blobFetch struct {
	bytes   int64
	seconds float64
	cached  bool // read from the cache, not the store
	corrupt bool // the cache held a copy that didn't hash to its name, removed and fetched again
}

// blobLocks holds one lock per blob, so two units of one runner wanting the same blob fetch it once; two runners
// sharing a root each fetch their own partial, and the rename keeps the name whole either way.
var blobLocks sync.Map

func (cache blobCache) lock(sum string) func() {
	value, _ := blobLocks.LoadOrStore(cache.directory+"/"+sum, &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	mutex.Lock()
	return mutex.Unlock
}

// open returns blob sum open at its start, checked against its hash: the cache's copy when it holds it whole,
// otherwise fetched from the store. A blob the store doesn't hold is ErrNotStored.
func (cache blobCache) open(fetchContext context.Context, sum string) (*os.File, blobFetch, error) {
	if !protocol.Sha256Pattern.MatchString(sum) {
		return nil, blobFetch{}, fmt.Errorf("%q isn't a blob's sha256: the store is poisoned", sum)
	}
	unlock := cache.lock(sum)
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
	file, size, err := cache.fetch(fetchContext, sum)
	if err != nil {
		return nil, blobFetch{}, err
	}
	fetch.bytes, fetch.seconds = size, time.Since(started).Seconds()
	return file, fetch, nil
}

// fetch reads blob sum from the store into a partial file, hashing it as it comes, and renames it to the blob's
// name only when it is whole and hashes to it. It returns the file open at its start.
func (cache blobCache) fetch(fetchContext context.Context, sum string) (*os.File, int64, error) {
	if err := os.MkdirAll(cache.directory, 0o755); err != nil {
		return nil, 0, err
	}
	request, err := http.NewRequestWithContext(fetchContext, http.MethodGet, cache.store+"/blobs/"+sum, nil)
	if err != nil {
		return nil, 0, err
	}
	response, err := cache.client.Do(request)
	if err != nil {
		return nil, 0, fmt.Errorf("blobs/%s: %w", sum, err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return nil, 0, fmt.Errorf("blobs/%s: %w", sum, builder.ErrNotStored)
	case response.StatusCode != http.StatusOK:
		return nil, 0, fmt.Errorf("%s/blobs/%s answered %s", cache.store, sum, response.Status)
	}
	partial, err := os.CreateTemp(cache.directory, partialPrefix+sum+"-")
	if err != nil {
		return nil, 0, err
	}
	keep := false
	defer func() {
		if !keep {
			partial.Close()
			os.Remove(partial.Name())
		}
	}()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(partial, hash), response.Body)
	if errors.Is(err, syscall.ENOSPC) {
		return nil, 0, fmt.Errorf("blobs/%s: the disk filled while it was fetched (%d bytes in): %w", sum, size, err)
	}
	if err != nil {
		return nil, 0, fmt.Errorf("blobs/%s: %w", sum, err)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != sum {
		return nil, 0, fmt.Errorf("blob %s hashes to %s: the store is poisoned", sum, actual)
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

// A cachedBlob is one blob the cache holds.
type cachedBlob struct {
	sum  string
	size int64
	used time.Time
}

// blobs lists the cache's blobs, least recently used first, and removes each partial fetch no one has written for
// partialStale.
func (cache blobCache) blobs() ([]cachedBlob, error) {
	entries, err := os.ReadDir(cache.directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	blobs := []cachedBlob{}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			continue
		}
		switch {
		case strings.HasPrefix(entry.Name(), partialPrefix):
			if time.Since(info.ModTime()) >= partialStale {
				os.Remove(filepath.Join(cache.directory, entry.Name()))
			}
		case protocol.Sha256Pattern.MatchString(entry.Name()) && info.Mode().IsRegular():
			blobs = append(blobs, cachedBlob{sum: entry.Name(), size: info.Size(), used: info.ModTime()})
		}
	}
	sort.Slice(blobs, func(left, right int) bool { return blobs[left].used.Before(blobs[right].used) })
	return blobs, nil
}

// trim removes blobs, least recently used first, until the cache holds at most its limit, leaving those in keep.
func (cache blobCache) trim(keep map[string]bool) error {
	blobs, err := cache.blobs()
	if err != nil {
		return err
	}
	total := int64(0)
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
// watched disk has under the floor free, blobs removed least recently used first. A disk still under the floor with
// the cache empty is errUnfit, named.
func (cache blobCache) ready() error {
	if err := cache.trim(nil); err != nil {
		return err
	}
	short := func() (string, uint64, error) {
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
	blobs, err := cache.blobs()
	if err != nil {
		return err
	}
	for {
		path, available, err := short()
		if err != nil {
			return fmt.Errorf("reading the free room on %s: %w", path, err)
		}
		if path == "" {
			return nil
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
