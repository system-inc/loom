package runner

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/system-inc/loom/housecache"
)

// The review of the house cache's clients (#ktkm6fr): a house cache that is unreachable, frozen or trickling must never
// fail a unit or spend its time, and must cost its wait once per process, not once per blob.

// quickHouseCache shortens the house cache's answer and trickle bounds for a test, as its seconds are here.
func quickHouseCache(t *testing.T, header, window time.Duration) {
	header0, window0 := housecache.HeaderTimeout, housecache.IdleWindow
	housecache.HeaderTimeout, housecache.IdleWindow = header, window
	t.Cleanup(func() { housecache.HeaderTimeout, housecache.IdleWindow = header0, window0 })
}

// frozenHouseCache accepts connections into its backlog and never answers, as a cache whose process hung does.
func frozenHouseCache(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return "http://" + listener.Addr().String()
}

// fetchAll opens every blob through cache, jobs at a time, and fails the test on any error.
func fetchAll(t *testing.T, cache blobCache, sums []string, jobs int) {
	next := make(chan string)
	var group sync.WaitGroup
	for range jobs {
		group.Add(1)
		go func() {
			defer group.Done()
			for sum := range next {
				file, _, err := cache.open(context.Background(), sum)
				if err != nil {
					t.Error(err)
					continue
				}
				file.Close()
			}
		}()
	}
	for _, sum := range sums {
		next <- sum
	}
	close(next)
	group.Wait()
}

// An unreachable house cache (the host powered off, its SYNs dropped) costs one connect and a probe, then the whole
// process reads the store for SkipFor, never a connect per blob.
func TestReviewUnreachableHouseCacheCostsOneWait(t *testing.T) {
	contents := [][]byte{}
	for index := range 32 {
		contents = append(contents, []byte(fmt.Sprintf("chunk %d", index)))
	}
	served, sums := newBlobServer(t, contents...)
	house := "http://10.255.255.1:7380"
	cache := housed(testCache(t, served, 1<<30), house)
	started := time.Now()
	fetchAll(t, cache, sums, 4)
	// Without the skip: 8 waves of 4, each a 2 s connect, 16 s.
	if elapsed := time.Since(started); elapsed > 3*housecache.ConnectTimeout+3*time.Second {
		t.Fatalf("32 blobs, 4 at a time, past an unreachable house cache took %v", elapsed)
	}
	if served.gets != len(sums) {
		t.Fatalf("the store gave %d of %d blobs", served.gets, len(sums))
	}
}

// A frozen house cache costs one answer's wait, then the process skips it: never a wait per blob, and never a unit.
func TestReviewFrozenHouseCacheCostsOneWait(t *testing.T) {
	quickHouseCache(t, time.Second, time.Second)
	contents := [][]byte{}
	for index := range 32 {
		contents = append(contents, []byte(fmt.Sprintf("blob %d", index)))
	}
	served, sums := newBlobServer(t, contents...)
	house := frozenHouseCache(t)
	cache := housed(testCache(t, served, 1<<30), house)
	started := time.Now()
	fetchAll(t, cache, sums, 4)
	// Without the skip: 8 waves of 4, each a second's wait, 8 s.
	if elapsed := time.Since(started); elapsed > housecache.HeaderTimeout+housecache.ConnectTimeout+3*time.Second {
		t.Fatalf("32 blobs past a frozen house cache took %v", elapsed)
	}
	if !housecache.Skipping(house) {
		t.Fatal("the frozen house cache isn't skipped")
	}
}

// A house cache that answers and then trickles is cut off once a window passes with too few bytes, and the blob read
// from the store, well within the unit's time; it still answers a probe, so it isn't skipped.
func TestReviewTricklingHouseCacheFallsBack(t *testing.T) {
	quickHouseCache(t, time.Second, time.Second)
	content := []byte("a blob the store has whole")
	served, sums := newBlobServer(t, content)
	var trickled atomic.Int64
	house := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Length", fmt.Sprint(len(content)))
		writer.WriteHeader(200)
		for index := range content {
			select {
			case <-request.Context().Done():
				return
			case <-time.After(300 * time.Millisecond):
			}
			writer.Write(content[index : index+1])
			writer.(http.Flusher).Flush()
			trickled.Add(1)
		}
	}))
	t.Cleanup(house.Close)
	cache := housed(testCache(t, served, 1<<30), house.URL)
	fetchContext, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	started := time.Now()
	file, fetch, err := cache.open(fetchContext, sums[0])
	if err != nil || fetch.house || fetch.unhoused == "" || served.gets != 1 || time.Since(started) > 3*housecache.IdleWindow+housecache.ConnectTimeout {
		t.Fatalf("a trickling house cache: %v, house %v, why %q, store %d, after %v (%d bytes trickled)", err, fetch.house, fetch.unhoused, served.gets, time.Since(started), trickled.Load())
	}
	file.Close()
	if housecache.Skipping(house.URL) {
		t.Fatal("a house cache that answers its probe is skipped")
	}
}

// A frozen house cache never spends the unit's time: the store gives the blob long before the unit's deadline.
func TestReviewFrozenHouseCacheFallsBackWithinTheUnitsTime(t *testing.T) {
	quickHouseCache(t, time.Second, time.Second)
	content := []byte("a blob the store has whole")
	served, sums := newBlobServer(t, content)
	cache := housed(testCache(t, served, 1<<30), frozenHouseCache(t))
	fetchContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := time.Now()
	file, fetch, err := cache.open(fetchContext, sums[0])
	if err != nil || fetch.house || served.gets != 1 || time.Since(started) > housecache.HeaderTimeout+2*time.Second {
		t.Fatalf("a frozen house cache: %v, why %q, store %d, after %v", err, fetch.unhoused, served.gets, time.Since(started))
	}
	file.Close()
}

// prepare.sh is given the house cache for the gate inputs' chunks, unless there is none or the process is skipping it.
func TestPrepareIsGivenTheHouseCacheUnlessItIsSkipped(t *testing.T) {
	quickHouseCache(t, time.Second, time.Second)
	has := func(options Options) bool {
		run := &unitRun{options: options}
		for _, variable := range run.prepareEnvironment() {
			if variable == housecache.Variable+"="+options.HouseCache {
				return true
			}
		}
		return false
	}
	if has(Options{}) {
		t.Fatal("prepare.sh was given a house cache with none set")
	}
	frozen := frozenHouseCache(t)
	if !has(Options{HouseCache: frozen}) {
		t.Fatal("prepare.sh wasn't given the house cache")
	}
	housecache.Unanswered(frozen)
	for deadline := time.Now().Add(5 * time.Second); !housecache.Skipping(frozen) && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if has(Options{HouseCache: frozen}) {
		t.Fatal("prepare.sh was given a house cache the process is skipping")
	}
}
