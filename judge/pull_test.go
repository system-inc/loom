package judge

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

type listedFutures []PlannedFuture

func (futures listedFutures) Planned() ([]PlannedFuture, error) { return futures, nil }

func finishedStream(unit, status string) []protocol.Event {
	return []protocol.Event{{Unit: unit, Type: "started"}, {Unit: unit, Type: "exit", Code: code(map[string]int{"passed": 0, "failed": 1}[status])},
		{Unit: unit, Type: "output", Text: testLine(map[string]string{"passed": "pass", "failed": "fail"}[status], "TestB")},
		{Unit: unit, Type: "finished", Status: status}}
}

func TestThePullerJudgesOnlyFinishedFuturesAndRerunsByKeyParts(t *testing.T) {
	done, running := strings.Repeat("d", 40), strings.Repeat("e", 40)
	unit, reused := strings.Repeat("1", 64), strings.Repeat("2", 64)
	units := []PlannedUnitWire{{UnitKey: unit, KeyParts: json.RawMessage(`{"package":"p"}`), Decision: "run"}, {UnitKey: reused, Decision: "reuse"}}
	change := PlannedChange{Change: "chg_A", Sha: done, Base: baseTree, Owner: "system_adamic_library"}
	source := listedFutures{{Future: done, Base: baseTree, Change: change, Units: units}, {Future: running, Base: baseTree, Change: change, Units: units}}
	streams := map[string][]protocol.Event{"future-" + done + "-1": finishedStream(unit, "failed"), "future-" + running + "-1": {{Unit: unit, Type: "started"}}}
	reruns := []string{}
	queue := &StubQueue{}
	puller := Puller{
		Source: source,
		RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
		Read:   func(run string) ([]protocol.Event, error) { return streams[run], nil },
		Rerun: func(keyParts json.RawMessage, _ protocol.Resources, sha string) ([]protocol.Event, error) {
			reruns = append(reruns, string(keyParts)+"@"+sha[:1])
			if sha == baseTree {
				return finishedStream("job-on-base", "passed"), nil
			}
			return finishedStream("job-on-candidate", "failed"), nil
		},
		Main:  NoMainRecords{},
		Queue: queue,
		Loop:  Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: func() time.Time { return time.Date(2026, 10, 9, 23, 58, 0, 0, time.UTC) }},
	}
	judged, err := puller.PullOnce()
	if err != nil || judged != 1 {
		t.Fatalf("judged %d %v, want the one finished future", judged, err)
	}
	if len(queue.Posts[running]) != 0 || len(queue.Posts[done]) != 1 {
		t.Fatalf("posts %v, want only the finished future's", queue.Posts)
	}
	post := queue.Posts[done][0]
	if post.Decision.Status != "red" || post.Run != "future-"+done+"-1" || len(post.Decision.Kicks) != 1 {
		t.Fatalf("post %+v", post.Decision)
	}
	if strings.Join(reruns, ",") != `{"package":"p"}@d,{"package":"p"}@b` {
		t.Fatalf("reruns %v, want the unit's keyParts on the candidate then on main's base", reruns)
	}
}

func TestNoMainRecordsNeverExcusesAFailure(t *testing.T) {
	if _, found, err := (NoMainRecords{}).Latest(baseTree, strings.Repeat("1", 64)); found || err != nil {
		t.Fatal("the stand-in claimed a main record")
	}
}

func TestVoidOnePostsTheListedAttemptOnlyAndNeverReruns(t *testing.T) {
	stopped := strings.Repeat("d", 40)
	unit, never := strings.Repeat("1", 64), strings.Repeat("3", 64)
	units := []PlannedUnitWire{{UnitKey: unit, Decision: "run"}, {UnitKey: never, Decision: "run"}}
	source := listedFutures{{Future: stopped, Base: baseTree, Change: PlannedChange{Change: "chg_A"}, Units: units}}
	queue := &StubQueue{}
	puller := Puller{
		Source: source,
		RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
		Read:   func(run string) ([]protocol.Event, error) { return finishedStream(unit, "passed"), nil },
		Rerun: func(json.RawMessage, protocol.Resources, string) ([]protocol.Event, error) {
			t.Fatal("a void reran a unit")
			return nil, nil
		},
		Main: NoMainRecords{}, Queue: queue, Loop: Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: time.Now},
	}
	if _, err := puller.VoidOne(stopped, 2, "stopped"); err == nil {
		t.Fatal("voided attempt 2 while Queue lists attempt 1")
	}
	if _, err := puller.VoidOne(strings.Repeat("e", 40), 1, "stopped"); err == nil {
		t.Fatal("voided a future Queue doesn't list")
	}
	post, err := puller.VoidOne(stopped, 1, "an operator's stop killed its loom run")
	if err != nil {
		t.Fatal(err)
	}
	if post.Run != "future-"+stopped+"-1" || post.Decision.Status != Void || len(queue.Posts[stopped]) != 1 {
		t.Fatalf("post %+v, want run -1 void, posted once", post)
	}
}

