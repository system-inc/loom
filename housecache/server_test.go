package housecache

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// A store is the upstream: objects by path, each GET counted, and a gate that holds every answer until it opens.
type store struct {
	mutex   sync.Mutex
	objects map[string][]byte
	gets    map[string]int
	arrived chan string
	gate    chan struct{}
	server  *httptest.Server
}

func newStore(t *testing.T) *store {
	upstream := &store{objects: map[string][]byte{}, gets: map[string]int{}}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upstream.mutex.Lock()
		upstream.gets[request.URL.Path]++
		content, found := upstream.objects[request.URL.Path]
		arrived, gate := upstream.arrived, upstream.gate
		upstream.mutex.Unlock()
		if arrived != nil {
			arrived <- request.URL.Path
		}
		if gate != nil {
			<-gate
		}
		if !found {
			http.NotFound(writer, request)
			return
		}
		writer.Write(content)
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

// put stores content at /blobs/<its sha256> and /releases/blobs/<its sha256>, and returns the sha256.
func (upstream *store) put(content []byte) string {
	sum := hashOf(content)
	upstream.mutex.Lock()
	upstream.objects["/blobs/"+sum] = content
	upstream.objects["/releases/blobs/"+sum] = content
	upstream.mutex.Unlock()
	return sum
}

func (upstream *store) set(path string, content []byte) {
	upstream.mutex.Lock()
	upstream.objects[path] = content
	upstream.mutex.Unlock()
}

func (upstream *store) remove(path string) {
	upstream.mutex.Lock()
	delete(upstream.objects, path)
	upstream.mutex.Unlock()
}

func (upstream *store) total() int {
	upstream.mutex.Lock()
	defer upstream.mutex.Unlock()
	total := 0
	for _, count := range upstream.gets {
		total += count
	}
	return total
}

// A disk is a filesystem of capacity bytes, all of them the cache's: its free room is capacity less what the cache's
// directory holds, partial fetches included.
type disk struct{ capacity uint64 }

func (fake disk) free(directory string) (uint64, error) {
	used := uint64(0)
	entries, _ := os.ReadDir(directory)
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			used += uint64(info.Size())
		}
	}
	if used > fake.capacity {
		return 0, nil
	}
	return fake.capacity - used, nil
}

// newCache is a house cache filling from upstream, bounded at limit bytes with floor kept free on a disk of capacity,
// served over HTTP, and its clock a minute later each time it is read.
func newCache(t *testing.T, upstream *store, limit int64, floor, capacity uint64) (*Server, *httptest.Server) {
	clock := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	var clockMutex sync.Mutex
	cache := &Server{Directory: filepath.Join(t.TempDir(), "house-cache"), Upstream: upstream.server.URL, Limit: limit, Floor: floor,
		Free: disk{capacity}.free, Client: upstream.server.Client(),
		Now: func() time.Time {
			clockMutex.Lock()
			defer clockMutex.Unlock()
			clock = clock.Add(time.Minute)
			return clock
		}}
	if err := cache.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	served := httptest.NewServer(cache)
	t.Cleanup(served.Close)
	return cache, served
}

type answer struct {
	status int
	body   []byte
	length string
	how    string
}

func get(t *testing.T, url string) answer {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{status: response.StatusCode, body: body, length: response.Header.Get("Content-Length"), how: response.Header.Get("X-Loom-House-Cache")}
}

func held(cache *Server) string {
	names := []string{}
	entries, _ := os.ReadDir(cache.Directory)
	for _, entry := range entries {
		if entry.Name() != lockName {
			names = append(names, entry.Name())
		}
	}
	return strings.Join(names, " ")
}

