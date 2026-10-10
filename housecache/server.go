package housecache

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
	"sync/atomic"
	"syscall"
	"time"
)

// A Server is the house cache: blobs on its disk under Directory, each named by its sha256, filled from Upstream.
//
//   - A hit is served from disk with its length, and hashed as it goes out: a copy the disk corrupted is removed once
//     it is found, and the client, which checks every hash, reads that blob from the store this once.
//   - A miss fetches the blob from Upstream once, however many clients ask for it meanwhile: they all wait for that one
//     fetch. It goes into a partial file, hashed as it comes, and only a whole blob that hashes to its name is made
//     read-only and renamed to it, so a name only ever holds the whole, checked blob. One that doesn't hash to its
//     name is refused, nothing kept, and every waiting client is told so and reads from the store.
//   - The disk is bounded: before a blob is fetched, and after, the least recently served blobs go until the cache
//     holds at most Limit bytes, fetches in flight counted, and its disk keeps Floor bytes free. A blob that can't fit
//     is never fetched, and its clients read from the store.
//
// A Server serves only by-hash paths (ByHash); anything else is 404 and never reaches Upstream.
type Server struct {
	Directory string
	Upstream  string
	Limit     int64
	Floor     uint64
	// Free reads the free bytes of the filesystem holding a path (builder.Free).
	Free func(path string) (uint64, error)
	// Client fetches from Upstream; nil means one bounded by FetchBound.
	Client *http.Client
	// Report gets one line per miss, refusal, removal and eviction; nil discards them.
	Report io.Writer
	// Stall is how long a fetch from Upstream may go without a byte before it is abandoned. Zero means a minute.
	Stall time.Duration
	// Now is the clock a served blob's use is stamped with; nil means time.Now.
	Now func() time.Time

	// Fetches counts the fetches from Upstream.
	Fetches atomic.Int64

	mutex   sync.Mutex
	flights map[string]*flight
	// room is held while room is made; reserved, under it, is the length each fetch in flight said its blob has, by
	// its partial's name, so two fetches at once never count on the same free bytes.
	room     sync.Mutex
	reserved map[string]int64
	lock     *os.File
}

// FetchBound bounds one fetch from Upstream whole.
const FetchBound = 30 * time.Minute

// partialPrefix names a fetch in progress: never a sha256, so nothing serves it.
const partialPrefix = ".partial-"

// lockName is the directory's lock, held by its one server for as long as it runs.
const lockName = ".lock"

// A flight is one fetch from Upstream that every client asking for its blob meanwhile waits for.
type flight struct {
	done chan struct{}
	err  error
}

// errNotUpstream is a blob the store doesn't hold.
var errNotUpstream = errors.New("the store doesn't hold it")

// errNoRoom is a blob the cache can't keep within its bound and its floor.
var errNoRoom = errors.New("no room")

// Open readies the server's directory: made if missing, locked against a second server on it, and every partial a
// killed server left removed.
func (server *Server) Open() error {
	if server.Free == nil {
		return errors.New("the house cache reads its disk's free room with Free, and none is set")
	}
	if err := os.MkdirAll(server.Directory, 0o755); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(server.Directory, lockName), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return fmt.Errorf("%s: another house cache serves this directory: %w", server.Directory, err)
	}
	server.lock = lock
	entries, err := os.ReadDir(server.Directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), partialPrefix) {
			os.Remove(filepath.Join(server.Directory, entry.Name()))
		}
	}
	return server.makeRoom("", "", 0)
}

// Close releases the directory's lock.
func (server *Server) Close() error {
	if server.lock == nil {
		return nil
	}
	return server.lock.Close()
}

func (server *Server) now() time.Time {
	if server.Now != nil {
		return server.Now()
	}
	return time.Now()
}

func (server *Server) say(format string, arguments ...any) {
	if server.Report != nil {
		fmt.Fprintf(server.Report, "loom house-cache: "+format+"\n", arguments...)
	}
}

