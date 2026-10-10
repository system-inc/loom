package placer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
)

// A run that won't start stops the pass there: one start is tried a pass, the attempt stays unplaced and is tried
// again, and its void is held by cause, so a cause every start meets can't storm Queue with voids or the host with
// starts. Mutant: the pass going on after a failed start (two starts, two voids in the first pass).
func TestARunThatWontStartStopsThePassAndIsHeld(t *testing.T) {
	other := strings.Repeat("9", 40)
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)}, {Future: other, Base: base, Attempt: 1, Units: everyKind(t)}}
	ledger := &MemoryLedger{}
	h := newHarness(t, source, ledger)
	starts := map[string]int{}
	h.placer.Start = func(placement Placement) error {
		starts[placement.Future]++
		return errors.New("fork/exec loom: no such file or directory")
	}
	if _, err := h.placer.PlaceOnce(); err == nil {
		t.Fatal("a failed start passed silently")
	}
	if starts[tree] != 1 || starts[other] != 0 || len(h.voids) != 1 || !strings.Contains(h.voids[0], "didn't start: fork/exec") {
		t.Fatalf("starts %v, voids %v: want one start and its void, the pass stopped", starts, h.voids)
	}
	// Queue lists attempt 2; the same cause within the window is held, and still one start a pass.
	source[0].Attempt = 2
	h.now = h.now.Add(time.Minute)
	h.placer.PlaceOnce()
	if starts[tree] != 2 || starts[other] != 0 || len(h.voids) != 1 {
		t.Fatalf("starts %v, voids %v: want attempt 2 tried once and held", starts, h.voids)
	}
	if record, _ := ledger.Find(tree, 2); record.placed() {
		t.Fatalf("attempt 2 never started and counts as placed: %+v", record)
	}
}

// A placed run whose loom run ends before the run's first event ran nothing: the attempt is voided as Loom's with the
// log's tail at once, never left to the judge's 45-minute backstop. One that ended after its events were posted is
// only recorded. Mutant: an early exit recorded and never voided.
func TestARunThatEndsBeforeItsFirstEventIsVoided(t *testing.T) {
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)}}
	ledger := &MemoryLedger{}
	h := newHarness(t, source, ledger)
	exits := make(chan Exit, 2)
	h.placer.Exits = exits
	started := false
	h.placer.RunStarted = func(run string) (bool, error) { return started, nil }
	h.placeOnce(t, 1)
	run := h.placements[0].Run
	exits <- Exit{Future: tree, Attempt: 1, Run: run, Status: "exit 3", Tail: "loom: run " + run + " already has its plan"}
	h.placeOnce(t, 0)
	if len(h.voids) != 1 || !strings.Contains(h.voids[0], "ended (exit 3) before the run's first event") || !strings.Contains(h.voids[0], "already has its plan") {
		t.Fatalf("voids %v", h.voids)
	}
	if record, _ := ledger.Find(tree, 1); record.Exit != "exit 3" || record.Void == "" {
		t.Fatalf("the ledger holds %+v", record)
	}
	source[0].Attempt, started = 2, true
	h.placeOnce(t, 1)
	exits <- Exit{Future: tree, Attempt: 2, Run: h.placements[1].Run, Status: "exit 0"}
	h.placeOnce(t, 0)
	if record, _ := ledger.Find(tree, 2); len(h.voids) != 1 || record.Exit != "exit 0" || record.EarlyExit != "" {
		t.Fatalf("a run that posted its events was voided (%v) or not recorded (%+v)", h.voids, record)
	}
}

// A future whose reads keep failing is backed off between tries, and voided as Loom's once they have failed for
// UnfitEvery, naming the read. Mutant: the reads retried every pass and never voided.
func TestAFutureWhoseReadsKeepFailingIsBackedOffThenVoided(t *testing.T) {
	source := listedFutures{{Future: tree, Base: base, Attempt: 2, Units: everyKind(t)}}
	h := newHarness(t, source, &MemoryLedger{})
	asked := 0
	h.placer.Carried = func(judge.PlannedFuture, int) ([]judge.CarriedUnit, error) {
		asked++
		return nil, errors.New("reading run future-x-1: 502 Bad Gateway")
	}
	for minute := 0; minute <= 40; minute++ {
		h.now = h.now.Add(time.Minute)
		h.placer.PlaceOnce()
		if len(h.voids) > 0 {
			break
		}
	}
	if asked > 8 {
		t.Errorf("asked %d times in 40 minutes, want backed off", asked)
	}
	if len(h.voids) != 1 || !strings.Contains(h.voids[0], "its reads failed for") || !strings.Contains(h.voids[0], "502 Bad Gateway") {
		t.Fatalf("voids %v after %d asks", h.voids, asked)
	}
}