func TestTheBackstopVoidsOnlyARunQuietForFortyFiveMinutes(t *testing.T) {
	now := time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)
	at := func(minutesAgo int) string {
		return now.Add(-time.Duration(minutesAgo) * time.Minute).Format(time.RFC3339Nano)
	}
	tree := strings.Repeat("d", 40)
	done, open := strings.Repeat("1", 64), strings.Repeat("3", 64)
	units := []PlannedUnitWire{{UnitKey: done, Decision: "run"}, {UnitKey: open, Decision: "run"}}
	cases := []struct {
		name   string
		events []protocol.Event
		voided bool
	}{
		{"its last unit finished 44 minutes ago, after a start 100 minutes ago: never voided", []protocol.Event{
			{Unit: done, Type: "started", Time: at(100)}, {Unit: open, Type: "started", Time: at(99)}, {Unit: done, Type: "finished", Status: "passed", Time: at(44)}}, false},
		{"its newest event is 46 minutes old: void", []protocol.Event{
			{Unit: done, Type: "started", Time: at(100)}, {Unit: open, Type: "started", Time: at(99)}, {Unit: done, Type: "finished", Status: "passed", Time: at(46)}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			queue := &StubQueue{}
			puller := NewPuller(Puller{
				Source: listedFutures{{Future: tree, Base: baseTree, Change: PlannedChange{Change: "chg_A"}, Units: units}},
				RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-1" },
				Read:   func(string) ([]protocol.Event, error) { return c.events, nil },
				Rerun: func(json.RawMessage, protocol.Resources, string) ([]protocol.Event, error) {
					t.Fatal("the backstop placed a unit")
					return nil, nil
				},
				Main: NoMainRecords{}, Queue: queue, Loop: Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: func() time.Time { return now }}, Stale: StaleAfter,
			})
			judged, err := puller.PullOnce()
			if err != nil {
				t.Fatal(err)
			}
			if voided := len(queue.Posts[tree]) == 1; voided != c.voided || judged != map[bool]int{true: 1, false: 0}[c.voided] {
				t.Fatalf("voided %v (judged %d), want %v", voided, judged, c.voided)
			}
			if c.voided {
				post := queue.Posts[tree][0]
				if post.Decision.Status != Void || !strings.HasPrefix(post.Decision.Problems[0], "run future-"+tree+"-1 void: silent: no event for 46 min with 1 units open") {
					t.Fatalf("decision %+v", post.Decision)
				}
				if record := recordOf(t, post, open); record.Infra != InfraSilent {
					t.Fatalf("open unit %+v, want void silent", record)
				}
			}
		})
	}
}

func TestARunWithNoEventsIsAgedFromTheFirstPassThatSawIt(t *testing.T) {
	now := time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)
	tree := strings.Repeat("d", 40)
	queue := &StubQueue{}
	puller := NewPuller(Puller{
		Source: listedFutures{{Future: tree, Change: PlannedChange{Change: "chg_A"}, Units: []PlannedUnitWire{{UnitKey: strings.Repeat("1", 64), Decision: "run"}}}},
		RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-1" },
		Read:   func(string) ([]protocol.Event, error) { return nil, nil },
		Main:   NoMainRecords{}, Queue: queue, Loop: Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: func() time.Time { return now }}, Stale: StaleAfter,
	})
	for _, minutes := range []int{0, 44, 45} {
		now = time.Date(2026, 10, 10, 2, minutes, 0, 0, time.UTC)
		if _, err := puller.PullOnce(); err != nil {
			t.Fatal(err)
		}
		if posted := len(queue.Posts[tree]); posted != map[bool]int{true: 1, false: 0}[minutes == 45] {
			t.Fatalf("at minute %d, %d posts", minutes, posted)
		}
	}
	off := puller
	off.Stale = 0
	if why := off.stale("run", nil, 1); why != "" {
		t.Fatalf("a backstop turned off said %q", why)
	}
}

