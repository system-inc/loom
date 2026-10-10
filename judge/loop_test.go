package judge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	futureTree = strings.Repeat("f", 40)
	baseTree   = strings.Repeat("b", 40)
)

type stubRuns map[string]Finished

func (runs stubRuns) Finished(run, unitKey string) (Finished, bool, error) {
	finished, found := runs[unitKey]
	return finished, found, nil
}

type stubMain map[string][]TestOutcome

func (records stubMain) Latest(base, unitKey string) ([]TestOutcome, bool, error) {
	tests, found := records[unitKey]
	return tests, found, nil
}

func passed() Finished {
	return Finished{Attempt: Attempt{Status: Passed}, Tests: []TestOutcome{outcome("TestA", "pass")}}
}

func failedWith(test string) Finished {
	return Finished{Attempt: Attempt{Status: Failed, Exit: 1}, Tests: []TestOutcome{outcome(test, "fail")}}
}

func broken(kind string) Finished {
	return Finished{Attempt: Attempt{Status: Broken, Exit: -1}, Infra: kind}
}

type harness struct {
	runs   stubRuns
	fabric *StubFabric
	main   stubMain
	queue  *StubQueue
}

func newHarness() harness {
	return harness{runs: stubRuns{}, fabric: &StubFabric{Script: map[string][]Finished{}}, main: stubMain{}, queue: &StubQueue{}}
}

func (h harness) script(unit, tree string, answers ...Finished) {
	h.fabric.Script[unit+" "+tree] = append(h.fabric.Script[unit+" "+tree], answers...)
}

