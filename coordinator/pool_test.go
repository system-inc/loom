package coordinator

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner"
)

// servePool runs workers in this process as `loom-runner serve` runs on a Codex instance: each asks the
// fake wire's pool for units with a pool token and runs them, posting their events to the run itself. They
// serve until the test ends.
func servePool(t *testing.T, wire *fakeWire, pool string, workers int) {
	token, err := protocol.MintToken(testSecret, protocol.TokenClaims{Run: pool, Scope: protocol.ScopePool, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	serveContext, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	for index := range workers {
		options := runner.ServeOptions{Pool: wire.server.URL + "/pools/" + pool, Token: token, Worker: fmt.Sprintf("worker-%d", index),
			Deadline: time.Now().Add(time.Hour), Unit: runner.Options{WorkspaceParent: t.TempDir()}}
		group.Add(1)
		go func() {
			defer group.Done()
			runner.Serve(serveContext, options)
		}()
	}
	t.Cleanup(func() {
		cancel()
		group.Wait()
	})
}

// poolSlots is one pool given to the coordinator as count slots: the same machine each time, so its slots
// share their reading of each run's log.
func poolSlots(wire *fakeWire, count int) []Machine {
	pool := &PoolMachine{Pool: "codex", Wire: wire.server.URL, Secret: testSecret, Version: runner.Version,
		GoPlatform: runtime.GOOS + "/" + runtime.GOARCH, CoreCount: 8}
	slots := make([]Machine, count)
	for index := range slots {
		slots[index] = pool
	}
	return slots
}

// poolUnit is a shell unit with a short timeout, so a unit the coordinator never hears from goes late soon.
func poolUnit(id string, script string) protocol.JobUnit {
	unit := shell(id, script)
	unit.TimeoutSeconds = 5
	return unit
}

func unitEventsOf(events []protocol.Event, unit string) []protocol.Event {
	var result []protocol.Event
	for _, event := range events {
		if event.Unit == unit {
			result = append(result, event)
		}
	}
	return result
}

func TestThreeUnitsOnPoolSlotsAreGreenAndTheWireHoldsTheRecord(t *testing.T) {
	wire := newFakeWire(t)
	servePool(t, wire, "codex", 2)
	result := run(t, config(wire, poolSlots(wire, 3)...), poolUnit("a", "echo a"), poolUnit("b", "echo b"), poolUnit("c", "echo c"))
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	// The runners posted each unit's stream; the coordinator read it back and relayed it, and the wire kept
	// one copy. Each unit's stream on the wire is the coordinator's record of it, event for event.
	held := wire.events(result.Run)
	if len(held) != len(result.Events) {
		t.Fatalf("the wire holds %d events, the record %d", len(held), len(result.Events))
	}
	for _, id := range []string{"a", "b", "c"} {
		onWire, inRecord := unitEventsOf(held, id), unitEventsOf(result.Events, id)
		if !reflect.DeepEqual(onWire, inRecord) {
			t.Fatalf("%s: the wire has %+v, the record %+v", id, onWire, inRecord)
		}
		// The worker says where it ran, by the name the pool shows it under; the pool is only where the coordinator
		// placed it.
		said := ""
		for _, event := range inRecord {
			if event.Type == "output" {
				said += event.Text
			}
		}
		if inRecord[0].Type != "started" || !strings.HasPrefix(inRecord[0].Machine, "worker-") || said != id {
			t.Fatalf("%s: %+v", id, inRecord)
		}
	}
	if again := protocol.Decide(result.Run, []string{"a", "b", "c"}, held); again.Status != "green" {
		t.Fatalf("the wire's events decide %+v", again)
	}
	// The three slots waited on the run together and read its log through one follower.
	wire.mutex.Lock()
	mostRead, machines := wire.mostRead, wire.machines
	wire.mutex.Unlock()
	if mostRead != 1 {
		t.Fatalf("the run's log was read %d at once; the pool's slots share one follower", mostRead)
	}
	if len(machines) != 1 || machines[0] != (BoardMachine{Name: "pool:codex", Cores: 8, Slots: 3}) {
		t.Fatalf("machines %+v", machines)
	}
	if !reflect.DeepEqual(result.Machines, []string{"pool:codex"}) {
		t.Fatalf("ran on %v", result.Machines)
	}
}

func TestAPoolMachinesPriorityRidesWithEveryUnitItQueues(t *testing.T) {
	wire := newFakeWire(t)
	servePool(t, wire, "codex", 2)
	slots := poolSlots(wire, 2)
	slots[0].(*PoolMachine).Priority = 30
	result := run(t, config(wire, slots...), poolUnit("a", "echo a"), poolUnit("b", "echo b"), poolUnit("c", "echo c"))
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	wire.mutex.Lock()
	priorities := wire.priority["codex"]
	wire.mutex.Unlock()
	if !reflect.DeepEqual(priorities, []int{30, 30, 30}) {
		t.Fatalf("the pool's batches came with priorities %v, not 30 each", priorities)
	}
}

// A pool unit's timeout runs from its first event: one that waits in the queue longer than its timeout and grace,
// for a worker busy elsewhere, still runs and finishes (the budget proof of Oct 9 lost ten 90 s units that way).
func TestAPoolUnitsClockStartsWhenItStartsNotWhenItIsQueued(t *testing.T) {
	wire := newFakeWire(t)
	settings := config(wire, poolSlots(wire, 1)...)
	settings.LateGrace = time.Second
	unit := poolUnit("waits", "echo ran")
	unit.TimeoutSeconds = 2
	// Workers arrive after 4 s, past the unit's 2 s and 1 s of grace counted from when it was queued.
	time.AfterFunc(4*time.Second, func() { servePool(t, wire, "codex", 1) })
	result := run(t, settings, unit)
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	// Green on its first attempt: a drop placed again would also end green, and hide the clock it came from.
	for _, event := range result.Events {
		if strings.Contains(event.Message, "dropped the unit") || strings.Contains(event.Message, "placed again") {
			t.Fatalf("the queued unit was dropped before it ran: %q", event.Message)
		}
	}
}

// The wait has an allowance of its own: a pool unit no worker starts within it is dropped, said so, and the run is
// void.
func TestAPoolUnitThatWaitsPastItsQueueAllowanceIsDropped(t *testing.T) {
	wire := newFakeWire(t)
	settings := config(wire, poolSlots(wire, 1)...)
	settings.PoolQueueWait = time.Second
	result := run(t, settings, poolUnit("waits", "true"))
	if result.Verdict.Status != "void" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	said := false
	for _, event := range result.Events {
		if strings.Contains(event.Message, "in the pool's queue and never started") {
			said = true
		}
	}
	if !said {
		t.Fatalf("no event says the unit never started: %+v", result.Events)
	}
}

func TestAPlantedFailureOnAPoolUnitTurnsTheRunRed(t *testing.T) {
	wire := newFakeWire(t)
	servePool(t, wire, "codex", 2)
	result := run(t, config(wire, poolSlots(wire, 2)...), poolUnit("a", "echo a"), poolUnit("planted", "echo boom >&2; exit 3"), poolUnit("c", "echo c"))
	if result.Verdict.Status != "red" || !reflect.DeepEqual(result.Verdict.Failed, []string{"planted"}) {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	if !strings.Contains(fmt.Sprint(unitEventsOf(result.Events, "planted")), "boom") {
		t.Fatal("the planted unit's output isn't in the record")
	}
}

func TestAPoolUnitNobodyTakesLeavesAStoppedRunVoid(t *testing.T) {
	wire := newFakeWire(t)
	stopping, stop := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, stop)
	started := time.Now()
	result, err := Run(stopping, config(wire, poolSlots(wire, 1)...), protocol.Job{Name: "j", Units: []protocol.JobUnit{poolUnit("waits", "true")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict.Status != "void" || time.Since(started) > 5*time.Second {
		t.Fatalf("verdict %+v after %v", result.Verdict, time.Since(started))
	}
	// The unit is left queued: cancelling the pool for the run would drop its other units too.
	wire.mutex.Lock()
	defer wire.mutex.Unlock()
	if queue := wire.pools["codex"]; len(queue) != 1 || queue[0].Wire == nil || queue[0].Wire.Url != wire.server.URL+"/runs/"+result.Run+"/events" {
		t.Fatalf("queued %+v", queue)
	}
}

func TestAPoolUnitHandedToAnAskNobodyHearsIsQueuedAgainAndFinishes(t *testing.T) {
	wire := newFakeWire(t)
	wire.swallow = 1
	servePool(t, wire, "codex", 1)
	var log strings.Builder
	pool := &PoolMachine{Pool: "codex", Wire: wire.server.URL, Secret: testSecret, Version: runner.Version,
		GoPlatform: runtime.GOOS + "/" + runtime.GOARCH, NeverStarted: 300 * time.Millisecond, QueueCheck: 50 * time.Millisecond, Log: &log}
	unit := poolUnit("lost", "echo lost")
	unit.TimeoutSeconds = 30
	result := run(t, config(wire, pool), unit)
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	if !strings.Contains(log.String(), "lost: queued again on pool codex") {
		t.Fatalf("the requeue wasn't logged: %q", log.String())
	}
	// It was one attempt: the unit never started the first time, so the record has one started event.
	starts := 0
	for _, event := range unitEventsOf(result.Events, "lost") {
		if event.Type == "started" {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("%d started events: %+v", starts, unitEventsOf(result.Events, "lost"))
	}
}

func TestAPoolUnitWhoseWorkerGoesSilentIsPlacedAgainAndContinuesItsStream(t *testing.T) {
	wire := newFakeWire(t)
	wire.ghosts = 1
	servePool(t, wire, "codex", 1)
	pool := &PoolMachine{Pool: "codex", Wire: wire.server.URL, Secret: testSecret, Version: runner.Version,
		GoPlatform: runtime.GOOS + "/" + runtime.GOARCH, QueueCheck: 50 * time.Millisecond}
	unit := poolUnit("orphaned", "echo finished")
	unit.TimeoutSeconds = 30
	started := time.Now()
	result := run(t, config(wire, pool), unit)
	if result.Verdict.Status != "green" || time.Since(started) > 20*time.Second {
		t.Fatalf("verdict %+v after %v", result.Verdict, time.Since(started))
	}
	// One stream across both attempts: the ghost's started, the coordinator's notes, then the second runner's
	// events numbered on from them, and the wire holds exactly the record's events, none in conflict.
	inRecord := unitEventsOf(result.Events, "orphaned")
	machines := []string{}
	for index, event := range inRecord {
		if event.Sequence != index {
			t.Fatalf("event %d has sequence %d: %+v", index, event.Sequence, inRecord)
		}
		if event.Type == "started" {
			machines = append(machines, event.Machine)
		}
	}
	if len(machines) != 2 || machines[0] != "ghost" || !strings.HasPrefix(machines[1], "worker-") {
		t.Fatalf("started on %v", machines)
	}
	onWire := unitEventsOf(wire.events(result.Run), "orphaned")
	sort.Slice(onWire, func(left, right int) bool { return onWire[left].Sequence < onWire[right].Sequence })
	if !reflect.DeepEqual(onWire, inRecord) {
		t.Fatalf("the wire holds %+v, the record %+v", onWire, inRecord)
	}
}

// agingSlots is one pool at priority 30 that lifts a waiting unit 10 every 150 ms, up to 50.
func agingSlots(wire *fakeWire, count int) []Machine {
	slots := poolSlots(wire, count)
	pool := slots[0].(*PoolMachine)
	pool.Priority, pool.AgeEvery, pool.AgeStep, pool.AgeCeiling = 30, 150*time.Millisecond, 10, 50
	pool.QueueCheck, pool.NeverStarted = 40*time.Millisecond, 300*time.Millisecond
	return slots
}

// waitFor polls the fake wire until check holds, failing the test after a few seconds.
func waitFor(t *testing.T, wire *fakeWire, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		wire.mutex.Lock()
		held := check()
		wire.mutex.Unlock()
		if held {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("never saw %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func startedCounts(events []protocol.Event) map[string]int {
	counts := map[string]int{}
	for _, event := range events {
		if event.Type == "started" {
			counts[event.Unit]++
		}
	}
	return counts
}

// #0k03wz1, Oct 9: a tier-30 job placed 0 of 6 units in 30 minutes behind a stream of tier-40 landings. A unit that
// waits climbs a tier each AgeEvery, to the ceiling and no higher, and sits in the queue once however often it moves.
func TestAWaitingUnitIsOfferedAgainATierHigherEachAgeStepUpToTheCeiling(t *testing.T) {
	wire := newFakeWire(t)
	done := make(chan Result, 1)
	go func() {
		result, _ := Run(context.Background(), config(wire, agingSlots(wire, 2)...), protocol.Job{Name: "test-job", Units: []protocol.JobUnit{poolUnit("a", "echo a"), poolUnit("b", "echo b")}})
		done <- result
	}()
	waitFor(t, wire, "both units at the ceiling", func() bool {
		ranks := wire.ranks["codex"]
		return len(ranks) == 2 && ranks[0] == 50 && ranks[1] == 50
	})
	time.Sleep(400 * time.Millisecond) // two more steps' worth: nothing climbs past the ceiling, nothing doubles
	wire.mutex.Lock()
	queued, ranks, batches := len(wire.pools["codex"]), append([]int{}, wire.ranks["codex"]...), append([]int{}, wire.priority["codex"]...)
	wire.mutex.Unlock()
	if queued != 2 || !reflect.DeepEqual(ranks, []int{50, 50}) {
		t.Fatalf("queued %d at %v, not each unit once at 50", queued, ranks)
	}
	if len(batches) < 4 || batches[0] != 30 || batches[1] != 30 || !slices.Contains(batches, 40) || slices.Max(batches) != 50 {
		t.Fatalf("batches came at %v: 30 for each unit, then 40, then 50, never higher", batches)
	}
	servePool(t, wire, "codex", 2)
	result := <-done
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	if counts := startedCounts(result.Events); counts["a"] != 1 || counts["b"] != 1 {
		t.Fatalf("started %v, not each unit once", counts)
	}
}

// The wire drops a run's queued units only all at once and says how many, not which. A worker that takes one between
// the re-offer's look and its drop must not get a second copy: the drop counts fewer than the look listed, nothing is
// queued again, and each unit the drop took is found gone and queued again on its own, at the tier it earned.
func TestAReofferThatRacesAWorkerQueuesNothingTwice(t *testing.T) {
	wire := newFakeWire(t)
	wire.racing = 1
	done := make(chan Result, 1)
	go func() {
		result, _ := Run(context.Background(), config(wire, agingSlots(wire, 3)...), protocol.Job{Name: "test-job", Units: []protocol.JobUnit{poolUnit("a", "echo a"), poolUnit("b", "echo b"), poolUnit("c", "echo c")}})
		done <- result
	}()
	waitFor(t, wire, "a re-offer racing a worker", func() bool { return wire.racing == 0 })
	// The worker that took the unit starts it at once, as a real one does; the two the drop took come back on their own.
	wire.mutex.Lock()
	before := len(wire.priority["codex"])
	wire.mutex.Unlock()
	servePool(t, wire, "codex", 2)
	result := <-done
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	wire.mutex.Lock()
	after := append([]int{}, wire.priority["codex"][before:]...)
	wire.mutex.Unlock()
	if len(after) != 2 || after[0] <= 30 || after[1] <= 30 {
		t.Fatalf("after the race the pool took batches at %v: the two dropped units, each on its own, above 30", after)
	}
	if counts := startedCounts(result.Events); counts["a"] != 1 || counts["b"] != 1 || counts["c"] != 1 {
		t.Fatalf("started %v, not each unit once", counts)
	}
}

// Judge's seam: a run's log read back is the record the coordinator wrote, event for event, page by page.
func TestReadRunEventsIsTheRecord(t *testing.T) {
	wire := newFakeWire(t)
	result := run(t, config(wire), shell("a", "echo a"), shell("b", "echo b"))
	read, err := ReadRunEvents(context.Background(), wire.server.URL, testSecret, result.Run)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, result.Events) {
		t.Fatalf("read %d events, the record holds %d", len(read), len(result.Events))
	}
	if FutureRun(strings.Repeat("c", 40), 2) != "future-"+strings.Repeat("c", 40)+"-2" || !protocol.RunIdPattern.MatchString(FutureRun(strings.Repeat("c", 40), 99)) {
		t.Fatalf("FutureRun %q isn't a run id", FutureRun(strings.Repeat("c", 40), 2))
	}
}

// Judge's seam: a unit rerun alone goes to the pool at RerunPriority, uncached, its id its unitKey, and the caller's
// pool keeps its own priority.
func TestRerunAloneTakesTheTopTierAndLeavesThePoolAsItWas(t *testing.T) {
	wire := newFakeWire(t)
	servePool(t, wire, "codex", 1)
	slots := poolSlots(wire, 2)
	slots[0].(*PoolMachine).Priority = 30
	unitKey := strings.Repeat("ab", 32)
	// A cached pass for the same unit must not stand in for the rerun: Judge reruns to see it fail or pass again.
	unit := poolUnit(unitKey, "echo again")
	unit.Cache = true
	if first := run(t, config(wire, slots...), unit); first.Verdict.Status != "green" {
		t.Fatalf("the first run: %+v", first.Verdict)
	}
	wire.mutex.Lock()
	cached, queuedBefore := len(wire.cache), len(wire.priority["codex"])
	wire.mutex.Unlock()
	if cached == 0 {
		t.Fatal("the first run cached nothing, so this test couldn't see a rerun served from the cache")
	}
	result, err := RerunAlone(context.Background(), config(wire, slots...), unit)
	if err != nil || result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v, %v", result.Verdict, err)
	}
	wire.mutex.Lock()
	priorities := append([]int{}, wire.priority["codex"][queuedBefore:]...)
	wire.mutex.Unlock()
	if !reflect.DeepEqual(priorities, []int{RerunPriority}) || slots[0].(*PoolMachine).Priority != 30 {
		t.Fatalf("queued at %v, the caller's pool now at %d", priorities, slots[0].(*PoolMachine).Priority)
	}
	if startedCounts(result.Events)[unitKey] != 1 || !strings.HasPrefix(result.Run, "rerun-abababababab") {
		t.Fatalf("run %s, events %+v", result.Run, result.Events)
	}
}

// A pipeline future's units run under the id Judge reads them by, a second run under it is refused, and an id the wire
// won't take is refused.
func TestARunTakesTheIdItIsGiven(t *testing.T) {
	wire := newFakeWire(t)
	settings := config(wire)
	settings.Run = FutureRun(strings.Repeat("e", 40), 1)
	result := run(t, settings, shell("a", "echo a"))
	if result.Run != settings.Run || len(wire.events(settings.Run)) == 0 {
		t.Fatalf("ran as %q, the wire holds %d events under %q", result.Run, len(wire.events(settings.Run)), settings.Run)
	}
	// The same id again finds its plan set: another coordinator's run, so this one queues nothing.
	queued := len(wire.events(settings.Run))
	if _, err := Run(context.Background(), settings, protocol.Job{Name: "j", Units: []protocol.JobUnit{shell("a", "echo a")}}); err == nil || !strings.Contains(err.Error(), "already has its plan") {
		t.Fatalf("a second run under %s wasn't refused (%v)", settings.Run, err)
	}
	if len(wire.events(settings.Run)) != queued {
		t.Fatal("the refused run posted events")
	}
	// A plan post that set the plan and lost its answer is retried, and the retry's 200 is its own plan: it runs.
	wire.lostPlans = 1
	settings.Run = FutureRun(strings.Repeat("e", 40), 2)
	if result := run(t, settings, shell("a", "echo a")); result.Run != settings.Run || len(wire.events(settings.Run)) == 0 {
		t.Fatalf("a retried plan post refused its own run %s", settings.Run)
	}
	settings.Run = "not a run/id"
	if _, err := Run(context.Background(), settings, protocol.Job{Name: "j", Units: []protocol.JobUnit{shell("a", "true")}}); err == nil {
		t.Fatal("a run id the wire won't take was accepted")
	}
}

// A strict runner says nothing while a package's go test runs, so its pool gives a started unit a silence window
// (--silence-drop) in place of three heartbeats (Oct 10: live units over 360 s were dropped all night). A worker silent
// for six of its heartbeats that then passes is never dropped. Mutant: the window ignored, and the unit is dropped and
// placed again.
func TestAStrictPoolsUnitSilentPastThreeHeartbeatsIsNotDropped(t *testing.T) {
	wire := newFakeWire(t)
	wire.sleepers = 1
	servePool(t, wire, "codex", 1)
	pool := &PoolMachine{Pool: "codex", Wire: wire.server.URL, Secret: testSecret, Version: runner.Version,
		GoPlatform: runtime.GOOS + "/" + runtime.GOARCH, QueueCheck: 50 * time.Millisecond, SilenceDrop: 5 * time.Second}
	unit := poolUnit("quiet", "echo never")
	unit.TimeoutSeconds = 30
	result := run(t, config(wire, pool), unit)
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	inRecord := unitEventsOf(result.Events, "quiet")
	if startedCounts(inRecord)["quiet"] != 1 || strings.Contains(fmt.Sprint(inRecord), "its worker is gone") {
		t.Fatalf("a silent unit that passed was dropped: %+v", inRecord)
	}
}