// A miss fetches the blob once and keeps it, read-only, under its sha256; every later ask is served from the disk with
// its length, even once the store no longer holds it.
func TestAMissFillsTheCacheAndAHitIsServedFromDisk(t *testing.T) {
	upstream := newStore(t)
	content := bytes.Repeat([]byte("a test binary "), 1000)
	sum := upstream.put(content)
	cache, served := newCache(t, upstream, 1<<20, 0, 1<<30)

	first := get(t, served.URL+"/blobs/"+sum)
	if first.status != http.StatusOK || !bytes.Equal(first.body, content) || first.length != "14000" || first.how != "miss" {
		t.Fatalf("a miss answered %d, %d bytes, length %q, %q", first.status, len(first.body), first.length, first.how)
	}
	info, err := os.Stat(filepath.Join(cache.Directory, sum))
	if err != nil || info.Mode().Perm() != 0o444 || info.Size() != int64(len(content)) {
		t.Fatalf("the miss didn't keep the blob read-only under its sha256: %v %v", info, err)
	}
	if got := held(cache); got != sum {
		t.Fatalf("the cache's directory holds %q, not only the blob", got)
	}

	upstream.remove("/blobs/" + sum)
	for range 3 {
		hit := get(t, served.URL+"/blobs/"+sum)
		if hit.status != http.StatusOK || !bytes.Equal(hit.body, content) || hit.length != "14000" || hit.how != "hit" {
			t.Fatalf("a hit answered %d, %d bytes, length %q, %q", hit.status, len(hit.body), hit.length, hit.how)
		}
	}
	// The release store's path names the same bytes by the same sha256: one copy serves both.
	if release := get(t, served.URL+"/releases/blobs/"+sum); release.how != "hit" || !bytes.Equal(release.body, content) {
		t.Fatalf("the release path of a held blob answered %q", release.how)
	}
	if cache.Fetches.Load() != 1 || upstream.total() != 1 {
		t.Fatalf("one blob asked for five times was fetched %d times (the store saw %d)", cache.Fetches.Load(), upstream.total())
	}
	request, _ := http.NewRequest(http.MethodHead, served.URL+"/blobs/"+sum, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusOK || response.ContentLength != int64(len(content)) {
		t.Fatalf("HEAD answered %v, length %d", err, response.ContentLength)
	}
	response.Body.Close()
}

// A store that gives bytes that don't hash to the name asked for is refused: nothing kept, not even a partial, and the
// client is told so; once the store gives the right bytes, they are what is kept.
func TestTheStoresCorruptBytesAreRefusedAndNothingKept(t *testing.T) {
	upstream := newStore(t)
	content := []byte("the tree's source")
	sum := upstream.put(content)
	upstream.set("/blobs/"+sum, []byte("the tree's source, poisoned"))
	cache, served := newCache(t, upstream, 1<<20, 0, 1<<30)

	refused := get(t, served.URL+"/blobs/"+sum)
	if refused.status != http.StatusBadGateway || bytes.Contains(refused.body, []byte("poisoned")) {
		t.Fatalf("corrupt upstream bytes answered %d: %q", refused.status, refused.body)
	}
	if got := held(cache); got != "" {
		t.Fatalf("a refused fetch left %q", got)
	}
	upstream.set("/blobs/"+sum, content)
	if again := get(t, served.URL+"/blobs/"+sum); again.status != http.StatusOK || !bytes.Equal(again.body, content) {
		t.Fatalf("the repaired blob answered %d", again.status)
	}
	if got := held(cache); got != sum {
		t.Fatalf("the cache holds %q, not the repaired blob", got)
	}
}

// However many clients miss one blob at once, the store is asked for it once, and each client gets the whole blob.
func TestConcurrentMissesShareOneFetch(t *testing.T) {
	upstream := newStore(t)
	content := bytes.Repeat([]byte("source "), 1<<16)
	sum := upstream.put(content)
	upstream.arrived, upstream.gate = make(chan string, 16), make(chan struct{})
	cache, served := newCache(t, upstream, 1<<30, 0, 1<<40)

	const clients = 8
	answers := make([]answer, clients)
	var group sync.WaitGroup
	for index := range clients {
		group.Add(1)
		go func() {
			defer group.Done()
			answers[index] = get(t, served.URL+"/blobs/"+sum)
		}()
	}
	<-upstream.arrived
	// Every client is asking by now, each waiting on the one fetch the store holds.
	time.Sleep(200 * time.Millisecond)
	close(upstream.gate)
	group.Wait()
	for index, got := range answers {
		if got.status != http.StatusOK || !bytes.Equal(got.body, content) {
			t.Fatalf("client %d got %d, %d bytes", index, got.status, len(got.body))
		}
	}
	if cache.Fetches.Load() != 1 || upstream.total() != 1 {
		t.Fatalf("%d clients missing one blob at once made %d fetches", clients, upstream.total())
	}
}

// The cache holds at most its bound: a blob that would pass it removes the least recently served first, and a blob
// served again is the most recent, whatever order the blobs came in.
func TestTheCacheIsBoundedLeastRecentlyServedFirst(t *testing.T) {
	upstream := newStore(t)
	sums := []string{}
	for _, letter := range "abcd" {
		sums = append(sums, upstream.put(bytes.Repeat([]byte(string(letter)), 100)))
	}
	cache, served := newCache(t, upstream, 300, 0, 1<<30)
	for _, sum := range sums[:3] {
		get(t, served.URL+"/blobs/"+sum)
	}
	// a, the first in, is served again, so b is the least recently served.
	if again := get(t, served.URL+"/blobs/"+sums[0]); again.how != "hit" {
		t.Fatal("a held blob was fetched again")
	}
	get(t, served.URL+"/blobs/"+sums[3])
	present := ""
	for index, sum := range sums {
		if _, err := os.Stat(filepath.Join(cache.Directory, sum)); err == nil {
			present += string("abcd"[index])
		}
	}
	if present != "acd" {
		t.Fatalf("bounded at 300 bytes the cache holds %q, not a, c and d", present)
	}
	// A blob over the bound is never fetched: its clients read from the store.
	big := upstream.put(bytes.Repeat([]byte("e"), 400))
	if over := get(t, served.URL+"/blobs/"+big); over.status != http.StatusServiceUnavailable {
		t.Fatalf("a blob over the bound answered %d", over.status)
	}
	if _, err := os.Stat(filepath.Join(cache.Directory, big)); err == nil {
		t.Fatal("a blob over the bound was kept")
	}
}

// The disk keeps its floor free: a blob that would take it under removes the least recently served first, and one that
// can't fit even in an empty cache is never fetched, nothing kept.
func TestTheFreeFloorEvictsAndRefusesWhatCantFit(t *testing.T) {
	upstream := newStore(t)
	sums := []string{}
	for _, letter := range "abcd" {
		sums = append(sums, upstream.put(bytes.Repeat([]byte(string(letter)), 200)))
	}
	// A 1000-byte disk keeping 400 free holds three 200-byte blobs; the fourth takes the oldest's place.
	cache, served := newCache(t, upstream, 1<<20, 400, 1000)
	for _, sum := range sums {
		if got := get(t, served.URL+"/blobs/"+sum); got.status != http.StatusOK {
			t.Fatalf("blob answered %d", got.status)
		}
	}
	if _, err := os.Stat(filepath.Join(cache.Directory, sums[0])); err == nil {
		t.Fatal("the oldest blob stayed while the disk went under its floor")
	}
	if free, _ := (disk{1000}).free(cache.Directory); free < 400 {
		t.Fatalf("the disk has %d bytes free, under its floor of 400", free)
	}

	// Under a floor of 900 on the same disk no 200-byte blob fits, even with every other one gone.
	tight, tightServed := newCache(t, upstream, 1<<20, 900, 1000)
	if refused := get(t, tightServed.URL+"/blobs/"+sums[1]); refused.status != http.StatusServiceUnavailable {
		t.Fatalf("a blob that can't fit over the floor answered %d", refused.status)
	}
	if got := held(tight); got != "" {
		t.Fatalf("a blob refused for the floor left %q", got)
	}
}

// Only a path named by its content is served. The release manifest, a tree's index and a ref change under one name,
// so the cache never serves them, never asks the store for them, and so never serves one stale.
func TestOnlyByHashPathsAreServedSoCurrentTxtIsNeverStale(t *testing.T) {
	upstream := newStore(t)
	upstream.set("/releases/current.txt", []byte("version one\n"))
	key := strings.Repeat("ab", 32)
	upstream.set("/trees/"+key+".json", []byte(`{"format":1}`))
	upstream.set("/refs/action/"+key, []byte(strings.Repeat("8", 64)))
	cache, served := newCache(t, upstream, 1<<20, 0, 1<<30)
	paths := []string{"/releases/current.txt", "/releases/manifests/" + strings.Repeat("9", 40) + ".txt", "/trees/" + key + ".json",
		"/refs/action/" + key, "/blobs/" + strings.ToUpper(key), "/blobs/" + key + "/x", "/blobs/../releases/current.txt", "/", "/blobs/"}
	for _, path := range paths {
		if got := get(t, served.URL+path); got.status != http.StatusNotFound {
			t.Fatalf("%s answered %d", path, got.status)
		}
	}
	upstream.set("/releases/current.txt", []byte("version two\n"))
	if got := get(t, served.URL+"/releases/current.txt"); got.status != http.StatusNotFound || bytes.Contains(got.body, []byte("version")) {
		t.Fatalf("current.txt answered %d: %q", got.status, got.body)
	}
	if upstream.total() != 0 || cache.Fetches.Load() != 0 || held(cache) != "" {
		t.Fatalf("paths the cache doesn't serve reached the store %d times and left %q", upstream.total(), held(cache))
	}
	sum := upstream.put([]byte("x"))
	request, _ := http.NewRequest(http.MethodPost, served.URL+"/blobs/"+sum, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil || response.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("a POST answered %v %v", err, response.StatusCode)
	}
	response.Body.Close()
}

// A blob its disk corrupted goes out as it is (the client checks the hash and reads it from the store) and is removed,
// so the next ask fetches it whole again.
func TestABlobCorruptedOnDiskIsRemovedOnceServed(t *testing.T) {
	upstream := newStore(t)
	content := []byte("a product's archive")
	sum := upstream.put(content)
	cache, served := newCache(t, upstream, 1<<20, 0, 1<<30)
	get(t, served.URL+"/blobs/"+sum)
	path := filepath.Join(cache.Directory, sum)
	os.Chmod(path, 0o644)
	os.WriteFile(path, []byte("a product's archivX"), 0o444)
	if bad := get(t, served.URL+"/blobs/"+sum); bad.how != "hit" {
		t.Fatalf("the corrupted copy answered %q", bad.how)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a copy that doesn't hash to its name stayed")
	}
	if again := get(t, served.URL+"/blobs/"+sum); again.how != "miss" || !bytes.Equal(again.body, content) {
		t.Fatalf("after the removal the blob answered %q", again.how)
	}
}

// A store that stops sending mid-blob is abandoned once it has been quiet for Stall: its clients hear so, nothing is
// kept, and the next ask fetches again.
func TestAStalledFetchIsAbandoned(t *testing.T) {
	content := bytes.Repeat([]byte("s"), 1<<16)
	sum := hashOf(content)
	stalled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", "65536")
		writer.Write(content[:100])
		writer.(http.Flusher).Flush()
		<-stalled
	}))
	defer upstream.Close()
	defer close(stalled)
	cache := &Server{Directory: t.TempDir(), Upstream: upstream.URL, Limit: 1 << 30, Free: disk{1 << 40}.free, Stall: 200 * time.Millisecond}
	if err := cache.Open(); err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	served := httptest.NewServer(cache)
	defer served.Close()
	started := time.Now()
	if got := get(t, served.URL+"/blobs/"+sum); got.status != http.StatusBadGateway || time.Since(started) > 5*time.Second {
		t.Fatalf("a stalled store answered %d after %v", got.status, time.Since(started))
	}
	if got := held(cache); got != "" {
		t.Fatalf("a stalled fetch left %q", got)
	}
}

