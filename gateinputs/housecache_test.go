package gateinputs

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/housecache"
	"github.com/system-inc/loom/r2/r2test"
)

// prepare.sh asks the house cache (LOOM_HOUSE_CACHE) for the gate inputs' chunks, never for the manifest, whose name
// isn't its own hash: a second box reads every chunk from the house cache and none from the store. A house cache that
// lies or is down costs one chunk's try, then the store gives that chunk and the rest, and the inputs unpack whole.
func TestPrepareAsksTheHouseCacheForChunksOnly(t *testing.T) {
	fake := r2test.New(t)
	run := prepareSection(t, fake)
	published, err := Publish(inputsFixture(t), 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(published.Manifest.Chunks) < 2 {
		t.Fatalf("the fixture makes %d chunks; this test needs several", len(published.Manifest.Chunks))
	}
	cache := &housecache.Server{Directory: t.TempDir(), Upstream: fake.Public(), Limit: 1 << 30, Free: func(string) (uint64, error) { return 1 << 40, nil }}
	if err := cache.Open(); err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	var mutex sync.Mutex
	asked := []string{}
	house := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mutex.Lock()
		asked = append(asked, request.URL.Path)
		mutex.Unlock()
		cache.ServeHTTP(writer, request)
	}))
	defer house.Close()

	for box := range 2 {
		fake.ResetRequests()
		if output, ok := run(t.TempDir(), published.Name, housecache.Variable+"="+house.URL); !ok || strings.Contains(output, "house cache didn't give") {
			t.Fatalf("box %d through the house cache: %s", box, output)
		}
		fromStore := fake.Count("PUBLIC", Prefix) - 1 // the manifest is always the store's
		if box == 1 && fromStore != 0 {
			t.Fatalf("the second box read %d chunks from the store, not the house cache", fromStore)
		}
		if got := fake.Count("PUBLIC", Prefix+published.Name); got != 1 {
			t.Fatalf("box %d read the manifest from the store %d times", box, got)
		}
	}
	for _, path := range asked {
		if path == "/"+Prefix+published.Name {
			t.Fatal("the house cache was asked for the manifest")
		}
	}
	if got := int(cache.Fetches.Load()); got != len(published.Manifest.Chunks) {
		t.Fatalf("two boxes made the house cache fetch %d chunks, not each of %d once", got, len(published.Manifest.Chunks))
	}

	liar := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Write([]byte("not a chunk"))
	}))
	defer liar.Close()
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := "http://" + listener.Addr().String()
	listener.Close()
	for _, failing := range []string{liar.URL, dead} {
		output, ok := run(t.TempDir(), published.Name, housecache.Variable+"="+failing)
		if !ok || strings.Count(output, "house cache didn't give") != 1 {
			t.Fatalf("through a failing house cache at %s: %v: %s", failing, ok, output)
		}
	}
}

// prepare.sh holds the house cache to the Go clients' floor, 64 KB a second over 10 s: one that trickles a chunk's
// right bytes slower than that is left after those 10 s, and the store gives that chunk and the rest. No total limit
// stands in for it, since a chunk is up to 90 MiB.
func TestPrepareLeavesATricklingHouseCache(t *testing.T) {
	fake := r2test.New(t)
	run := prepareSection(t, fake)
	published, err := Publish(inputsFixture(t), 4096, fake.Bucket(), fake.Public(), fake.Server.Client())
	if err != nil {
		t.Fatal(err)
	}
	trickling := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		content, found := fake.Object(strings.TrimPrefix(request.URL.Path, "/"))
		if !found {
			http.NotFound(writer, request)
			return
		}
		writer.Header().Set("Content-Length", fmt.Sprint(len(content)))
		for len(content) > 0 {
			piece := min(len(content), 128)
			if _, err := writer.Write(content[:piece]); err != nil {
				return
			}
			writer.(http.Flusher).Flush()
			content = content[piece:]
			select {
			case <-request.Context().Done():
				return
			case <-time.After(500 * time.Millisecond):
			}
		}
	}))
	defer trickling.Close()
	started := time.Now()
	output, ok := run(t.TempDir(), published.Name, housecache.Variable+"="+trickling.URL)
	if !ok || strings.Count(output, "house cache didn't give") != 1 || time.Since(started) > 30*time.Second {
		t.Fatalf("through a house cache trickling 256 bytes a second: %v after %v: %s", ok, time.Since(started), output)
	}
}