func TestAnEarlierAttemptsPassIsCarriedOnlyWithinTheSameFuture(t *testing.T) {
	tree, other := strings.Repeat("d", 40), strings.Repeat("e", 40)
	carried, placed, redBefore := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	runOf := func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) }
	listing := func(units ...string) listedFutures {
		wire := []PlannedUnitWire{}
		for _, unit := range units {
			wire = append(wire, PlannedUnitWire{UnitKey: unit, Decision: "run"})
		}
		return listedFutures{{Future: tree, Base: other, Attempt: 2, Change: PlannedChange{Change: "chg_A"}, Units: wire}}
	}
	streams := map[string][]protocol.Event{
		runOf(tree, 1):  append(finishedStream(carried, "passed"), finishedStream(redBefore, "failed")...),
		runOf(tree, 2):  finishedStream(placed, "passed"),
		runOf(other, 1): finishedStream(redBefore, "passed"), // a pass on another future, never carried
	}
	pullerFor := func(source listedFutures, queue *StubQueue) Puller {
		return NewPuller(Puller{Source: source, RunOf: runOf, Read: func(run string) ([]protocol.Event, error) { return streams[run], nil },
			Rerun: func(json.RawMessage, protocol.Resources, string) ([]protocol.Event, error) {
				t.Fatal("placed a unit")
				return nil, nil
			},
			Main: NoMainRecords{}, Queue: queue, Loop: Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: time.Now}})
	}
	queue := &StubQueue{}
	if judged, err := pullerFor(listing(carried, placed), queue).PullOnce(); err != nil || judged != 1 {
		t.Fatalf("judged %d (%v), want the future with its carried unit", judged, err)
	}
	post := queue.Posts[tree][0]
	if post.Decision.Status != "green" || post.Run != runOf(tree, 2) {
		t.Fatalf("decision %+v run %s", post.Decision, post.Run)
	}
	if !strings.Contains(string(post.Verdicts[0]), `"rule":"judge-v1 carried `+runOf(tree, 1)+`"`) || !strings.Contains(string(post.Verdicts[0]), `"run":"`+runOf(tree, 2)+`"`) {
		t.Fatalf("carried record %s: want its source run in the rule and this run as its run", post.Verdicts[0])
	}
	if !strings.Contains(string(post.Verdicts[1]), `"rule":"judge-v1"`) {
		t.Fatalf("placed record %s", post.Verdicts[1])
	}
	// A unit whose only earlier attempt failed here, and passed only on another future, is open: nothing is judged.
	queue = &StubQueue{}
	if judged, err := pullerFor(listing(redBefore, placed), queue).PullOnce(); err != nil || judged != 0 || len(queue.Posts) != 0 {
		t.Fatalf("judged %d (%v) with a unit whose only pass was on another future", judged, err)
	}
}

func TestCarriedListsTheUnitsAndSourceRunsTheLoopCarries(t *testing.T) {
	tree := strings.Repeat("d", 40)
	early, late, red, killed := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)
	runOf := func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) }
	killedPass := []protocol.Event{{Unit: killed, Type: "started"}, {Unit: killed, Type: "exit", Code: code(0), Signal: "killed"}, {Unit: killed, Type: "finished", Status: "passed"}}
	streams := map[string][]protocol.Event{
		runOf(tree, 1): append(append(finishedStream(early, "passed"), finishedStream(late, "passed")...), killedPass...),
		runOf(tree, 2): append(finishedStream(late, "passed"), finishedStream(red, "failed")...),
	}
	units := []PlannedUnitWire{}
	for _, unit := range []string{early, late, red, killed} {
		units = append(units, PlannedUnitWire{UnitKey: unit, Decision: "run"})
	}
	puller := Puller{Source: listedFutures{{Future: tree, Attempt: 3, Units: units}}, RunOf: runOf,
		Read: func(run string) ([]protocol.Event, error) { return streams[run], nil }}
	got, err := puller.Carried(tree, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != (CarriedUnit{early, runOf(tree, 1)}) || got[1] != (CarriedUnit{late, runOf(tree, 2)}) {
		t.Fatalf("carried %+v: want early from attempt 1 and late from its newest pass, attempt 2; never the red or the killed pass", got)
	}
	if _, err := puller.Carried(strings.Repeat("e", 40), 3); err == nil {
		t.Fatal("listed carried units for a future Queue doesn't list")
	}
}

