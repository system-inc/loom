package coordinator

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

var testSecret = []byte("loom-test-secret")

// A fakeWire is the Worker's endpoints, enough to watch what the coordinator posts: the plan, every event
// line in arrival order, the verdict, blobs and cache entries, and a pool's queue with its next and the
// run's log read back by position. Tokens are verified with the test secret. As the Worker does, it drops
// an event it already holds and refuses a batch that would change one.
type fakeWire struct {
	mutex    sync.Mutex
	plans    map[string]protocol.Plan
	lines    map[string][]protocol.Event // a run's log; an event's position is its index plus one
	verdict  map[string]protocol.Verdict
	blobs    map[string][]byte
	cache    map[string]protocol.CacheEntry
	machines []BoardMachine
	pools    map[string][]protocol.Unit // each pool's queue
	priority map[string][]int           // each pool's batches' priorities, in the order they came
	swallow  int                        // the next asks to take a unit lose it, as an ask whose turn ended does
	ghosts   int                        // the next units taken go to a worker that says started, then is gone
	reading  map[string]int             // reads of each run's log in flight
	mostRead int                        // the most reads of one run's log ever in flight at once
	server   *httptest.Server
}

func newFakeWire(t *testing.T) *fakeWire {
	wire := &fakeWire{plans: map[string]protocol.Plan{}, lines: map[string][]protocol.Event{}, verdict: map[string]protocol.Verdict{},
		blobs: map[string][]byte{}, cache: map[string]protocol.CacheEntry{}, pools: map[string][]protocol.Unit{}, priority: map[string][]int{}, reading: map[string]int{}}
	wire.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		claims, err := protocol.VerifyToken(testSecret, strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "), time.Now())
		if err != nil {
			http.Error(writer, err.Error(), http.StatusUnauthorized)
			return
		}
		parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/"), "/")
		body, _ := io.ReadAll(request.Body)
		// These two wait for something to arrive, so they take the mutex themselves.
		if request.Method == http.MethodGet && len(parts) == 3 && parts[0] == "runs" && parts[2] == "events" {
			wire.serveLog(writer, request, claims, parts[1])
			return
		}
		if len(parts) == 3 && parts[0] == "pools" && parts[2] == "next" {
			wire.serveNext(writer, claims, parts[1], body)
			return
		}
		wire.mutex.Lock()
		defer wire.mutex.Unlock()
		switch {
		case len(parts) == 2 && parts[0] == "board" && parts[1] == "machines":
			var posted struct{ Machines []BoardMachine }
			protocol.Decode(bytes.NewReader(body), &posted)
			wire.machines = posted.Machines
		case len(parts) == 2 && parts[0] == "cache":
			if request.Method == http.MethodPut {
				var entry protocol.CacheEntry
				if err := protocol.Decode(bytes.NewReader(body), &entry); err != nil || entry.Key != parts[1] {
					http.Error(writer, "bad entry", http.StatusBadRequest)
					return
				}
				if _, held := wire.blobs[entry.Events]; !held {
					http.Error(writer, "no event log", http.StatusConflict)
					return
				}
				wire.cache[parts[1]] = entry
				return
			}
			entry, ok := wire.cache[parts[1]]
			if !ok {
				http.NotFound(writer, request)
				return
			}
			json.NewEncoder(writer).Encode(entry)
		case len(parts) == 3 && parts[0] == "pools" && parts[2] == "units":
			if claims.Scope != protocol.ScopeCoordinator {
				http.Error(writer, "a coordinator token queues units", http.StatusForbidden)
				return
			}
			var posted struct {
				Units    []protocol.Unit `json:"units"`
				Priority int             `json:"priority"`
			}
			if err := protocol.Decode(bytes.NewReader(body), &posted); err != nil {
				http.Error(writer, err.Error(), http.StatusBadRequest)
				return
			}
			wire.pools[parts[1]] = append(wire.pools[parts[1]], posted.Units...)
			wire.priority[parts[1]] = append(wire.priority[parts[1]], posted.Priority)
			json.NewEncoder(writer).Encode(map[string]int{"queued": len(wire.pools[parts[1]])})
		case len(parts) == 3 && parts[0] == "pools" && parts[2] == "queued":
			var asked struct {
				Run string `json:"run"`
			}
			if claims.Scope != protocol.ScopeCoordinator || protocol.Decode(bytes.NewReader(body), &asked) != nil {
				http.Error(writer, "a coordinator token and a run", http.StatusForbidden)
				return
			}
			units := []string{}
			for _, unit := range wire.pools[parts[1]] {
				if unit.Run == asked.Run {
					units = append(units, unit.Unit)
				}
			}
			json.NewEncoder(writer).Encode(map[string][]string{"units": units})
		case len(parts) >= 3 && parts[0] == "runs" && claims.Run != parts[1]:
			http.Error(writer, "other run", http.StatusForbidden)
		case len(parts) == 3 && parts[2] == "plan":
			var plan protocol.Plan
			protocol.Decode(bytes.NewReader(body), &plan)
			wire.plans[parts[1]] = plan
		case len(parts) == 3 && parts[2] == "events":
			if _, done := wire.verdict[parts[1]]; done {
				http.Error(writer, "verdict held", http.StatusConflict)
				return
			}
			var accepted []protocol.Event
			var conflicts []string
			scanner := bufio.NewScanner(bytes.NewReader(body))
			for scanner.Scan() {
				var event protocol.Event
				if err := protocol.Decode(strings.NewReader(scanner.Text()), &event); err != nil {
					http.Error(writer, err.Error(), http.StatusBadRequest)
					return
				}
				if held, found := heldEvent(append(wire.lines[parts[1]], accepted...), event); found {
					if !reflect.DeepEqual(held, event) {
						conflicts = append(conflicts, fmt.Sprintf("%s %d", event.Unit, event.Sequence))
					}
					continue
				}
				accepted = append(accepted, event)
			}
			// A batch lands whole or not at all.
			if len(conflicts) > 0 {
				writer.WriteHeader(http.StatusConflict)
				json.NewEncoder(writer).Encode(map[string][]string{"conflicts": conflicts})
				return
			}
			wire.lines[parts[1]] = append(wire.lines[parts[1]], accepted...)
		case len(parts) == 3 && parts[2] == "verdict":
			var verdict protocol.Verdict
			protocol.Decode(bytes.NewReader(body), &verdict)
			wire.verdict[parts[1]] = verdict
		case len(parts) == 4 && parts[2] == "blobs":
			blob, held := wire.blobs[parts[3]]
			switch request.Method {
			case http.MethodPut:
				sum := sha256.Sum256(body)
				if hex.EncodeToString(sum[:]) != parts[3] {
					http.Error(writer, "hash", http.StatusBadRequest)
					return
				}
				wire.blobs[parts[3]] = body
			case http.MethodHead, http.MethodGet:
				if !held {
					http.NotFound(writer, request)
					return
				}
				writer.Write(blob)
			}
		default:
			http.NotFound(writer, request)
		}
	}))
	t.Cleanup(wire.server.Close)
	return wire
}

