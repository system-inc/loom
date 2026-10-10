package runner

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/housecache"
	"github.com/system-inc/loom/protocol"
)

// houseCacheBefore is a real house cache filling from upstream, served over HTTP.
func houseCacheBefore(t *testing.T, upstream string) *httptest.Server {
	cache := &housecache.Server{Directory: t.TempDir(), Upstream: upstream, Limit: 1 << 30, Free: func(string) (uint64, error) { return 1 << 40, nil }}
	if err := cache.Open(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cache.Close() })
	served := httptest.NewServer(cache)
	t.Cleanup(served.Close)
	return served
}

// deadHouseCache is an address nothing listens on.
func deadHouseCache(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return "http://" + address
}

// lyingHouseCache answers every path with the same bytes, which hash to no blob's name.
func lyingHouseCache(t *testing.T) (*httptest.Server, *int) {
	asked := 0
	served := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		asked++
		writer.Write([]byte("not the blob you asked for"))
	}))
	t.Cleanup(served.Close)
	return served, &asked
}

func housed(cache blobCache, house string) blobCache {
	cache.house, cache.houseClient = house, housecache.Client()
	return cache
}

// Two boxes reading one blob through the house cache cost the store one fetch, and each box's record says the blob
// came from the house cache.
func TestBoxesReadingThroughTheHouseCacheFetchEachBlobOnce(t *testing.T) {
	content := bytes.Repeat([]byte("a test binary "), 1<<12)
	served, sums := newBlobServer(t, content)
	house := houseCacheBefore(t, served.server.URL)
	for box := range 3 {
		got, fetch := readBlob(t, housed(testCache(t, served, 1<<30), house.URL), sums[0])
		if !bytes.Equal(got, content) || !fetch.house || fetch.unhoused != "" || fetch.bytes != int64(len(content)) {
			t.Fatalf("box %d read %d bytes, house %v (%s)", box, len(got), fetch.house, fetch.unhoused)
		}
	}
	if served.gets != 1 {
		t.Fatalf("three boxes through the house cache made %d fetches from the store", served.gets)
	}
}

// A house cache that is down costs a box its short connect, then the store, and the record says why.
func TestADeadHouseCacheFallsBackToTheStore(t *testing.T) {
	content := []byte("the tree's source")
	served, sums := newBlobServer(t, content)
	cache := housed(testCache(t, served, 1<<30), deadHouseCache(t))
	started := time.Now()
	got, fetch := readBlob(t, cache, sums[0])
	if !bytes.Equal(got, content) || fetch.house || fetch.unhoused == "" || time.Since(started) > housecache.ConnectTimeout+5*time.Second {
		t.Fatalf("with the house cache down: %d bytes, house %v, why %q, in %v", len(got), fetch.house, fetch.unhoused, time.Since(started))
	}
	if served.gets != 1 {
		t.Fatalf("%d fetches from the store", served.gets)
	}
}

// Bytes from the house cache are checked as the store's are: a tampered blob is refused, never kept, and the box reads
// the blob from the store.
func TestATamperedHouseCacheBlobIsRefusedAndReadFromTheStore(t *testing.T) {
	content := []byte("a product's archive")
	served, sums := newBlobServer(t, content)
	liar, asked := lyingHouseCache(t)
	cache := housed(testCache(t, served, 1<<30), liar.URL)
	got, fetch := readBlob(t, cache, sums[0])
	if !bytes.Equal(got, content) || fetch.house || !strings.Contains(fetch.unhoused, "hashes to") || *asked != 1 || served.gets != 1 {
		t.Fatalf("a tampered house blob: %q, house %v, why %q, house asked %d, store %d", got, fetch.house, fetch.unhoused, *asked, served.gets)
	}
	if kept, err := os.ReadFile(filepath.Join(cache.directory, sums[0])); err != nil || !bytes.Equal(kept, content) {
		t.Fatalf("the blob kept: %q, %v", kept, err)
	}
	// What the store doesn't hold the house cache doesn't either: the box hears it from the store, as before.
	missing := hashOf([]byte("never uploaded"))
	if _, _, err := cache.open(context.Background(), missing); err == nil || !strings.Contains(err.Error(), "not in the action store") {
		t.Fatalf("a blob nobody holds: %v", err)
	}
}

// A served unit's runner is fetched through the house cache too: once from the release store for every box, checked by
// its sha256, and from the store directly when the house cache is down or lies.
func TestARunnerIsFetchedThroughTheHouseCache(t *testing.T) {
	store := newReleaseStore(t)
	named := store.put([]byte("a loom-runner"))
	house := houseCacheBefore(t, store.server.URL)
	for range 2 {
		cache := runnerCache{directory: t.TempDir(), releases: store.url(), client: http.DefaultClient, house: house.URL, houseClient: housecache.Client()}
		if path, err := cache.path(context.Background(), named); err != nil || !fileHolds(path, "a loom-runner") {
			t.Fatalf("through the house cache: %s, %v", path, err)
		}
	}
	if store.gets != 1 {
		t.Fatalf("two boxes fetched the runner through the house cache with %d fetches from the store", store.gets)
	}
	liar, asked := lyingHouseCache(t)
	for _, house := range []string{deadHouseCache(t), liar.URL} {
		cache := runnerCache{directory: t.TempDir(), releases: store.url(), client: http.DefaultClient, house: house, houseClient: housecache.Client()}
		if path, err := cache.path(context.Background(), named); err != nil || !fileHolds(path, "a loom-runner") {
			t.Fatalf("with the house cache at %s failing: %s, %v", house, path, err)
		}
	}
	if *asked != 1 || store.gets != 3 {
		t.Fatalf("the lying house cache was asked %d times, the store %d", *asked, store.gets)
	}
}

func fileHolds(path, content string) bool {
	got, err := os.ReadFile(path)
	return err == nil && string(got) == content
}

// Serve hands the house cache to the runner a unit names by the environment, never a flag, which a pinned runner from
// before the house cache would refuse; serve's own fetch of that runner goes through the house cache, and past it,
// when it is down, to the release store.
func TestServeHandsTheHouseCacheToTheNamedRunner(t *testing.T) {
	directory := t.TempDir()
	store := newReleaseStore(t)
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + directory + "/arguments'\nprintf '%s\\n' \"$" + housecache.Variable + "\" > '" + directory + "/house'\ncat > /dev/null\necho '{\"type\":\"started\"}'\nexit 0\n"
	named := store.put([]byte(script))
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.job("first", named)}
	options := pool.serveOptions(t, time.Now().Add(time.Second+1500*time.Millisecond), time.Second, io.Discard)
	house := deadHouseCache(t)
	options.Unit.Strict, options.Unit.Root, options.Releases, options.checkRunner = true, filepath.Join(directory, "root"), store.url(), takeAnyRunner
	options.Unit.HouseCache = house
	summary, err := Serve(context.Background(), options)
	if err != nil || summary.Units != 1 || summary.Passed != 1 {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
	if !fileHolds(filepath.Join(directory, "house"), house+"\n") {
		got, _ := os.ReadFile(filepath.Join(directory, "house"))
		t.Fatalf("the named runner saw %s=%q", housecache.Variable, got)
	}
	if arguments, _ := os.ReadFile(filepath.Join(directory, "arguments")); strings.Contains(string(arguments), "house") {
		t.Fatalf("the named runner was given a flag a pinned runner would refuse: %q", arguments)
	}
}