func TestAnEmptyFutureIsLeftAndOneFuturesErrorNeverStopsTheRest(t *testing.T) {
	docs, broken, ready := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	unit := strings.Repeat("1", 64)
	units := []PlannedUnitWire{{UnitKey: unit, Decision: "run"}}
	queue := &StubQueue{}
	puller := NewPuller(Puller{
		Source: listedFutures{{Future: docs, Empty: true, Change: PlannedChange{Change: "chg_D"}}, {Future: broken, Units: units, Change: PlannedChange{Change: "chg_B"}},
			{Future: ready, Units: units, Change: PlannedChange{Change: "chg_R"}}},
		RunOf: func(tree string, attempt int) string { return "future-" + tree + "-1" },
		Read: func(run string) ([]protocol.Event, error) {
			if strings.Contains(run, broken) {
				return nil, errors.New("wire down")
			}
			return finishedStream(unit, "passed"), nil
		},
		Main: NoMainRecords{}, Queue: queue, Loop: Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: time.Now},
	})
	judged, err := puller.PullOnce()
	if judged != 1 || len(queue.Posts[ready]) != 1 || err == nil || !strings.Contains(err.Error(), "future "+broken) {
		t.Fatalf("judged %d, posts %v, error %v: want the ready future judged past the broken one's error", judged, queue.Posts, err)
	}
	if len(queue.Posts[docs]) != 0 {
		t.Fatal("an empty docs future was posted by judge-v1")
	}
}

func TestThePullerReadsAUnitsKindAndRunnerFromItsKey(t *testing.T) {
	tree, unit := strings.Repeat("d", 40), strings.Repeat("1", 64)
	pull := func(ranOn string) FuturePost {
		queue := &StubQueue{}
		puller := NewPuller(Puller{
			Source: listedFutures{{Future: tree, Change: PlannedChange{Change: "chg_A"}, Units: []PlannedUnitWire{{UnitKey: unit,
				KeyParts: json.RawMessage(`{"kind":"phase","tools":{"runner":"` + strings.Repeat("a", 64) + `"}}`), Decision: "run"}}}},
			RunOf: func(tree string, attempt int) string { return "future-" + tree + "-1" },
			Read: func(string) ([]protocol.Event, error) {
				return []protocol.Event{{Unit: unit, Type: "started", RunnerSha256: ranOn}, {Unit: unit, Type: "exit", Code: code(1)}, {Unit: unit, Type: "finished", Status: "failed"}}, nil
			},
			// A phase red is never rerun alone; a void attempt is placed again, and this placement never reports.
			Rerun: func(json.RawMessage, protocol.Resources, string) ([]protocol.Event, error) { return nil, nil },
			Main:  NoMainRecords{}, Queue: queue, Loop: Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: time.Now, RequireTestLog: true},
		})
		if judged, err := puller.PullOnce(); err != nil || judged != 1 {
			t.Fatalf("judged %d (%v)", judged, err)
		}
		return queue.Posts[tree][0]
	}
	if post := pull(strings.Repeat("a", 64)); post.Decision.Status != "red" {
		t.Fatalf("decision %+v: want the phase red by its exit on its key's runner", post.Decision)
	}
	if post := pull(strings.Repeat("b", 64)); post.Decision.Status != "void" {
		t.Fatalf("decision %+v: want void, on a runner other than keyParts.tools.runner", post.Decision)
	}
}

func TestARerunsNeedIsTheListingsElseUnitNeeds(t *testing.T) {
	needs := planner.UnitNeeds{Units: []planner.UnitNeed{{Package: "stage1/cohere/typeaware", MemoryMegabytes: 32768, Cpus: 4, Record: "r"}}}
	typeaware := json.RawMessage(`{"package":"github.com/system-inc/adamic/stage1/cohere/typeaware","select":{"run":""}}`)
	if need, err := NeedOf(protocol.Resources{}, typeaware, needs); err != nil || need.MemoryMegabytes != 32768 {
		t.Fatalf("need %+v (%v), want unit-needs' 32768 MB for a plan that carried none", need, err)
	}
	if need, _ := NeedOf(protocol.Resources{MemoryMegabytes: 2048, Cpus: 1}, typeaware, needs); need.MemoryMegabytes != 2048 {
		t.Fatalf("need %+v, want the listing's own", need)
	}
	if need, err := NeedOf(protocol.Resources{}, json.RawMessage(`{"package":"github.com/system-inc/adamic/internal/oracle"}`), needs); err != nil || need != (protocol.Resources{}) {
		t.Fatalf("need %+v (%v), want none declared", need, err)
	}
	// A need no pool holds is still the unit's need; refusing it is placement's (the next test), as void.
	needs.Units[0].MemoryMegabytes = 131072
	if need, err := NeedOf(protocol.Resources{}, typeaware, needs); err != nil || need.MemoryMegabytes != 131072 {
		t.Fatalf("need %+v (%v), want the declared 131072 MB for placement to refuse", need, err)
	}
}