// An attempt whose plan Queue replaced after it was placed can't run the new plan under its run id (the wire holds
// the first): it is voided, so the next attempt runs it, and never started twice. Mutant: the plan hash not compared.
func TestAnAttemptPlannedAgainAfterItWasPlacedIsVoided(t *testing.T) {
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)}}
	h := newHarness(t, source, &MemoryLedger{})
	h.placeOnce(t, 1)
	h.placeOnce(t, 0)
	source[0].Units = source[0].Units[:2]
	h.placeOnce(t, 0)
	if len(h.placements) != 1 || len(h.voids) != 1 || !strings.Contains(h.voids[0], "its plan changed after future-"+tree+"-1") {
		t.Fatalf("placements %d, voids %v", len(h.placements), h.voids)
	}
}

// A record whose line didn't reach the ledger's file is never held (jsonlines cuts a half-written line back). Mutant:
// the record held before its write.
func TestARecordThatDidntReachTheLedgerIsntHeld(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placed.jsonl")
	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	if err := ledger.Append(Record{Future: tree, Attempt: 1, Run: "r1"}); err != nil {
		t.Fatal(err)
	}
	// A directory where the file was fails every write, for root too.
	os.Remove(path)
	os.Mkdir(path, 0o700)
	if err := ledger.Append(Record{Future: tree, Attempt: 2, Run: "r2"}); err == nil {
		t.Fatal("a write that couldn't happen passed")
	}
	if _, found := ledger.Find(tree, 2); found {
		t.Fatal("a record that didn't reach the file is held")
	}
}

// A warm runner's test goes only to a pool the judge would read as cold: marked, naming its machines, cold since a
// readable time already past, and sharing no machine with an unmarked pool. Mutant: the cold mark alone.
func TestAWarmRunnersTestGoesOnlyWhereTheJudgeReadsCold(t *testing.T) {
	cases := []struct {
		name  string
		pools func([]Pool) []Pool
		fits  bool
	}{
		{"cold with machines since an hour ago", func(pools []Pool) []Pool { return pools }, true},
		{"marked cold, naming no machine", func(pools []Pool) []Pool { pools[0].Machines = nil; return pools }, false},
		{"cold since a time not yet come", func(pools []Pool) []Pool { pools[0].ColdSince = "2026-10-10T10:00:00Z"; return pools }, false},
		{"cold since a time unreadable", func(pools []Pool) []Pool { pools[0].ColdSince = "this morning"; return pools }, false},
		{"its machine is also an unmarked pool's", func(pools []Pool) []Pool {
			pools[1].Machines = []string{"cloud-box-2"}
			return pools
		}, false},
	}
	for _, c := range cases {
		pools := fixturePools()
		pools[0].Machines, pools[0].ColdSince = []string{"cloud-box-2"}, "2026-10-10T08:00:00Z"
		pools = c.pools(pools)
		h := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)[:1]}}, &MemoryLedger{})
		h.placer.Pools = func() ([]Pool, error) { return pools, nil }
		h.placer.WarmRunners = map[string]bool{testRunner: true}
		h.placer.PlaceOnce()
		if placed := len(h.placements) == 1; placed != c.fits {
			t.Errorf("%s: placed %v, voids %v", c.name, placed, h.voids)
		}
	}
}