// heldEvent finds the event of the same unit and sequence in a log.
func heldEvent(log []protocol.Event, event protocol.Event) (protocol.Event, bool) {
	for _, held := range log {
		if held.Unit == event.Unit && held.Sequence == event.Sequence {
			return held, true
		}
	}
	return protocol.Event{}, false
}

// serveLog answers GET /runs/<run>/events?after=<position> with the log's lines after it, waiting a moment
// for the first when there are none, as the Worker waits 20 s.
func (wire *fakeWire) serveLog(writer http.ResponseWriter, request *http.Request, claims protocol.TokenClaims, run string) {
	if claims.Scope != protocol.ScopeCoordinator || claims.Run != run {
		http.Error(writer, "a coordinator token of the run reads its log", http.StatusForbidden)
		return
	}
	after, err := strconv.Atoi(request.URL.Query().Get("after"))
	if err != nil || after < 0 {
		http.Error(writer, "after is a position", http.StatusBadRequest)
		return
	}
	wire.mutex.Lock()
	wire.reading[run]++
	wire.mostRead = max(wire.mostRead, wire.reading[run])
	wire.mutex.Unlock()
	defer func() {
		wire.mutex.Lock()
		wire.reading[run]--
		wire.mutex.Unlock()
	}()
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		wire.mutex.Lock()
		log := wire.lines[run]
		if len(log) > after || time.Now().After(deadline) {
			for index := after; index < len(log); index++ {
				line, _ := json.Marshal(log[index])
				fmt.Fprintf(writer, "{\"position\":%d,\"event\":%s}\n", index+1, line)
			}
			wire.mutex.Unlock()
			return
		}
		wire.mutex.Unlock()
		select {
		case <-request.Context().Done():
			return
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// serveNext answers POST /pools/<pool>/next with the queue's first unit, or 204 when none arrives in a moment.
func (wire *fakeWire) serveNext(writer http.ResponseWriter, claims protocol.TokenClaims, pool string, body []byte) {
	if claims.Scope != protocol.ScopePool || claims.Run != pool {
		http.Error(writer, "a pool token for this pool asks for its units", http.StatusForbidden)
		return
	}
	var asker struct {
		Worker string `json:"worker"`
		Cpus   int    `json:"cpus"`
	}
	if err := protocol.Decode(bytes.NewReader(body), &asker); err != nil || asker.Worker == "" {
		http.Error(writer, "worker and cpus", http.StatusBadRequest)
		return
	}
	deadline := time.Now().Add(300 * time.Millisecond)
	for {
		wire.mutex.Lock()
		if queue := wire.pools[pool]; len(queue) > 0 {
			wire.pools[pool] = queue[1:]
			swallowed := wire.swallow > 0
			if swallowed {
				wire.swallow--
			}
			if !swallowed && wire.ghosts > 0 {
				wire.ghosts--
				swallowed = true
				unit := queue[0]
				wire.lines[unit.Run] = append(wire.lines[unit.Run], protocol.Event{Run: unit.Run, Unit: unit.Unit, Sequence: unit.SequenceStart,
					Time: time.Now().UTC().Format(timeLayout), Type: "started", Machine: "ghost", HeartbeatSeconds: 0.1})
			}
			wire.mutex.Unlock()
			if swallowed {
				writer.WriteHeader(http.StatusNoContent)
				return
			}
			json.NewEncoder(writer).Encode(queue[0])
			return
		}
		wire.mutex.Unlock()
		if time.Now().After(deadline) {
			writer.WriteHeader(http.StatusNoContent)
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (wire *fakeWire) put(content string) string {
	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	wire.mutex.Lock()
	defer wire.mutex.Unlock()
	wire.blobs[hash] = []byte(content)
	return hash
}

func (wire *fakeWire) events(run string) []protocol.Event {
	wire.mutex.Lock()
	defer wire.mutex.Unlock()
	return append([]protocol.Event(nil), wire.lines[run]...)
}

func shell(id string, script string) protocol.JobUnit {
	return protocol.JobUnit{Id: id, Argv: []string{"sh", "-c", script}, TimeoutSeconds: 30}
}

func config(wire *fakeWire, slots ...Machine) Config {
	if len(slots) == 0 {
		slots = []Machine{LocalMachine{Label: "box-a"}, LocalMachine{Label: "box-b"}}
	}
	return Config{Wire: wire.server.URL, Secret: testSecret, Slots: slots, Log: io.Discard, LateGrace: 2 * time.Second}
}

func run(t *testing.T, config Config, units ...protocol.JobUnit) Result {
	t.Helper()
	result, err := Run(context.Background(), config, protocol.Job{Name: "test-job", Units: units})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAGreenRunPostsThePlanEveryEventAndTheVerdict(t *testing.T) {
	wire := newFakeWire(t)
	result := run(t, config(wire), shell("a", "echo a"), shell("b", "echo b"), shell("c", "echo c"))
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	if got := wire.plans[result.Run].Units; !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("plan %v", got)
	}
	if wire.verdict[result.Run].Status != "green" {
		t.Fatalf("posted verdict %+v", wire.verdict[result.Run])
	}
	// The wire holds the coordinator's record exactly, so it decides the same way.
	if again := protocol.Decide(result.Run, []string{"a", "b", "c"}, wire.events(result.Run)); again.Status != "green" {
		t.Fatalf("the wire's events decide %+v", again)
	}
	if !strings.Contains(result.Page, "/runs/"+result.Run+"?token=") {
		t.Fatalf("page %s", result.Page)
	}
	// The board learns each machine once, with the slots this coordinator holds on it.
	if len(wire.machines) != 2 || wire.machines[0].Name != "box-a" || wire.machines[0].Slots != 1 || wire.machines[0].Cores < 1 {
		t.Fatalf("machines %+v", wire.machines)
	}
	twoSlots := config(wire, LocalMachine{Label: "box-a"}, LocalMachine{Label: "box-a"}, LocalMachine{Label: "box-b"})
	run(t, twoSlots, shell("a", "true"))
	if len(wire.machines) != 2 || wire.machines[0].Slots != 2 || wire.machines[1].Slots != 1 {
		t.Fatalf("machines %+v", wire.machines)
	}
}

func TestAPlantedFailureTurnsTheRunRedNamingIt(t *testing.T) {
	wire := newFakeWire(t)
	result := run(t, config(wire), shell("a", "echo a"), shell("planted", "echo boom >&2; exit 3"), shell("c", "echo c"))
	if result.Verdict.Status != "red" || !reflect.DeepEqual(result.Verdict.Failed, []string{"planted"}) {
		t.Fatalf("verdict %+v", result.Verdict)
	}
}

// A dropMachine drops the first drops units it's given (the runner says started, then nothing) and runs the
// rest locally. hang makes a drop hold on until it is cancelled, the way a wedged box looks.
type dropMachine struct {
	LocalMachine
	mutex sync.Mutex
	drops int
	hang  bool
	ran   []string
}

func (machine *dropMachine) Run(runContext context.Context, unit protocol.Unit, events io.Writer) error {
	machine.mutex.Lock()
	drop := machine.drops > 0
	machine.drops--
	machine.ran = append(machine.ran, unit.Unit)
	machine.mutex.Unlock()
	if !drop {
		return machine.LocalMachine.Run(runContext, unit, events)
	}
	line, _ := json.Marshal(protocol.Event{Run: unit.Run, Unit: unit.Unit, Sequence: 0, Time: "2026-10-08T19:00:00.000Z", Type: "started"})
	events.Write(append(line, '\n'))
	if machine.hang {
		<-runContext.Done()
	}
	return fmt.Errorf("connection to %s lost", machine.Name())
}

func TestADroppedUnitIsPlacedAgainOnceElsewhereAndTheDropIsInTheRecord(t *testing.T) {
	wire := newFakeWire(t)
	flaky := &dropMachine{LocalMachine: LocalMachine{Label: "flaky"}, drops: 1}
	steady := &dropMachine{LocalMachine: LocalMachine{Label: "steady"}}
	result := run(t, config(wire, flaky, steady), shell("a", "sleep 0.3; echo a"))
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	if !reflect.DeepEqual(flaky.ran, []string{"a"}) || !reflect.DeepEqual(steady.ran, []string{"a"}) {
		t.Fatalf("flaky ran %v, steady ran %v; want one each", flaky.ran, steady.ran)
	}
	var notes []string
	sequences := []int{}
	for _, event := range wire.events(result.Run) {
		sequences = append(sequences, event.Sequence)
		if event.Phase == protocol.PhasePlace {
			notes = append(notes, event.Message)
		}
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "flaky dropped the unit after 1 events") || notes[1] != "placed again on steady" {
		t.Fatalf("place notes %q", notes)
	}
	for index, sequence := range sequences {
		if sequence != index {
			t.Fatalf("one gapless stream across both attempts, got %v", sequences)
		}
	}
}

func TestAUnitDroppedTwiceLeavesTheRunVoid(t *testing.T) {
	wire := newFakeWire(t)
	flaky := &dropMachine{LocalMachine: LocalMachine{Label: "flaky"}, drops: 5}
	result := run(t, config(wire, flaky), shell("a", "echo a"), shell("b", "echo b"))
	if result.Verdict.Status != "void" || len(flaky.ran) != 4 {
		t.Fatalf("verdict %+v after %d attempts", result.Verdict, len(flaky.ran))
	}
	if !strings.Contains(strings.Join(result.Verdict.Problems, "\n"), "unit a never finished") {
		t.Fatalf("problems %q", result.Verdict.Problems)
	}
}

func TestALateUnitIsDroppedAndPlacedAgain(t *testing.T) {
	wire := newFakeWire(t)
	wedged := &dropMachine{LocalMachine: LocalMachine{Label: "wedged"}, drops: 1, hang: true}
	steady := &dropMachine{LocalMachine: LocalMachine{Label: "steady"}}
	configuration := config(wire, wedged, steady)
	configuration.LateGrace = 200 * time.Millisecond
	unit := shell("a", "echo a")
	unit.TimeoutSeconds = 1
	started := time.Now()
	result := run(t, configuration, unit)
	if result.Verdict.Status != "green" || time.Since(started) > 10*time.Second {
		t.Fatalf("verdict %+v after %v", result.Verdict, time.Since(started))
	}
	if !strings.Contains(fmt.Sprint(wire.events(result.Run)), "ran past its timeout") {
		t.Fatal("the late drop isn't in the record")
	}
}

// A twiceMachine's runner reports the unit finished twice: the run must not be green.
type twiceMachine struct{ LocalMachine }

func (machine twiceMachine) Run(runContext context.Context, unit protocol.Unit, events io.Writer) error {
	var buffer bytes.Buffer
	machine.LocalMachine.Run(runContext, unit, &buffer)
	events.Write(buffer.Bytes())
	lines := strings.Split(strings.TrimSpace(buffer.String()), "\n")
	var last protocol.Event
	json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	last.Sequence++
	line, _ := json.Marshal(last)
	events.Write(append(line, '\n'))
	return nil
}

func TestADuplicatedUnitIsRefused(t *testing.T) {
	wire := newFakeWire(t)
	result := run(t, config(wire, twiceMachine{}), shell("a", "echo a"))
	if result.Verdict.Status != "void" || !strings.Contains(strings.Join(result.Verdict.Problems, "\n"), "finished 2 times") {
		t.Fatalf("verdict %+v", result.Verdict)
	}
}

func TestAUnitWhoseNeedFailedIsNotPlacedAndTheRunIsVoidNamingTheFailure(t *testing.T) {
	wire := newFakeWire(t)
	build := shell("build", "exit 1")
	tests := shell("tests", "echo never")
	tests.Needs = []string{"build"}
	result := run(t, config(wire), build, tests)
	if result.Verdict.Status != "void" || !reflect.DeepEqual(result.Verdict.Failed, []string{"build"}) {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	if !strings.Contains(fmt.Sprint(wire.events(result.Run)), "not placed: it needs build, which ended failed") {
		t.Fatal("the skip isn't in the record")
	}
}

func TestNeedsRunInOrder(t *testing.T) {
	wire := newFakeWire(t)
	first := shell("first", "sleep 0.2")
	second := shell("second", "echo second")
	second.Needs = []string{"first"}
	result := run(t, config(wire), second, first)
	var order []string
	for _, event := range wire.events(result.Run) {
		if event.Type == "started" {
			order = append(order, event.Unit)
		}
	}
	if result.Verdict.Status != "green" || !reflect.DeepEqual(order, []string{"first", "second"}) {
		t.Fatalf("verdict %+v, started %v", result.Verdict, order)
	}
}

func TestTheLongestRecordedUnitIsPlacedFirst(t *testing.T) {
	wire := newFakeWire(t)
	durations, _ := LoadDurations("")
	durations.Set("test-job", "short", 1)
	durations.Set("test-job", "long", 100)
	configuration := config(wire, LocalMachine{Label: "only"})
	configuration.Durations = durations
	result := run(t, configuration, shell("short", "true"), shell("long", "true"))
	if first := wire.events(result.Run)[0].Unit; first != "long" {
		t.Fatalf("placed %s first", first)
	}
	if seconds, ok := durations.Get("test-job", "short"); !ok || seconds > 10 {
		t.Fatalf("short's new duration %v %v", seconds, ok)
	}
}

func TestACacheablePassIsServedFromTheCacheUnlessUncached(t *testing.T) {
	wire := newFakeWire(t)
	unit := shell("a", "echo computed")
	unit.Cache = true
	first := run(t, config(wire), unit)
	if first.Verdict.Status != "green" || len(first.Verdict.Cached) != 0 || len(wire.cache) != 1 {
		t.Fatalf("first run %+v, %d cache entries", first.Verdict, len(wire.cache))
	}
	second := run(t, config(wire), unit)
	if second.Verdict.Status != "green" || !reflect.DeepEqual(second.Verdict.Cached, []string{"a"}) {
		t.Fatalf("second run %+v", second.Verdict)
	}
	var entry protocol.CacheEntry
	for _, held := range wire.cache {
		entry = held
	}
	if !strings.Contains(string(wire.blobs[entry.Events]), "computed") || entry.Run != first.Run {
		t.Fatalf("the entry's event log doesn't hold the original output: %+v", entry)
	}
	uncachedConfig := config(wire)
	uncachedConfig.Uncached = true
	third := run(t, uncachedConfig, unit)
	if third.Verdict.Status != "green" || len(third.Verdict.Cached) != 0 {
		t.Fatalf("uncached run %+v", third.Verdict)
	}
	// Off by default: the same unit without the flag always runs.
	unit.Cache = false
	if fourth := run(t, config(wire), unit); len(fourth.Verdict.Cached) != 0 {
		t.Fatalf("a unit not marked cacheable came from the cache: %+v", fourth.Verdict)
	}
}

func TestAnInputMissingFromTheStoreStopsTheRunBeforeItsPlan(t *testing.T) {
	wire := newFakeWire(t)
	unit := shell("a", "cat x")
	unit.Inputs = []protocol.Input{{Path: "x", Sha256: strings.Repeat("a", 64)}}
	if _, err := Run(context.Background(), config(wire), protocol.Job{Name: "j", Units: []protocol.JobUnit{unit}}); err == nil || !strings.Contains(err.Error(), "isn't in the store") {
		t.Fatalf("err %v", err)
	}
	if len(wire.plans) != 0 {
		t.Fatal("a plan was posted")
	}
	// With the blob in the store the unit fetches it through the run's blob endpoint.
	unit.Inputs[0].Sha256 = wire.put("from the store")
	result := run(t, config(wire), unit)
	if result.Verdict.Status != "green" || !strings.Contains(fmt.Sprint(wire.events(result.Run)), "from the store") {
		t.Fatalf("verdict %+v", result.Verdict)
	}
}

func TestRunIdsAreWhatTheWireAccepts(t *testing.T) {
	for _, name := range []string{"adamic-gate", "", "a b/c", strings.Repeat("x", 200), "...weird..."} {
		if id := RunId(name, time.Now()); !protocol.RunIdPattern.MatchString(id) {
			t.Errorf("%q gives %q", name, id)
		}
	}
}

func TestARunnerVersionGoesToOneBoxFirstAndTheRestAfterAGreen(t *testing.T) {
	rollout, _ := LoadRollout("")
	if ok, _ := rollout.MayRun("v1", "home"); !ok {
		t.Fatal("the first box is refused")
	}
	rollout.Installed("v1", "home")
	if ok, why := rollout.MayRun("v1", "workshop"); ok || !strings.Contains(why, "no green run") {
		t.Fatalf("a second box before any green: %v %q", ok, why)
	}
	if ok, _ := rollout.MayRun("v1", "home"); !ok {
		t.Fatal("the box that has it is refused")
	}
	rollout.Green("v1", "home")
	rollout.Installed("v1", "home")
	if ok, _ := rollout.MayRun("v1", "workshop"); !ok {
		t.Fatal("a second box after a green is refused, or Installed undid the green")
	}
	if ok, _ := rollout.MayRun("v2", "workshop"); !ok {
		t.Fatal("a new version's first box is refused")
	}
}

func TestAStoppedCoordinatorStillDecidesVoidAndPostsIt(t *testing.T) {
	wire := newFakeWire(t)
	stopping, stop := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, stop)
	result, err := Run(stopping, config(wire), protocol.Job{Name: "j", Units: []protocol.JobUnit{shell("slow", "sleep 20")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict.Status != "void" || wire.verdict[result.Run].Status != "void" {
		t.Fatalf("verdict %+v, posted %+v", result.Verdict, wire.verdict[result.Run])
	}
}

func TestAUnitPreemptedByTheGateIsQueuedAgainNotFailed(t *testing.T) {
	wire := newFakeWire(t)
	var mutex sync.Mutex
	limit := 2
	configuration := config(wire, LocalMachine{Label: "box"}, LocalMachine{Label: "box"})
	configuration.SlotLimit = func(string) int { mutex.Lock(); defer mutex.Unlock(); return limit }
	// The gate takes one slot back while both units run.
	time.AfterFunc(400*time.Millisecond, func() { mutex.Lock(); limit = 1; mutex.Unlock() })
	result := run(t, configuration, shell("a", "sleep 5; echo a"), shell("b", "sleep 5; echo b"))
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	if !strings.Contains(fmt.Sprint(wire.events(result.Run)), "the gate took the slot back; queued again") {
		t.Fatal("the preemption isn't in the record")
	}
}

func TestUnitsWaitForSlotsWhileTheLimitIsZero(t *testing.T) {
	wire := newFakeWire(t)
	var mutex sync.Mutex
	limit := 0
	configuration := config(wire, LocalMachine{Label: "box"})
	configuration.SlotLimit = func(string) int { mutex.Lock(); defer mutex.Unlock(); return limit }
	started := time.Now()
	time.AfterFunc(time.Second, func() { mutex.Lock(); limit = 1; mutex.Unlock() })
	result := run(t, configuration, shell("a", "echo a"))
	if result.Verdict.Status != "green" || time.Since(started) < time.Second {
		t.Fatalf("verdict %+v after %v", result.Verdict, time.Since(started))
	}
}