func TestARerunGoesOnlyToAPoolServingItsRunnerThatHoldsItsNeed(t *testing.T) {
	content, err := os.ReadFile("testdata/pools.json")
	if err != nil {
		t.Fatal(err)
	}
	pools, err := LoadPools(content)
	if err != nil || len(pools) != 5 {
		t.Fatalf("pools %+v (%v)", pools, err)
	}
	keyRunner := "8a70ebce11315bce6395da08b0226f32f592cbb747d4f329438594bf7056b7f0"
	names := func(fit []PoolEntry) string {
		list := []string{}
		for _, pool := range fit {
			list = append(list, pool.Name)
		}
		return strings.Join(list, ",")
	}
	if got := names(FitPools(pools, "test", keyRunner, protocol.Resources{})); got != "codex-strict,box-strict-8a70" {
		t.Fatalf("fit %s, want both pools serving its key's runner", got)
	}
	if got := names(FitPools(pools, "test", keyRunner, protocol.Resources{MemoryMegabytes: 32768, Cpus: 4})); got != "box-strict-8a70" {
		t.Fatalf("fit %s, want only the box serving its runner for a 32 GB need", got)
	}
	if got := FitPools(pools, "test", keyRunner, protocol.Resources{MemoryMegabytes: 131072}); len(got) != 0 {
		t.Fatalf("fit %v for a need no pool holds", got)
	}
	// A phase pool serving the same runner never takes a test rerun, even one only it could hold, and an empty key
	// runner doesn't let a test reach it either; a phase goes only there.
	if got := FitPools(pools, "test", keyRunner, protocol.Resources{MemoryMegabytes: 32768, Cpus: 16}); len(got) != 0 {
		t.Fatalf("fit %v: a test rerun went to the phase pool", got)
	}
	for _, pool := range FitPools(pools, "test", "", protocol.Resources{}) {
		if pool.Name == "box-phase-8a70" {
			t.Fatal("a test rerun with no key runner went to the phase pool")
		}
	}
	if got := names(FitPools(pools, "phase", keyRunner, protocol.Resources{})); got != "box-phase-8a70" {
		t.Fatalf("fit %s, want only the phase pool for a phase", got)
	}
	if _, err := LoadPools([]byte(`{"pools":[{"name":"p","runner":"","memoryMegabytes":1,"cpus":1}]}`)); err == nil {
		t.Fatal("read a pool with no runner")
	}
}

func TestARerunThatCantBePlacedIsVoidNeverAnError(t *testing.T) {
	h := newHarness()
	h.runs["u"] = failedWith("TestB")
	loop := Loop{Runs: h.runs, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: time.Now,
		Fabric: EventFabric{Rerun: func(string, string) ([]protocol.Event, error) {
			return []protocol.Event{{Type: "error", Phase: protocol.PhasePlace, Message: "not placed: a \"phase\" unit has no job yet"}}, nil
		}}}
	post, err := loop.JudgeFuture(censusJob(PlanUnit{UnitKey: "u"}))
	if err != nil {
		t.Fatalf("an unplaceable rerun stalled the future: %v", err)
	}
	if record := recordOf(t, post, "u"); record.Status != Void || record.Infra != InfraNeverPlaced {
		t.Fatalf("%+v, want void neverPlaced", record)
	}
}