// One directory has one server, and a killed server's partial fetches are gone when the next one opens it.
func TestOpenLocksTheDirectoryAndClearsDeadPartials(t *testing.T) {
	directory := t.TempDir()
	dead := filepath.Join(directory, partialPrefix+strings.Repeat("a", 64)+"-123")
	os.WriteFile(dead, []byte("half"), 0o644)
	first := &Server{Directory: directory, Limit: 1 << 20, Free: disk{1 << 30}.free}
	if err := first.Open(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dead); err == nil {
		t.Fatal("a dead partial stayed")
	}
	second := &Server{Directory: directory, Limit: 1 << 20, Free: disk{1 << 30}.free}
	if err := second.Open(); err == nil {
		t.Fatal("a second server opened a directory the first holds")
	}
	first.Close()
	if err := second.Open(); err != nil {
		t.Fatalf("once the first closed, the second couldn't open: %v", err)
	}
	second.Close()
}

// blobs/X and releases/blobs/X are two objects of the store, and the action store's may be gone after its 7 days while
// the release store's stays: a miss of one never waits on the other's fetch, so the release isn't refused for the
// expired blob's 404.
func TestAMissOfOnePathNeverWaitsOnAnothers(t *testing.T) {
	upstream := newStore(t)
	content := []byte("a loom-runner, released and since expired from the action store")
	sum := upstream.put(content)
	upstream.remove("/blobs/" + sum)
	upstream.arrived, upstream.gate = make(chan string, 4), make(chan struct{})
	_, served := newCache(t, upstream, 1<<20, 0, 1<<30)
	expired := make(chan answer, 1)
	go func() { expired <- get(t, served.URL+"/blobs/"+sum) }()
	<-upstream.arrived
	released := make(chan answer, 1)
	go func() { released <- get(t, served.URL+"/releases/blobs/"+sum) }()
	select {
	case <-upstream.arrived:
	case <-time.After(2 * time.Second):
	}
	close(upstream.gate)
	if got := <-released; got.status != http.StatusOK || !bytes.Equal(got.body, content) {
		t.Fatalf("the release answered %d while the expired blob's fetch was in flight", got.status)
	}
	if got := <-expired; got.status != http.StatusNotFound {
		t.Fatalf("the expired blob answered %d", got.status)
	}
}