func (server *Server) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet && request.Method != http.MethodHead {
		writer.Header().Set("Allow", "GET, HEAD")
		http.Error(writer, "the house cache only serves blobs", http.StatusMethodNotAllowed)
		return
	}
	sum, ok := ByHash(request.URL.Path)
	if !ok || request.URL.RawQuery != "" {
		http.Error(writer, "the house cache serves only blobs named by their sha256, /blobs/<sha256> and /releases/blobs/<sha256>; read everything else from the store", http.StatusNotFound)
		return
	}
	started := time.Now()
	path := filepath.Join(server.Directory, sum)
	file, err := os.Open(path)
	how := "hit"
	if err != nil {
		how = "miss"
		if err = server.fill(request.Context(), request.URL.Path, sum); err == nil {
			file, err = os.Open(path)
		}
	}
	if err != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(err, errNotUpstream):
			status = http.StatusNotFound
		case errors.Is(err, errNoRoom):
			status = http.StatusServiceUnavailable
		case request.Context().Err() != nil:
			return
		}
		server.say("%s %s: %v", how, request.URL.Path, err)
		http.Error(writer, err.Error(), status)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(writer, err.Error(), http.StatusInternalServerError)
		return
	}
	now := server.now()
	os.Chtimes(path, now, now)
	writer.Header().Set("Content-Type", "application/octet-stream")
	writer.Header().Set("Content-Length", fmt.Sprint(info.Size()))
	writer.Header().Set("X-Loom-House-Cache", how)
	if request.Method == http.MethodHead {
		return
	}
	hash := sha256.New()
	sent, err := io.Copy(writer, io.TeeReader(file, hash))
	if err != nil || sent != info.Size() {
		return
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != sum {
		// Its client refuses what it got and reads from the store; the next ask fetches it again.
		os.Remove(path)
		server.say("removed blob %s: on disk it hashes to %s", sum, actual)
		return
	}
	if how == "miss" {
		server.say("served %s, %d bytes, fetched from the store in %.2f s", request.URL.Path, info.Size(), time.Since(started).Seconds())
	}
}

// fill has the blob on disk: one fetch from Upstream, which every client asking for it meanwhile waits for. A waiting
// client that goes away leaves the fetch running for the others.
func (server *Server) fill(waitContext context.Context, path, sum string) error {
	server.mutex.Lock()
	if _, err := os.Stat(filepath.Join(server.Directory, sum)); err == nil {
		// A fetch that finished since this client looked.
		server.mutex.Unlock()
		return nil
	}
	if server.flights == nil {
		server.flights = map[string]*flight{}
	}
	fetching := server.flights[sum]
	if fetching == nil {
		fetching = &flight{done: make(chan struct{})}
		server.flights[sum] = fetching
		go func() {
			fetching.err = server.fetch(path, sum)
			server.mutex.Lock()
			delete(server.flights, sum)
			server.mutex.Unlock()
			close(fetching.done)
		}()
	}
	server.mutex.Unlock()
	select {
	case <-fetching.done:
		return fetching.err
	case <-waitContext.Done():
		return waitContext.Err()
	}
}

// fetch reads one blob from Upstream into a partial file, hashing it as it comes, and renames it to its sha256,
// read-only, only when it is whole and hashes to it.
func (server *Server) fetch(path, sum string) error {
	server.Fetches.Add(1)
	fetchContext, cancel := context.WithTimeout(context.Background(), FetchBound)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchContext, http.MethodGet, strings.TrimSuffix(server.Upstream, "/")+path, nil)
	if err != nil {
		return err
	}
	client := server.Client
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("fetching it from the store: %w", err)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		return errNotUpstream
	case response.StatusCode != http.StatusOK:
		return fmt.Errorf("the store answered %s", response.Status)
	}
	partial, err := os.CreateTemp(server.Directory, partialPrefix+sum+"-")
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		partial.Close()
		if !keep {
			os.Remove(partial.Name())
		}
	}()
	name := filepath.Base(partial.Name())
	if err = server.makeRoom(sum, name, max(response.ContentLength, 0)); err != nil {
		return err
	}
	defer server.release(name)
	stall := server.Stall
	if stall == 0 {
		stall = time.Minute
	}
	watched := &progress{reader: response.Body}
	watched.last.Store(time.Now().UnixNano())
	go watched.watch(fetchContext, cancel, stall)
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(partial, hash), watched)
	switch {
	case err != nil:
		return fmt.Errorf("fetching it from the store, %d bytes in: %w", size, err)
	case response.ContentLength >= 0 && size != response.ContentLength:
		return fmt.Errorf("the store sent %d bytes of %d", size, response.ContentLength)
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != sum {
		server.say("refused %s: the store's bytes hash to %s, so nothing is kept", path, actual)
		return fmt.Errorf("the store's bytes hash to %s: refused, nothing kept", actual)
	}
	if err = partial.Chmod(0o444); err != nil {
		return err
	}
	if err = partial.Sync(); err != nil {
		return err
	}
	if err = os.Rename(partial.Name(), filepath.Join(server.Directory, sum)); err != nil {
		return err
	}
	keep = true
	now := server.now()
	os.Chtimes(filepath.Join(server.Directory, sum), now, now)
	// A store that sent no length was given no room ahead; the bound holds again now.
	server.release(name)
	if err = server.makeRoom(sum, "", 0); err != nil {
		server.say("after fetching %s: %v", path, err)
	}
	return nil
}