// Release's mutant (Oct 10 02:26Z): a unit that failed placed with no declared need, typeaware on 8 cores, and would
// pass alone at its declared 16 cpus must never read flake or green. It's void, need changed, with no rerun placed; with
// the same need on both sides, today's flake rule is unchanged.
func TestAFailureWhoseNeedGrewIsVoidNeverAFlake(t *testing.T) {
	tree := strings.Repeat("d", 40)
	unit := strings.Repeat("1", 64)
	for _, c := range []struct {
		name   string
		need   protocol.Resources
		status string
		reruns int
	}{
		{"need grew: void, nothing rerun", protocol.Resources{MemoryMegabytes: 9710, Cpus: 16}, "void", 0},
		{"same need: the flake rule stands", protocol.Resources{}, "green", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			units := []PlannedUnitWire{{UnitKey: unit, KeyParts: json.RawMessage(`{"package":"p","kind":"test"}`), Decision: "run"}}
			source := listedFutures{{Future: tree, Base: baseTree, Change: PlannedChange{Change: "chg_A", Sha: tree, Base: baseTree}, Units: units}}
			reruns := 0
			queue := &StubQueue{}
			puller := Puller{
				Source: source,
				RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
				Read:   func(string) ([]protocol.Event, error) { return finishedStream(unit, "failed"), nil },
				Rerun: func(json.RawMessage, protocol.Resources, string) ([]protocol.Event, error) {
					reruns++
					return finishedStream("job", "passed"), nil
				},
				NeedNow: func(json.RawMessage, protocol.Resources) (protocol.Resources, error) { return c.need, nil },
				Main:    NoMainRecords{},
				Queue:   queue,
				Loop:    Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: func() time.Time { return time.Date(2026, 10, 10, 2, 30, 0, 0, time.UTC) }},
			}
			if judged, err := puller.PullOnce(); err != nil || judged != 1 {
				t.Fatalf("judged %d %v", judged, err)
			}
			post := queue.Posts[tree][0]
			if post.Decision.Status != c.status || reruns != c.reruns {
				t.Fatalf("run %s with %d reruns, want %s with %d", post.Decision.Status, reruns, c.status, c.reruns)
			}
			if c.status == "void" {
				var record struct {
					Status string  `json:"status"`
					Infra  *string `json:"infra"`
				}
				if err := json.Unmarshal(post.Verdicts[0], &record); err != nil || record.Status != Void || record.Infra == nil || *record.Infra != InfraNeedChanged {
					t.Fatalf("record %+v (%v), want void needChanged", record, err)
				}
				if len(post.Quarantine) != 0 {
					t.Fatalf("quarantined %v for a need that grew", post.Quarantine)
				}
			}
		})
	}
}

func TestNeedGrewIsMoreCpusOrMemoryThanThePlacement(t *testing.T) {
	if NeedGrew(protocol.Resources{}, protocol.Resources{Cpus: 16, MemoryMegabytes: 9710}) == "" {
		t.Fatal("a 16-cpu need over a placement with none declared didn't grow")
	}
	if NeedGrew(protocol.Resources{Cpus: 4, MemoryMegabytes: 9710}, protocol.Resources{Cpus: 4, MemoryMegabytes: 16384}) == "" {
		t.Fatal("more memory didn't grow")
	}
	if why := NeedGrew(protocol.Resources{Cpus: 16, MemoryMegabytes: 9710}, protocol.Resources{Cpus: 16, MemoryMegabytes: 9710}); why != "" {
		t.Fatalf("the same need grew: %s", why)
	}
	if why := NeedGrew(protocol.Resources{Cpus: 16, MemoryMegabytes: 16384}, protocol.Resources{Cpus: 4}); why != "" {
		t.Fatalf("a smaller need grew: %s", why)
	}
}

