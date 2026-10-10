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
//   - A miss fetches the blob from Upstream once, however many clients ask for it meanwhile, and streams it to every
//     one of them as it arrives: each answer starts once the store answers, so no client waits on the whole blob over
//     a slow link. It goes into a partial file, hashed as it comes, and only a whole blob that hashes to its name is
//     made read-only and renamed to it, so a name only ever holds the whole, checked blob. Every answer holds back its
//     last byte until the hash checks, never waiting on the disk's sync that follows: a blob that doesn't hash to its
//     name, or a store that fails midway, cuts every answer short, so each client's own read fails and it reads the
//     store, and nothing is kept.
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
	// IdleWindow and IdleBytes watch a fetch's body as a client watches the cache's answer (Watch): once a window
	// passes in which fewer than IdleBytes arrived, the fetch is abandoned, its clients read the store, and the next ask
	// fetches again on a new connection. Zero means the clients' own, IdleWindow and IdleBytes. Oct 10: the big house's
	// downloads go over its cellular line, where a connection stalls mid-body when the line drops, and a fetch that
	// waited on it held its blob's flight for up to FetchBound.
	IdleWindow time.Duration
	IdleBytes  int64
	// Now is the clock a served blob's use is stamped with; nil means time.Now.
	Now func() time.Time
	// Sync makes a fetched blob durable before it takes its name; nil means its file's Sync. Tests plant a slow disk.
	Sync func(file *os.File) error

	// Fetches counts the fetches from Upstream.
	Fetches atomic.Int64

	mutex sync.Mutex
	// flights are the fetches in progress by path, never by sha256 alone: blobs/X and releases/blobs/X are two objects
	// of the store, and one may be gone (the action store's 7 days) while the other stays.
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

// A flight is one fetch from Upstream, streamed to every client asking for its path meanwhile.
type flight struct {
	// started closes once the store answered, or failed to: refused is why it failed before answering, length its
	// Content-Length (-1 unknown), partial the file the bytes arrive in. Each is written only before started closes.
	started chan struct{}
	refused error
	length  int64
	partial string

	mutex sync.Mutex
	// written is how many bytes the partial holds; checked says they are the whole blob and hash to its name, so the
	// answers may send their last byte; finished says the fetch ended, and then err says how (nil: the blob is whole,
	// checked and stored). moved closes and is replaced each time any of them changes.
	written  int64
	checked  bool
	finished bool
	err      error
	moved    chan struct{}
}