// The gate inputs' chunks are served by their sha256 like any blob; their manifest, named by the tar's sha256 and not
// its own, never hashes to its name, so it is refused and never kept, and its clients read it from the store.
func TestGateInputsChunksAreServedAndTheirManifestNever(t *testing.T) {
	upstream := newStore(t)
	chunk := []byte("a gate inputs chunk")
	chunkSum := hashOf(chunk)
	upstream.set("/gate-inputs/"+chunkSum, chunk)
	tarSum := hashOf([]byte("the uncompressed tar"))
	upstream.set("/gate-inputs/"+tarSum, []byte(chunkSum+"\ntotal "+chunkSum+" 19\ntar "+tarSum+" 20\n"))
	cache, served := newCache(t, upstream, 1<<20, 0, 1<<30)
	if got := get(t, served.URL+"/gate-inputs/"+chunkSum); got.status != http.StatusOK || !bytes.Equal(got.body, chunk) {
		t.Fatalf("a chunk answered %d", got.status)
	}
	for range 2 {
		if got := get(t, served.URL+"/gate-inputs/"+tarSum); got.status != http.StatusBadGateway {
			t.Fatalf("the manifest answered %d: %q", got.status, got.body)
		}
	}
	if got := held(cache); got != chunkSum {
		t.Fatalf("the cache holds %q, not the chunk alone", got)
	}
}