// Loom's mutant (Oct 10 02:37Z): f7812fff attempt 4 placed ec123b7f, declared at 16 cpus, on a 4-cpu Codex instance,
// since the coordinator checked memory and not cpus. A failure that ran below its declared need is void, belowNeed,
// never a red or a flake, and nothing is rerun. At its need, or on a runner that reported no cpus, today's rules
// stand.
func TestAFailureThatRanBelowItsNeedIsVoidNeverARed(t *testing.T) {
	tree := strings.Repeat("d", 40)
	unit := strings.Repeat("1", 64)
	need := protocol.Resources{MemoryMegabytes: 9710, Cpus: 16}
	for _, c := range []struct {
		name    string
		ranWith protocol.Resources
		alone   string
		status  string
		infra   string
		reruns  int
	}{
		{"16-cpu unit on a 4-cpu slot: void belowNeed", protocol.Resources{Cpus: 4, MemoryMegabytes: 16384}, "failed", "void", InfraBelowNeed, 0},
		{"at its need: fails alone, the change's red", protocol.Resources{Cpus: 16, MemoryMegabytes: 65536}, "failed", "red", "", 2},
		{"cpus unreported: judged as today", protocol.Resources{}, "passed", "green", "", 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			units := []PlannedUnitWire{{UnitKey: unit, KeyParts: json.RawMessage(`{"package":"p","kind":"test"}`), Decision: "run", Resources: need}}
			source := listedFutures{{Future: tree, Base: baseTree, Change: PlannedChange{Change: "chg_A", Sha: tree, Base: baseTree}, Units: units}}
			stream := finishedStream(unit, "failed")
			stream[0].Cpus, stream[0].MemoryMegabytes = c.ranWith.Cpus, c.ranWith.MemoryMegabytes
			reruns := 0
			queue := &StubQueue{}
			puller := Puller{
				Source: source,
				RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
				Read:   func(string) ([]protocol.Event, error) { return stream, nil },
				Rerun: func(_ json.RawMessage, _ protocol.Resources, sha string) ([]protocol.Event, error) {
					reruns++
					if sha == baseTree {
						return finishedStream("job", "passed"), nil
					}
					return finishedStream("job", c.alone), nil
				},
				NeedNow: func(_ json.RawMessage, listed protocol.Resources) (protocol.Resources, error) { return listed, nil },
				Main:    NoMainRecords{},
				Queue:   queue,
				Loop:    Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: func() time.Time { return time.Date(2026, 10, 10, 2, 40, 0, 0, time.UTC) }},
			}
			if judged, err := puller.PullOnce(); err != nil || judged != 1 {
				t.Fatalf("judged %d %v", judged, err)
			}
			post := queue.Posts[tree][0]
			if post.Decision.Status != c.status || reruns != c.reruns {
				t.Fatalf("run %s with %d reruns, want %s with %d", post.Decision.Status, reruns, c.status, c.reruns)
			}
			var record struct {
				Infra *string `json:"infra"`
			}
			if err := json.Unmarshal(post.Verdicts[0], &record); err != nil {
				t.Fatal(err)
			}
			if got := ""; record.Infra != nil {
				got = *record.Infra
				if got != c.infra {
					t.Fatalf("infra %q, want %q", got, c.infra)
				}
			} else if c.infra != "" {
				t.Fatalf("no infra, want %q", c.infra)
			}
		})
	}
}

// Carried, a warm pass would be left out by Fabric's placer and read warm by the judge, voiding the future on every
// attempt. So the carried list skips it: the unit is placed again, or carried from an older pass that ran cold. An
// error asking warm never carries.
func TestAWarmPassIsNeverCarried(t *testing.T) {
	unit := strings.Repeat("1", 64)
	future := PlannedFuture{Future: strings.Repeat("d", 40), Units: []PlannedUnitWire{{UnitKey: unit, KeyParts: json.RawMessage(`{"kind":"test","tools":{"runner":"8a70"}}`), Decision: "run"}}}
	passedOn := func(machine string) []protocol.Event {
		stream := finishedStream(unit, "passed")
		stream[0].Machine = machine
		return stream
	}
	earlier := map[string][]protocol.Event{"run-2": passedOn("codex"), "run-1": passedOn("Cloud")}
	warm := func(run string, planned PlanUnit, attempt Attempt) (bool, error) {
		if planned.Kind != "test" || planned.Runner != "8a70" {
			t.Fatalf("asked about %+v, want the unit's kind and runner from its key", planned)
		}
		return attempt.Machine == "codex", nil
	}
	if got := carried(future, []string{"run-2"}, earlier, warm); len(got) != 0 {
		t.Fatalf("carried %v from a warm pass", got)
	}
	if got := carried(future, []string{"run-2", "run-1"}, earlier, warm); len(got) != 1 || got[0].Run != "run-1" {
		t.Fatalf("carried %v, want the older cold pass", got)
	}
	failing := func(string, PlanUnit, Attempt) (bool, error) { return false, errors.New("pools.json unreadable") }
	if got := carried(future, []string{"run-1"}, earlier, failing); len(got) != 0 {
		t.Fatalf("carried %v on an error asking warm", got)
	}
	if open := openUnits(future, nil, []string{"run-2"}, earlier, warm); open != 1 {
		t.Fatalf("open %d, want the warm-passed unit still open", open)
	}
}