// note changes the flight under its lock and wakes every client streaming it.
func (fetching *flight) note(change func()) {
	fetching.mutex.Lock()
	change()
	close(fetching.moved)
	fetching.moved = make(chan struct{})
	fetching.mutex.Unlock()
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

func (server *Server) sync(file *os.File) error {
	if server.Sync != nil {
		return server.Sync(file)
	}
	return file.Sync()
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
		if request.Method == http.MethodHead {
			// A HEAD says what is held; only a GET fetches.
			http.Error(writer, "the house cache doesn't hold it; a GET fetches it", http.StatusNotFound)
			return
		}
		var fetching *flight
		if fetching, err = server.fill(request.Context(), request.URL.Path, sum); err == nil {
			if fetching == nil {
				// A fetch that finished since this client looked: a hit after all.
				how = "hit"
				file, err = os.Open(path)
			} else {
				server.stream(writer, request, fetching, sum)
				return
			}
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

// stream answers one client from a flight: once the store answered, the bytes as they arrive in the partial, all but
// the last until the blob is whole and checked. A flight that ends otherwise aborts the answer (http.ErrAbortHandler),
// cut short, so the client's read fails and it reads the store.
func (server *Server) stream(writer http.ResponseWriter, request *http.Request, fetching *flight, sum string) {
	select {
	case <-fetching.started:
	case <-request.Context().Done():
		return
	}
	if fetching.refused != nil {
		status := http.StatusBadGateway
		switch {
		case errors.Is(fetching.refused, errNotUpstream):
			status = http.StatusNotFound
		case errors.Is(fetching.refused, errNoRoom):
			status = http.StatusServiceUnavailable
		}
		server.say("miss %s: %v", request.URL.Path, fetching.refused)
		http.Error(writer, fetching.refused.Error(), status)
		return
	}
	file, err := os.Open(fetching.partial)
	if err != nil {
		// The fetch finished and renamed it, or failed and removed it.
		if failed := fetching.wait(); failed != nil {
			http.Error(writer, failed.Error(), http.StatusBadGateway)
			return
		}
		if file, err = os.Open(filepath.Join(server.Directory, sum)); err != nil {
			http.Error(writer, err.Error(), http.StatusBadGateway)
			return
		}
	}
	defer file.Close()
	writer.Header().Set("Content-Type", "application/octet-stream")
	if fetching.length >= 0 {
		writer.Header().Set("Content-Length", fmt.Sprint(fetching.length))
	}
	writer.Header().Set("X-Loom-House-Cache", "miss")
	writer.WriteHeader(http.StatusOK)
	controller := http.NewResponseController(writer)
	sent := int64(0)
	for {
		fetching.mutex.Lock()
		written, checked, finished, failed, moved := fetching.written, fetching.checked, fetching.finished, fetching.err, fetching.moved
		fetching.mutex.Unlock()
		// Once checked, the answer is the whole blob, whatever storing it on disk then meets.
		if finished && failed != nil && !checked {
			panic(http.ErrAbortHandler)
		}
		ready := written
		if !checked {
			ready--
		}
		if ready > sent {
			copied, err := io.CopyN(writer, file, ready-sent)
			sent += copied
			if err != nil {
				return
			}
			controller.Flush()
			continue
		}
		if checked || finished {
			return
		}
		select {
		case <-moved:
		case <-request.Context().Done():
			return
		}
	}
}

// wait waits for the flight to finish and says how it ended.
func (fetching *flight) wait() error {
	for {
		fetching.mutex.Lock()
		finished, failed, moved := fetching.finished, fetching.err, fetching.moved
		fetching.mutex.Unlock()
		if finished {
			return failed
		}
		<-moved
	}
}

// fill joins, or starts, the one fetch from Upstream of this path, which every client asking for it meanwhile streams.
// It is nil when the blob is already on disk. A client that goes away leaves the fetch running for the others.
func (server *Server) fill(waitContext context.Context, path, sum string) (*flight, error) {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	if _, err := os.Stat(filepath.Join(server.Directory, sum)); err == nil {
		return nil, nil
	}
	if server.flights == nil {
		server.flights = map[string]*flight{}
	}
	fetching := server.flights[path]
	if fetching == nil {
		fetching = &flight{started: make(chan struct{}), length: -1, moved: make(chan struct{})}
		server.flights[path] = fetching
		go func() {
			server.fetch(fetching, path, sum)
			server.mutex.Lock()
			delete(server.flights, path)
			server.mutex.Unlock()
		}()
	}
	return fetching, nil
}

// fetch reads one blob from Upstream into a partial file, hashing it as it comes and telling the flight's clients of
// every byte, and renames it to its sha256, read-only, only when it is whole and hashes to it. Its outcome is the
// flight's err: set before started closes when it failed before the store answered, else when it finishes.
func (server *Server) fetch(fetching *flight, path, sum string) {
	startedOnce := sync.Once{}
	start := func() { startedOnce.Do(func() { close(fetching.started) }) }
	finish := func(err error) {
		start()
		fetching.note(func() { fetching.err, fetching.finished = err, true })
	}
	fail := func(err error) {
		// Before the store answered, the clients hear it as a status.
		select {
		case <-fetching.started:
		default:
			fetching.refused = err
		}
		finish(err)
	}
	server.Fetches.Add(1)
	fetchContext, cancel := context.WithTimeout(context.Background(), FetchBound)
	defer cancel()
	request, err := http.NewRequestWithContext(fetchContext, http.MethodGet, strings.TrimSuffix(server.Upstream, "/")+path, nil)
	if err != nil {
		fail(err)
		return
	}
	client := server.Client
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(request)
	if err != nil {
		fail(fmt.Errorf("fetching it from the store: %w", err))
		return
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusNotFound:
		fail(errNotUpstream)
		return
	case response.StatusCode != http.StatusOK:
		fail(fmt.Errorf("the store answered %s", response.Status))
		return
	}
	partial, err := os.CreateTemp(server.Directory, partialPrefix+sum+"-")
	if err != nil {
		fail(err)
		return
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
		fail(err)
		return
	}
	defer server.release(name)
	fetching.length, fetching.partial = response.ContentLength, partial.Name()
	start()
	window, least := server.IdleWindow, server.IdleBytes
	if window == 0 {
		window = IdleWindow
	}
	if least == 0 {
		least = IdleBytes
	}
	watched := watchIdle(response.Body, cancel, window, least)
	defer watched.Close()
	hash := sha256.New()
	buffer := make([]byte, 256<<10)
	size := int64(0)
	for {
		count, readErr := watched.Read(buffer)
		if count > 0 {
			if _, err = partial.Write(buffer[:count]); err != nil {
				finish(fmt.Errorf("writing it, %d bytes in: %w", size, err))
				return
			}
			hash.Write(buffer[:count])
			size += int64(count)
			fetching.note(func() { fetching.written = size })
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			server.say("fetching %s from the store failed %d bytes in: %v", path, size, readErr)
			finish(fmt.Errorf("fetching it from the store, %d bytes in: %w", size, readErr))
			return
		}
	}
	if response.ContentLength >= 0 && size != response.ContentLength {
		finish(fmt.Errorf("the store sent %d bytes of %d", size, response.ContentLength))
		return
	}
	if actual := hex.EncodeToString(hash.Sum(nil)); actual != sum {
		server.say("refused %s: the store's bytes hash to %s, so nothing is kept and every answer is cut short", path, actual)
		finish(fmt.Errorf("the store's bytes hash to %s: refused, nothing kept", actual))
		return
	}
	// Whole and checked: every answer may send its last byte now, never waiting on a slow disk's sync of a large blob
	// (a hit hashes the stored copy again as it goes out).
	fetching.note(func() { fetching.checked = true })
	if err = partial.Chmod(0o444); err == nil {
		err = server.sync(partial)
	}
	if err == nil {
		err = os.Rename(partial.Name(), filepath.Join(server.Directory, sum))
	}
	if err != nil {
		finish(err)
		return
	}
	keep = true
	now := server.now()
	os.Chtimes(filepath.Join(server.Directory, sum), now, now)
	finish(nil)
	server.say("fetched %s, %d bytes, from the store", path, size)
	// A store that sent no length was given no room ahead; the bound holds again now.
	server.release(name)
	if err = server.makeRoom(sum, "", 0); err != nil {
		server.say("after fetching %s: %v", path, err)
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