// A listed future that isn't a tree sha places nothing and never panics. The ledger keeps every listed future's
// attempts, and drops an unlisted one's once it's Keep old. Mutant: listed futures dropped by age too.
func TestTheLedgerKeepsListedFuturesAndDropsOldUnlistedOnes(t *testing.T) {
	h := newHarness(t, listedFutures{{Future: "abc", Attempt: 1}}, &MemoryLedger{})
	h.placeOnce(t, 0)
	ledger := &MemoryLedger{}
	old, recent := h.now.Add(-8*24*time.Hour).Format(time.RFC3339), h.now.Add(-time.Hour).Format(time.RFC3339)
	listed, gone, fresh := strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40)
	ledger.Append(Record{Future: listed, Attempt: 1, At: old})
	ledger.Append(Record{Future: gone, Attempt: 1, At: old, Void: "not placed: x"})
	ledger.Append(Record{Future: fresh, Attempt: 1, At: recent})
	h = newHarness(t, listedFutures{{Future: listed, Attempt: 1, Units: everyKind(t)}}, ledger)
	h.placer.Keep = 7 * 24 * time.Hour
	h.placeOnce(t, 0)
	_, keptListed := ledger.Find(listed, 1)
	_, keptGone := ledger.Find(gone, 1)
	_, keptFresh := ledger.Find(fresh, 1)
	_, voidKept := ledger.LastVoid(gone)
	if !keptListed || keptGone || !keptFresh || voidKept {
		t.Fatalf("kept listed %v, gone %v (void %v), fresh %v", keptListed, keptGone, voidKept, keptFresh)
	}
}

type failingFutures struct{}

func (failingFutures) Planned() ([]judge.PlannedFuture, error) { return nil, errors.New("Queue: 503") }

// When every run ends before its first event, each with its own log tail (a broken loom, a wire refusing every plan),
// the voids are held by exit status alone and a pass voids at most one future: no future is cycled every pass.
// Mutants: the tail in the hold key (every attempt voided again), the pass going on after an early exit's void.
func TestRunsThatAllEndEarlyNeverCycleTheFutures(t *testing.T) {
	trees := []string{strings.Repeat("a", 40), strings.Repeat("c", 40), strings.Repeat("e", 40)}
	source := listedFutures{}
	for _, tree := range trees {
		source = append(source, judge.PlannedFuture{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)})
	}
	h := newHarness(t, source, &MemoryLedger{})
	exits := make(chan Exit, 64)
	h.placer.Exits = exits
	h.placer.RunStarted = func(string) (bool, error) { return false, nil }
	placed := map[string]int{}
	h.placer.Start = func(placement Placement) error {
		placed[placement.Future]++
		exits <- Exit{Future: placement.Future, Attempt: placement.Attempt, Run: placement.Run, Status: "exit status 3",
			Tail: fmt.Sprintf("loom: run %s refused at %s", placement.Run, h.now.Format(time.RFC3339Nano))}
		return nil
	}
	h.placer.Void = func(future judge.PlannedFuture, attempt int, cause string) error {
		h.voids = append(h.voids, cause)
		for index := range source {
			if source[index].Future == future.Future {
				source[index].Attempt++ // Queue counts the void and lists the next attempt
			}
		}
		return nil
	}
	for range 25 {
		before := len(h.voids)
		h.now = h.now.Add(time.Minute)
		h.placer.PlaceOnce()
		if len(h.voids)-before > 1 {
			t.Fatalf("one pass voided %d futures", len(h.voids)-before)
		}
	}
	for _, tree := range trees {
		if placed[tree] > 2 {
			t.Errorf("future %.8s placed %d times in 25 minutes, want its first attempt and one more, held", tree, placed[tree])
		}
	}
	if len(h.voids) != len(trees) {
		t.Fatalf("voids %d, want one a future", len(h.voids))
	}
}

// Ended runs are taken off the channel even when Queue won't answer, so an outage never blocks the runs' reporters.
// Mutant: the channel drained only after the listing is read.
func TestEndedRunsAreDrainedThroughAnOutage(t *testing.T) {
	h := newHarness(t, failingFutures{}, &MemoryLedger{})
	exits := make(chan Exit, 1)
	h.placer.Exits = exits
	exits <- Exit{Future: tree, Attempt: 1, Run: "future-" + tree + "-1", Status: "exit status 3"}
	if _, err := h.placer.PlaceOnce(); err == nil {
		t.Fatal("the outage passed silently")
	}
	if len(exits) != 0 || len(h.placer.exited) != 1 {
		t.Fatalf("%d left on the channel, %d held", len(exits), len(h.placer.exited))
	}
}