func TestThePlacersCarriedListAsksTheLoopsWarmRule(t *testing.T) {
	tree, unit := strings.Repeat("d", 40), strings.Repeat("1", 64)
	source := listedFutures{{Future: tree, Base: baseTree, Units: []PlannedUnitWire{{UnitKey: unit, KeyParts: json.RawMessage(`{"kind":"test","tools":{"runner":"8a70"}}`), Decision: "run"}}}}
	puller := Puller{
		Source: source,
		RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
		Read:   func(string) ([]protocol.Event, error) { return finishedStream(unit, "passed"), nil },
		Loop:   Loop{Warm: func(string, PlanUnit, Attempt) (bool, error) { return true, nil }},
	}
	if units, err := puller.Carried(tree, 2); err != nil || len(units) != 0 {
		t.Fatalf("carried %v (%v): a warm pass went on the placer's list", units, err)
	}
	puller.Loop.Warm = nil
	if units, err := puller.Carried(tree, 2); err != nil || len(units) != 1 {
		t.Fatalf("carried %v (%v), want the pass with no warm rule", units, err)
	}
}

// Kirk's budget (Oct 10 03:0xZ): the runner stops a unit at 30 s to ready or 60 s to run and reports the cause as an
// error event, then finished failed. That's Loom's, never the change's: void overBudget, no retry and no alone
// rerun, nothing quarantined. The cause word in another phase isn't the runner's budget, and is judged as today.
func TestAnOverBudgetUnitIsVoidNeverRedFlakeOrGreen(t *testing.T) {
	tree, unit := strings.Repeat("d", 40), strings.Repeat("1", 64)
	overBudget := func(phase, message string) []protocol.Event {
		stream := finishedStream(unit, "failed")
		return append(stream[:len(stream)-1], protocol.Event{Unit: unit, Type: "error", Phase: phase, Message: message}, stream[len(stream)-1])
	}
	for _, c := range []struct {
		name   string
		first  []protocol.Event
		alone  []protocol.Event
		status string
		infra  string
		reruns int
	}{
		{"over its run budget", overBudget(protocol.PhaseRun, "overBudgetRun: 60 s"), finishedStream("job", "passed"), "void", InfraOverBudget, 0},
		{"over its ready budget", overBudget(protocol.PhaseStart, "overBudgetReady: 30 s"), finishedStream("job", "passed"), "void", InfraOverBudget, 0},
		{"the word in another phase", overBudget(protocol.PhaseUpload, "overBudgetRun"), finishedStream("job", "failed"), "red", "", 2},
		{"a plain failure", finishedStream(unit, "failed"), finishedStream("job", "failed"), "red", "", 2},
		{"an alone rerun over budget", finishedStream(unit, "failed"), append(finishedStream("job", "failed")[:3], protocol.Event{Unit: "job", Type: "error", Phase: protocol.PhaseRun, Message: "overBudgetRun: 60 s"}, protocol.Event{Unit: "job", Type: "finished", Status: "failed"}), "void", InfraOverBudget, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			units := []PlannedUnitWire{{UnitKey: unit, KeyParts: json.RawMessage(`{"package":"p","kind":"test"}`), Decision: "run"}}
			source := listedFutures{{Future: tree, Base: baseTree, Change: PlannedChange{Change: "chg_A", Sha: tree, Base: baseTree}, Units: units}}
			reruns := 0
			queue := &StubQueue{}
			puller := Puller{
				Source: source,
				RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
				Read:   func(string) ([]protocol.Event, error) { return c.first, nil },
				Rerun: func(_ json.RawMessage, _ protocol.Resources, sha string) ([]protocol.Event, error) {
					reruns++
					if sha == baseTree {
						return finishedStream("job", "passed"), nil
					}
					return c.alone, nil
				},
				Main:  NoMainRecords{},
				Queue: queue,
				Loop:  Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: func() time.Time { return time.Date(2026, 10, 10, 3, 5, 0, 0, time.UTC) }},
			}
			if judged, err := puller.PullOnce(); err != nil || judged != 1 {
				t.Fatalf("judged %d %v", judged, err)
			}
			post := queue.Posts[tree][0]
			var record struct {
				Infra *string `json:"infra"`
			}
			if err := json.Unmarshal(post.Verdicts[0], &record); err != nil {
				t.Fatal(err)
			}
			infra := ""
			if record.Infra != nil {
				infra = *record.Infra
			}
			if post.Decision.Status != c.status || infra != c.infra || reruns != c.reruns || len(post.Quarantine) != 0 {
				t.Fatalf("run %s infra %q, %d reruns, quarantine %v; want %s %q, %d", post.Decision.Status, infra, reruns, post.Quarantine, c.status, c.infra, c.reruns)
			}
		})
	}
}