// progress is a fetch's body, noting when it last gave a byte, so a stalled store is abandoned rather than waited on.
type progress struct {
	reader io.Reader
	last   atomic.Int64
}

func (watched *progress) Read(buffer []byte) (int, error) {
	count, err := watched.reader.Read(buffer)
	if count > 0 {
		watched.last.Store(time.Now().UnixNano())
	}
	return count, err
}

// watch cancels the fetch once it has gone stall without a byte, and returns when the fetch ends.
func (watched *progress) watch(fetchContext context.Context, cancel context.CancelFunc, stall time.Duration) {
	ticker := time.NewTicker(max(stall/4, 10*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-fetchContext.Done():
			return
		case <-ticker.C:
			if time.Since(time.Unix(0, watched.last.Load())) > stall {
				cancel()
				return
			}
		}
	}
}

// A heldBlob is one blob on the cache's disk.
type heldBlob struct {
	sum  string
	size int64
	used time.Time
}

// makeRoom removes blobs, least recently served first and never keep, until the cache holds at most Limit bytes with
// incoming more on the way to the partial named partial, and its disk keeps Floor bytes free once that and every other
// fetch in flight is whole. It refuses when no removal can make that room; otherwise incoming is reserved for partial
// until release.
func (server *Server) makeRoom(keep, partial string, incoming int64) error {
	server.room.Lock()
	defer server.room.Unlock()
	if incoming > server.Limit {
		return fmt.Errorf("%w: %d bytes is over the cache's bound of %d", errNoRoom, incoming, server.Limit)
	}
	entries, err := os.ReadDir(server.Directory)
	if err != nil {
		return err
	}
	held := []heldBlob{}
	// total is what the cache holds once every fetch is whole; coming, what the disk has yet to take for that.
	total, coming := incoming, incoming
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		switch _, isBlob := ByHash("/blobs/" + entry.Name()); {
		case strings.HasPrefix(entry.Name(), partialPrefix):
			expected := max(info.Size(), server.reserved[entry.Name()])
			total += expected
			coming += expected - info.Size()
		case isBlob:
			total += info.Size()
			if entry.Name() != keep {
				held = append(held, heldBlob{sum: entry.Name(), size: info.Size(), used: info.ModTime()})
			}
		}
	}
	sort.Slice(held, func(left, right int) bool { return held[left].used.Before(held[right].used) })
	evict := func(why string) error {
		if len(held) == 0 {
			return fmt.Errorf("%w: %s, with nothing left to remove", errNoRoom, why)
		}
		oldest := held[0]
		held = held[1:]
		if err := os.Remove(filepath.Join(server.Directory, oldest.sum)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		total -= oldest.size
		server.say("evicted blob %s, %d bytes, last served %s: %s", oldest.sum, oldest.size, oldest.used.UTC().Format(time.RFC3339), why)
		return nil
	}
	for total > server.Limit {
		if err := evict(fmt.Sprintf("the cache would hold %d bytes, over its bound of %d", total, server.Limit)); err != nil {
			return err
		}
	}
	for {
		free, err := server.Free(server.Directory)
		if err != nil {
			return fmt.Errorf("reading the free room on %s: %w", server.Directory, err)
		}
		if free >= server.Floor+uint64(coming) {
			break
		}
		if err := evict(fmt.Sprintf("its disk has %d bytes free, under the floor of %d with %d more to come", free, server.Floor, coming)); err != nil {
			return err
		}
	}
	if partial != "" {
		if server.reserved == nil {
			server.reserved = map[string]int64{}
		}
		server.reserved[partial] = incoming
	}
	return nil
}

// release ends a fetch's reservation.
func (server *Server) release(partial string) {
	server.room.Lock()
	delete(server.reserved, partial)
	server.room.Unlock()
}
