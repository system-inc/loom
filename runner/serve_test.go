package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

const testPoolToken = "test-pool-token"

// A testPool is a pool's next endpoint and the wire's events endpoint on one server. next hands out queued
// units in order and, with none, waits briefly and answers 204, as the real pool does after 20 s.
type testPool struct {
	mutex  sync.Mutex
	queue  []protocol.Unit
	taken  []string
	askers []string // "<worker> <whether it said its cpus>" per ask
	events map[string][]protocol.Event
	refuse bool
	server *httptest.Server
}

func newTestPool(t *testing.T, units ...protocol.Unit) *testPool {
	pool := &testPool{queue: units, events: map[string][]protocol.Event{}}
	pool.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		switch {
		case request.URL.Path == "/pools/codex/next":
			pool.mutex.Lock()
			refuse := pool.refuse
			pool.mutex.Unlock()
			if refuse || request.Header.Get("Authorization") != "Bearer "+testPoolToken {
				http.Error(writer, "not this pool's token", http.StatusUnauthorized)
				return
			}
			var asker struct {
				Worker string `json:"worker"`
				Cpus   int    `json:"cpus"`
			}
			if err := protocol.Decode(bytes.NewReader(body), &asker); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			pool.mutex.Lock()
			pool.askers = append(pool.askers, fmt.Sprintf("%s %v", asker.Worker, asker.Cpus > 0))
			if len(pool.queue) == 0 {
				pool.mutex.Unlock()
				time.Sleep(100 * time.Millisecond)
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			unit := pool.queue[0]
			pool.queue = pool.queue[1:]
			pool.taken = append(pool.taken, unit.Unit)
			pool.mutex.Unlock()
			json.NewEncoder(writer).Encode(unit)
		case strings.HasSuffix(request.URL.Path, "/events") && request.Method == http.MethodPost:
			if request.Header.Get("Authorization") != "Bearer "+testToken {
				http.Error(writer, "no token", http.StatusUnauthorized)
				return
			}
			pool.mutex.Lock()
			defer pool.mutex.Unlock()
			for _, line := range bytes.Split(bytes.TrimSpace(body), []byte("\n")) {
				var event protocol.Event
				if err := protocol.Decode(bytes.NewReader(line), &event); err != nil {
					http.Error(writer, err.Error(), http.StatusBadRequest)
					return
				}
				pool.events[event.Unit] = append(pool.events[event.Unit], event)
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(pool.server.Close)
	return pool
}

// poolUnit is a unit as the coordinator queues it: its wire set, so the runner posts its own events.
func (pool *testPool) unit(id string, script string) protocol.Unit {
	unit := testUnit("sh", "-c", script)
	unit.Unit = id
	unit.Wire = &protocol.Endpoint{Url: pool.server.URL + "/runs/r-test/events"}
	return unit
}

func (pool *testPool) serveOptions(t *testing.T, deadline time.Time, margin time.Duration, events io.Writer) ServeOptions {
	options := testOptions(t)
	options.Events = events
	return ServeOptions{Pool: pool.server.URL + "/pools/codex", Token: testPoolToken, Worker: "codex-1", Deadline: deadline, Margin: margin, Unit: options}
}

func TestServeRunsUnitsInOrderPostsTheirEventsAndTakesNoneInItsLastMargin(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{
		pool.unit("first", "sleep 1; echo first"),
		pool.unit("second", "sleep 1; echo second >&2; exit 3"),
		pool.unit("third", "echo never"),
	}
	var log lockedBuffer
	started := time.Now()
	// Each unit takes a second, so the third is due when less than the margin remains: it stays queued.
	summary, err := Serve(context.Background(), pool.serveOptions(t, started.Add(2500*time.Millisecond), time.Second, &log))
	if err != nil {
		t.Fatal(err)
	}
	if summary.Units != 2 || summary.Passed != 1 || summary.Failed != 1 || summary.Broken != 0 || summary.Stopped != "at the deadline" {
		t.Fatalf("summary %+v", summary)
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("served %v, past its deadline", elapsed)
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if !reflect.DeepEqual(pool.taken, []string{"first", "second"}) || len(pool.queue) != 1 {
		t.Fatalf("took %v, left %d queued", pool.taken, len(pool.queue))
	}
	if pool.askers[0] != "codex-1 true" {
		t.Fatalf("asked as %q; want the worker's name and its cpus", pool.askers[0])
	}
	// Each unit's whole stream reached the wire with the unit's own token, and the log holds the same.
	logged := decodeEvents(t, log.Bytes())
	for _, id := range []string{"first", "second"} {
		events := pool.events[id]
		checkStream(t, protocol.Unit{Run: "r-test", Unit: id}, events)
		var inLog []protocol.Event
		for _, event := range logged {
			if event.Unit == id {
				inLog = append(inLog, event)
			}
		}
		if !reflect.DeepEqual(events, inLog) {
			t.Fatalf("%s: the wire has %d events, the log %d", id, len(events), len(inLog))
		}
	}
	if pool.events["first"][len(pool.events["first"])-1].Status != protocol.StatusPassed || pool.events["second"][len(pool.events["second"])-1].Status != protocol.StatusFailed {
		t.Fatal("the finished statuses on the wire are wrong")
	}
	if summary.String() != "loom-runner serve: 2 units, 1 passed, 1 failed, 0 broken in 2 s; stopped at the deadline" {
		t.Fatalf("summary line %q", summary.String())
	}
}

// The deadline only stops serve asking: a unit taken before it runs to its own finish, however far past the deadline
// that is (#f6r8wvp asked whether --until abandons a unit in flight; it doesn't).
func TestServeFinishesTheUnitInHandPastItsDeadline(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("long", "sleep 2; echo long"), pool.unit("next", "echo never")}
	started := time.Now()
	deadline := started.Add(1500 * time.Millisecond)
	summary, err := Serve(context.Background(), pool.serveOptions(t, deadline, time.Second, io.Discard))
	if err != nil || summary.Units != 1 || summary.Passed != 1 || summary.Broken != 0 || summary.Stopped != "at the deadline" {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
	if time.Now().Before(deadline.Add(400 * time.Millisecond)) {
		t.Fatalf("served %v; the unit should have run past the %v deadline", time.Since(started), deadline.Sub(started))
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if events := pool.events["long"]; len(events) == 0 || events[len(events)-1].Status != protocol.StatusPassed || len(pool.queue) != 1 {
		t.Fatalf("the wire has %+v for the unit in hand, %d queued", events, len(pool.queue))
	}
}

func TestStoppingServeBreaksTheUnitInHandAndEnds(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("long", "sleep 100"), pool.unit("next", "true")}
	serveContext, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	started := time.Now()
	summary, err := Serve(serveContext, pool.serveOptions(t, time.Now().Add(time.Hour), time.Minute, io.Discard))
	if err != nil || summary.Units != 1 || summary.Broken != 1 || summary.Stopped != "by a signal" || time.Since(started) > 10*time.Second {
		t.Fatalf("summary %+v, err %v after %v", summary, err, time.Since(started))
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if events := pool.events["long"]; len(events) == 0 || events[len(events)-1].Status != protocol.StatusBroken || len(pool.queue) != 1 {
		t.Fatalf("the wire has %+v for the stopped unit, %d queued", events, len(pool.queue))
	}
}

func TestServeEndsWhenThePoolRefusesItsToken(t *testing.T) {
	pool := newTestPool(t)
	pool.refuse = true
	started := time.Now()
	summary, err := Serve(context.Background(), pool.serveOptions(t, time.Now().Add(time.Hour), time.Minute, io.Discard))
	if !errors.Is(err, errPoolRefused) || summary.Units != 0 || !strings.Contains(summary.Stopped, "401") || time.Since(started) > 5*time.Second {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
}

func TestThePoolTokenFileIsReadOnceAndRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool-token")
	os.WriteFile(path, []byte("  pool-token\n"), 0o600)
	token, err := ReadTokenFile(path)
	if err != nil || token != "pool-token" {
		t.Fatalf("token %q, %v", token, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the token file is still there: %v", err)
	}
	os.WriteFile(path, []byte("\n"), 0o600)
	if _, err := ReadTokenFile(path); err == nil {
		t.Fatalf("an empty token file was taken")
	}
}

// A planted full disk (#zzmz489): serve asks the pool for nothing while there's no room for a unit, says so once,
// and asks again once there is. The mutant that asks anyway (the stand-down's continue dropped) takes the unit while
// the disk is full and fails here.
func TestServeStandsDownOnAFullDiskAndAsksAgainOnceThereIsRoom(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("a", "true")}
	var report lockedBuffer
	options := pool.serveOptions(t, time.Now().Add(2*time.Second), 500*time.Millisecond, io.Discard)
	options.Report, options.UnfitPause = &report, 20*time.Millisecond
	var mutex sync.Mutex
	full, asksWhileFull := true, 0
	options.freeMegabytes = func(path string) (int64, error) {
		mutex.Lock()
		defer mutex.Unlock()
		pool.mutex.Lock()
		asked := len(pool.askers)
		pool.mutex.Unlock()
		if full {
			asksWhileFull = asked
			return 900, nil
		}
		return 50000, nil
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		mutex.Lock()
		full = false
		mutex.Unlock()
	}()
	summary, err := Serve(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if asksWhileFull != 0 {
		t.Fatalf("asked the pool %d times with 900 MB free", asksWhileFull)
	}
	if summary.Units != 1 || summary.Passed != 1 || summary.Unfit != "" {
		t.Fatalf("summary %+v", summary)
	}
	if text := string(report.Bytes()); strings.Count(text, "unfit: 900 MB free on ") != 1 || !strings.Contains(text, "room again") {
		t.Fatalf("report %q", text)
	}
}

// Full to the end: no unit taken, and the summary line says unfit, which rearm.sh reads to leave the instance alone.
func TestServeFullToItsDeadlineTakesNothingAndSaysUnfit(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("a", "true")}
	options := pool.serveOptions(t, time.Now().Add(1500*time.Millisecond), time.Second, io.Discard)
	options.UnfitPause = 20 * time.Millisecond
	options.freeMegabytes = func(path string) (int64, error) { return 12, nil }
	summary, _ := Serve(context.Background(), options)
	pool.mutex.Lock()
	asks := len(pool.askers)
	pool.mutex.Unlock()
	if asks != 0 || summary.Units != 0 || !strings.HasSuffix(summary.String(), "; stopped at the deadline; unfit: 12 MB free on "+options.Unit.WorkspaceParent+", under 1500") {
		t.Fatalf("asks %d, summary %q", asks, summary.String())
	}
}

func TestFreeMegabytesReadsTheNearestDirectoryThatExists(t *testing.T) {
	free, err := freeMegabytes(filepath.Join(t.TempDir(), "not", "made", "yet"))
	if err != nil || free <= 0 {
		t.Fatalf("free %d, %v", free, err)
	}
}