func (h harness) judge(t *testing.T, plan ...PlanUnit) FuturePost {
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Now: func() time.Time { return time.Date(2026, 10, 9, 23, 45, 0, 0, time.UTC) }}
	post, err := loop.JudgeFuture(Job{Record: ChangeRecord{Change: "chg_A", Sha: futureTree, Base: baseTree, Owner: "system_adamic_library"}, Change: "chg_A", Future: futureTree, Base: baseTree, Run: "run-1", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if posted := h.queue.Posts[futureTree]; len(posted) != 1 {
		t.Fatalf("queue got %d posts for the future, want 1", len(posted))
	}
	return post
}

func recordOf(t *testing.T, post FuturePost, unit string) Verdict {
	for _, raw := range post.Verdicts {
		var record struct {
			UnitKey string  `json:"unitKey"`
			Status  string  `json:"status"`
			Cause   *string `json:"cause"`
			Infra   *string `json:"infra"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		if record.UnitKey == unit {
			verdict := Verdict{UnitKey: record.UnitKey, Status: record.Status}
			if record.Cause != nil {
				verdict.Cause = *record.Cause
			}
			if record.Infra != nil {
				verdict.Infra = *record.Infra
			}
			return verdict
		}
	}
	t.Fatalf("no record for %s", unit)
	return Verdict{}
}

func TestEachPathThroughTheLoop(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(harness)
		plan   PlanUnit
		status string
		cause  string
		infra  string
		asked  int
		run    string
	}{
		{"a pass posts passed, nothing rerun", func(h harness) { h.runs["u"] = passed() }, PlanUnit{UnitKey: "u"}, Passed, "", "", 0, "green"},
		{"a reused unit is passed without reading its run", func(h harness) {}, PlanUnit{UnitKey: "u", Reused: "verdict-7"}, Passed, "", "", 0, "green"},
		{"a kill is placed again and its pass stands", func(h harness) {
			h.runs["u"] = broken(InfraKill)
			h.script("u", futureTree, passed())
		}, PlanUnit{UnitKey: "u"}, Passed, "", "", 1, "green"},
		{"infra past its retries is void", func(h harness) {
			h.runs["u"] = broken(InfraDisk)
			h.script("u", futureTree, broken(InfraDisk), broken(InfraDisk))
		}, PlanUnit{UnitKey: "u"}, Void, CauseInfra, InfraDisk, 2, "void"},
		{"a never-reported unit is placed again", func(h harness) { h.script("u", futureTree, passed()) }, PlanUnit{UnitKey: "u"}, Passed, "", "", 1, "green"},
		{"a failure that fails alone on the candidate only is the change's", func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, failedWith("TestB"))
			h.script("u", baseTree, passed())
		}, PlanUnit{UnitKey: "u"}, Failed, CauseChange, "", 2, "red"},
		{"a failure that passes alone on both is a flake, passed", func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, passed())
			h.script("u", baseTree, passed())
		}, PlanUnit{UnitKey: "u"}, Passed, CauseFlake, "", 2, "green"},
		{"a failure main shares on its record is main's red, excused", func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, failedWith("TestB"))
			h.script("u", baseTree, failedWith("TestB"))
			h.main["u"] = []TestOutcome{outcome("TestB", "fail")}
		}, PlanUnit{UnitKey: "u"}, Failed, CauseMainRed, "", 2, "green"},
		{"a rerun that breaks reruns both again", func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, broken(InfraNeverPlaced), failedWith("TestB"))
			h.script("u", baseTree, passed(), passed())
		}, PlanUnit{UnitKey: "u"}, Failed, CauseChange, "", 4, "red"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			c.setup(h)
			post := h.judge(t, c.plan)
			record := recordOf(t, post, c.plan.UnitKey)
			if record.Status != c.status || record.Cause != c.cause || record.Infra != c.infra {
				t.Fatalf("record %+v, want %s %s %s", record, c.status, c.cause, c.infra)
			}
			if len(h.fabric.Asked) != c.asked {
				t.Fatalf("fabric asked %v, want %d placements", h.fabric.Asked, c.asked)
			}
			if post.Decision.Status != c.run {
				t.Fatalf("run %s, want %s", post.Decision.Status, c.run)
			}
		})
	}
}

func TestAFlakeIsPostedForQuarantine(t *testing.T) {
	h := newHarness()
	h.runs["u"] = failedWith("TestB")
	h.script("u", futureTree, passed())
	h.script("u", baseTree, passed())
	post := h.judge(t, PlanUnit{UnitKey: "u"})
	if len(post.Quarantine) != 1 || post.Quarantine[0].Test != "TestB" {
		t.Fatalf("quarantine %v, want TestB", post.Quarantine)
	}
}

func TestTheReranRedRunsOnTheCandidateAndOnMain(t *testing.T) {
	h := newHarness()
	h.runs["u"] = failedWith("TestB")
	h.script("u", futureTree, failedWith("TestB"))
	h.script("u", baseTree, passed())
	h.judge(t, PlanUnit{UnitKey: "u"})
	if strings.Join(h.fabric.Asked, ",") != "u "+futureTree+",u "+baseTree {
		t.Fatalf("placements %v, want the candidate's tree then main's base", h.fabric.Asked)
	}
}

func TestThePostCarriesThePlanAndARecordPerUnit(t *testing.T) {
	h := newHarness()
	h.runs["u"], h.runs["v"] = passed(), passed()
	post := h.judge(t, PlanUnit{UnitKey: "u"}, PlanUnit{UnitKey: "v"}, PlanUnit{UnitKey: "w", Reused: "verdict-3"})
	if strings.Join(post.Plan, ",") != "u,v,w" || len(post.Verdicts) != 3 || post.Rule != Rule || post.Change != "chg_A" || post.Run != "run-1" {
		t.Fatalf("post %+v", post)
	}
	for _, raw := range post.Verdicts {
		if !strings.Contains(string(raw), `"future":"`+futureTree+`"`) || !strings.Contains(string(raw), `"change":"chg_A"`) {
			t.Fatalf("record %s lacks the future or the change", raw)
		}
	}
}

func TestARedPostCarriesItsKickAndTheDecisionReadsAsQueueParsesIt(t *testing.T) {
	h := newHarness()
	h.runs["u"], h.runs["v"] = failedWith("TestB"), passed()
	h.script("u", futureTree, failedWith("TestB"))
	h.script("u", baseTree, passed())
	post := h.judge(t, PlanUnit{UnitKey: "u"}, PlanUnit{UnitKey: "v"})
	kick, found := post.Decision.Kicks["u"]
	if !found || len(post.Decision.Kicks) != 1 || kick.Repro != "loom repro u" || kick.Owner != "system_adamic_library" || kick.Tests[0].Test != "TestB" {
		t.Fatalf("kicks %+v", post.Decision.Kicks)
	}
	encoded, err := json.Marshal(post)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"decision":{"status":"red"`, `"red":["u"]`, `"excused":[]`, `"problems":[]`, `"kicks":{"u":{`, `"plan":["u","v"]`, `"rule":"judge-v1"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("%s lacks %s", encoded, field)
		}
	}
}

func TestAVoidedRunPostsEveryRunUnitVoidAndRerunsNothing(t *testing.T) {
	h := newHarness()
	// u finished passed and v failed before the stop; w never started; x was reused.
	h.runs["u"], h.runs["v"] = passed(), failedWith("TestB")
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Now: func() time.Time { return time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC) }}
	post, err := loop.VoidFuture(Job{Change: "chg_A", Future: futureTree, Base: baseTree, Run: "run-1",
		Plan: []PlanUnit{{UnitKey: "u"}, {UnitKey: "v"}, {UnitKey: "w"}, {UnitKey: "x", Reused: "verdict-3"}}}, "an operator stopped the run")
	if err != nil {
		t.Fatal(err)
	}
	if post.Decision.Status != Void || len(post.Decision.Red) != 0 || len(post.Decision.Kicks) != 0 {
		t.Fatalf("decision %+v, want void with no red and no kick", post.Decision)
	}
	if len(post.Decision.Problems) == 0 || post.Decision.Problems[0] != "run run-1 void: an operator stopped the run" {
		t.Fatalf("problems %v, want the cause first", post.Decision.Problems)
	}
	for _, unit := range []string{"u", "v", "w"} {
		if record := recordOf(t, post, unit); record.Status != Void || record.Cause != CauseInfra || record.Infra != InfraKill {
			t.Fatalf("unit %s: %+v, want void infra kill", unit, record)
		}
	}
	if record := recordOf(t, post, "x"); record.Status != Passed {
		t.Fatalf("reused unit %+v, want passed", record)
	}
	if len(h.fabric.Asked) != 0 || len(h.queue.Posts[futureTree]) != 1 {
		t.Fatalf("placements %v and %d posts, want none and one", h.fabric.Asked, len(h.queue.Posts[futureTree]))
	}
	if !strings.Contains(string(post.Verdicts[1]), `"tests":[{"outcome":"fail"`) {
		t.Fatalf("v's record %s lacks its attempt's tests as evidence", post.Verdicts[1])
	}
}

func TestAVoidNamesItsCause(t *testing.T) {
	h := newHarness()
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Now: time.Now}
	if _, err := loop.VoidFuture(Job{Run: "run-1"}, " "); err == nil {
		t.Fatal("a void with no cause was posted")
	}
}

// skippedUnit is a unit whose tests passed with one skip of test in internal/native, saying why.
func skippedUnit(test, why string) Finished {
	return Finished{Attempt: Attempt{Status: Passed}, Tests: []TestOutcome{outcome("TestA", "pass"), {Package: nativePackage, Test: test, Outcome: "skip"}},
		Events: append([]TestEvent{{Action: "pass", Package: nativePackage, Test: "TestA"}}, skipOf(test, why)...)}
}

func censusLoop(h harness) Loop {
	rows := []CensusRow{{File: "internal/native/a_test.go", ID: "m", Callers: []string{"TestMeasured"}, Message: `"a measurement"`, Class: "measurement", Provides: "timing"}}
	return Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Now: time.Now, Census: &CensusConfig{Rows: rows, Platform: "linux"}}
}

func censusJob(plan ...PlanUnit) Job {
	return Job{Record: ChangeRecord{Change: "chg_A", Sha: futureTree, Base: baseTree, Owner: "system_adamic_library"}, Change: "chg_A", Future: futureTree, Base: baseTree, Run: "run-1", Plan: plan}
}

func TestAnUnclassifiedSkipRedsItsUnitAtTheCensusWithoutReruns(t *testing.T) {
	h := newHarness()
	h.runs["u"], h.runs["v"] = skippedUnit("TestWASIUnit07", "no sysroot"), skippedUnit("TestMeasured", "a measurement")
	post, err := censusLoop(h).JudgeFuture(censusJob(PlanUnit{UnitKey: "u"}, PlanUnit{UnitKey: "v"}))
	if err != nil {
		t.Fatal(err)
	}
	if post.Decision.Status != "red" || strings.Join(post.Decision.Red, ",") != "u" || len(h.fabric.Asked) != 0 {
		t.Fatalf("decision %+v, placements %v: want u red at the census and nothing rerun", post.Decision.RunVerdict, h.fabric.Asked)
	}
	if record := recordOf(t, post, "u"); record.Status != Failed || record.Cause != CauseChange {
		t.Fatalf("u %+v", record)
	}
	if !strings.Contains(string(post.Verdicts[0]), `"rule":"judge-v1 census"`) || !strings.Contains(string(post.Verdicts[1]), `"rule":"judge-v1"`) {
		t.Fatalf("records %s %s: want u's rule census and v's plain", post.Verdicts[0], post.Verdicts[1])
	}
	kick := post.Decision.Kicks["u"]
	if len(kick.Tests) != 1 || kick.Tests[0].Test != "TestWASIUnit07" || kick.Tests[0].Outcome != "skip" || !strings.Contains(kick.Why, "census: unknown "+nativePackage+" TestWASIUnit07") {
		t.Fatalf("kick %+v", kick)
	}
}

func TestATestRedStaysATestRedAndAVoidIsNeverCensused(t *testing.T) {
	h := newHarness()
	failing := failedWith("TestB")
	failing.Events = skipOf("TestWASIUnit07", "no sysroot")
	h.runs["u"], h.runs["v"] = failing, broken(InfraDisk)
	h.script("u", futureTree, failedWith("TestB"))
	h.script("u", baseTree, passed())
	h.script("v", futureTree, broken(InfraDisk), broken(InfraDisk))
	post, err := censusLoop(h).JudgeFuture(censusJob(PlanUnit{UnitKey: "u"}, PlanUnit{UnitKey: "v"}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(post.Verdicts[0]), "census") || recordOf(t, post, "v").Status != Void {
		t.Fatalf("records %s %s", post.Verdicts[0], post.Verdicts[1])
	}
	reading, err := ReadingOf(futureTree, post)
	if err != nil || reading.FirstStep != "tests" {
		t.Fatalf("reading %+v (%v), want a red at tests", reading, err)
	}
}

// 0096078f, deferred-red-reaches-census, through the new path: its TestWASIUnit shards skip at an undeclared site, the
// unit's tests pass, and the batch must read red at the census, which is what the suite declares.
func TestTheCensusMutantReadsAsDeclared(t *testing.T) {
	h := newHarness()
	h.runs["native"] = skippedUnit("TestWASIUnit00", "gate mutant 3: a deferred red that must reach the census")
	post, err := censusLoop(h).JudgeFuture(censusJob(PlanUnit{UnitKey: "native"}))
	if err != nil {
		t.Fatal(err)
	}
	reading, err := ReadingOf(futureTree, post)
	if err != nil {
		t.Fatal(err)
	}
	mutant := Mutant{Name: "deferred-red-reaches-census", Sha: futureTree, Step: "census"}
	if said := JudgeMutant(mutant, reading); said != "ok" {
		t.Fatalf("%s: reading %+v", said, reading)
	}
	// Without the census step the same batch reads green, which the suite must call wrong.
	h2 := newHarness()
	h2.runs["native"] = h.runs["native"]
	bare := censusLoop(h2)
	bare.Census = nil
	post, err = bare.JudgeFuture(censusJob(PlanUnit{UnitKey: "native"}))
	if err != nil {
		t.Fatal(err)
	}
	reading, _ = ReadingOf(futureTree, post)
	if said := JudgeMutant(mutant, reading); !strings.HasPrefix(said, "wrong green") {
		t.Fatalf("a census-less judge read the census mutant %q", said)
	}
}

// The box runs its census after its tests, so a batch with a test red and a census red reads red at tests.
func TestATestRedComesBeforeACensusRed(t *testing.T) {
	h := newHarness()
	h.runs["tests"], h.runs["census"] = failedWith("TestB"), skippedUnit("TestWASIUnit07", "no sysroot")
	h.script("tests", futureTree, failedWith("TestB"))
	h.script("tests", baseTree, passed())
	post, err := censusLoop(h).JudgeFuture(censusJob(PlanUnit{UnitKey: "tests"}, PlanUnit{UnitKey: "census"}))
	if err != nil {
		t.Fatal(err)
	}
	reading, err := ReadingOf(futureTree, post)
	if err != nil || len(post.Decision.Red) != 2 || reading.FirstStep != "tests" || reading.FirstFailure != testPackage+" TestB" {
		t.Fatalf("red %v, reading %+v (%v): want both red and the step tests", post.Decision.Red, reading, err)
	}
}
