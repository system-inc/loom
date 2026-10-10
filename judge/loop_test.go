package judge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
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

// stubReused answers every reused unit with one tests object, naming the run it reuses.
type stubReused struct{}

func (stubReused) Tests(unitKey, reused string) (json.RawMessage, error) {
	return json.RawMessage(`{"failed":0,"inline":[],"passed":7,"sha256":"` + strings.Repeat("7", 64) + `","skipped":0}`), nil
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
	blobs  *StubBlobs
}

func newHarness() harness {
	return harness{runs: stubRuns{}, fabric: &StubFabric{Script: map[string][]Finished{}}, main: stubMain{}, queue: &StubQueue{}, blobs: &StubBlobs{}}
}

func (h harness) script(unit, tree string, answers ...Finished) {
	h.fabric.Script[unit+" "+tree] = append(h.fabric.Script[unit+" "+tree], answers...)
}

func (h harness) judge(t *testing.T, plan ...PlanUnit) FuturePost {
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: func() time.Time { return time.Date(2026, 10, 9, 23, 45, 0, 0, time.UTC) }}
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

// A witness of main (base = sha, chg_2bfxkyyp's shape) reruns its one tree twice: only failed alone both times is red,
// which Queue records as main.red; a failure that passes alone either time is a flake; Loom's breaks are void (#r0xntgv).
func TestAWitnessIsRedOnlyWhenItsOneTreeFailsAloneTwice(t *testing.T) {
	cases := []struct {
		name   string
		first  Finished
		alone  []Finished // the candidate's rerun, then main's: both on the witness's one tree
		status string
		cause  string
		infra  string
		run    string
	}{
		{"failed alone both times: red, the witness's own", failedWith("TestB"), []Finished{failedWith("TestB"), failedWith("TestB")}, Failed, CauseChange, "", "red"},
		{"failed, then failed and passed alone: a flake", failedWith("TestB"), []Finished{failedWith("TestB"), passed()}, Passed, CauseFlake, "", "green"},
		{"failed, then passed and failed alone: a flake", failedWith("TestB"), []Finished{passed(), failedWith("TestB")}, Passed, CauseFlake, "", "green"},
		{"failed, then passed alone both times: a flake", failedWith("TestB"), []Finished{passed(), passed()}, Passed, CauseFlake, "", "green"},
		{"broken by Loom every time: void, never red", broken(InfraRefused), []Finished{broken(InfraRefused), broken(InfraRefused)}, Void, CauseInfra, InfraRefused, "void"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.runs["u"] = c.first
			h.script("u", futureTree, c.alone...)
			loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Now: func() time.Time { return time.Date(2026, 10, 10, 20, 30, 0, 0, time.UTC) }}
			post, err := loop.JudgeFuture(Job{Record: ChangeRecord{Change: "chg_W", Sha: futureTree, Base: futureTree, Owner: "system_adamic_loom"}, Change: "chg_W",
				Future: futureTree, Base: futureTree, Run: "run-1", Plan: []PlanUnit{{UnitKey: "u"}}})
			if err != nil {
				t.Fatal(err)
			}
			if record := recordOf(t, post, "u"); record.Status != c.status || record.Cause != c.cause || record.Infra != c.infra {
				t.Fatalf("record %+v, want %s %s %s", record, c.status, c.cause, c.infra)
			}
			if post.Decision.Status != c.run {
				t.Fatalf("run %s, want %s", post.Decision.Status, c.run)
			}
			if c.cause == CauseFlake && (len(post.Quarantine) != 1 || post.Quarantine[0].Test != "TestB") {
				t.Fatalf("quarantine %v, want TestB", post.Quarantine)
			}
			if _, kicked := post.Decision.Kicks["u"]; kicked != (c.run == "red") {
				t.Fatalf("kicks %v for a %s run", post.Decision.Kicks, c.run)
			}
		})
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
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: func() time.Time { return time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC) }}
	post, err := loop.VoidFuture(Job{Change: "chg_A", Future: futureTree, Base: baseTree, Run: "run-1",
		Plan: []PlanUnit{{UnitKey: "u"}, {UnitKey: "v"}, {UnitKey: "w"}, {UnitKey: "x", Reused: "verdict-3"}}}, InfraKill, "an operator stopped the run")
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
	if !strings.Contains(string(post.Verdicts[1]), `"inline":[{"outcome":"fail","package":"`+nativePackage+`","test":"TestB"}]`) {
		t.Fatalf("v's record %s lacks its attempt's tests as evidence", post.Verdicts[1])
	}
}

func TestAVoidNamesItsCause(t *testing.T) {
	h := newHarness()
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: time.Now}
	if _, err := loop.VoidFuture(Job{Run: "run-1"}, InfraKill, " "); err == nil {
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
	return Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: time.Now, Census: &CensusConfig{Rows: rows, Platform: "linux"}}
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

func TestTheTestsListIsCanonicalAndCounted(t *testing.T) {
	content, ref := TestsList([]TestOutcome{outcome("TestB", "pass"), outcome("TestA/sub", "skip"), outcome("TestA", "fail"), outcome("TestC", "run")})
	want := `[{"outcome":"fail","package":"` + testPackage + `","test":"TestA"},{"outcome":"skip","package":"` + testPackage + `","test":"TestA/sub"},` +
		`{"outcome":"pass","package":"` + testPackage + `","test":"TestB"},{"outcome":"run","package":"` + testPackage + `","test":"TestC"}]`
	if string(content) != want {
		t.Fatalf("list\n%s\nwant\n%s", content, want)
	}
	sum := sha256.Sum256(content)
	if ref.Sha256 != hex.EncodeToString(sum[:]) || ref.Passed != 1 || ref.Failed != 1 || ref.Skipped != 1 || len(ref.Inline) != 2 || ref.Inline[1].Outcome != "run" {
		t.Fatalf("ref %+v", ref)
	}
}

func TestEveryListIsInTheStoreBeforeThePostNamesIt(t *testing.T) {
	h := newHarness()
	h.runs["u"] = passed()
	post := h.judge(t, PlanUnit{UnitKey: "u"})
	var record struct {
		Tests TestsRef `json:"tests"`
	}
	if err := json.Unmarshal(post.Verdicts[0], &record); err != nil {
		t.Fatal(err)
	}
	held, found := h.blobs.Held[record.Tests.Sha256]
	if !found || !strings.Contains(string(held), `"test":"TestA"`) || record.Tests.Passed != 1 {
		t.Fatalf("record names %s, store holds %v", record.Tests.Sha256, h.blobs.Held)
	}
	// A list the store refuses is never named by a posted record.
	refused := newHarness()
	refused.runs["u"] = passed()
	refused.blobs.Fail = errors.New("store down")
	loop := Loop{Runs: refused.runs, Fabric: refused.fabric, Main: refused.main, Queue: refused.queue, Blobs: refused.blobs, Reused: stubReused{}, Now: time.Now}
	if _, err := loop.JudgeFuture(censusJob(PlanUnit{UnitKey: "u"})); err == nil || len(refused.queue.Posts) != 0 {
		t.Fatalf("posted %v (%v) past a refused list", refused.queue.Posts, err)
	}
	loop.Blobs = nil
	if _, err := loop.JudgeFuture(censusJob(PlanUnit{UnitKey: "u"})); err == nil || len(refused.queue.Posts) != 0 {
		t.Fatal("posted with no store")
	}
}

func TestAReusedRecordCarriesTheTestsOfTheVerdictItReuses(t *testing.T) {
	h := newHarness()
	post := h.judge(t, PlanUnit{UnitKey: "w", Reused: "run-0"})
	want := `"tests":{"failed":0,"inline":[],"passed":7,"sha256":"` + strings.Repeat("7", 64) + `","skipped":0}`
	if !strings.Contains(string(post.Verdicts[0]), want) || !strings.Contains(string(post.Verdicts[0]), `"rule":"judge-v1 reused run-0"`) || len(h.blobs.Held) != 0 {
		t.Fatalf("record %s, store %v: want the reused tests object, its run named, nothing put", post.Verdicts[0], h.blobs.Held)
	}
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Now: time.Now}
	if _, err := loop.JudgeFuture(censusJob(PlanUnit{UnitKey: "w", Reused: "run-0"})); err == nil {
		t.Fatal("a reused unit posted with nothing to read the verdict it reuses")
	}
}

// runsByRun answers each (run, unit) on its own, so a job's earlier attempts can differ from its run.
type runsByRun map[string]Finished

func (runs runsByRun) Finished(run, unitKey string) (Finished, bool, error) {
	finished, found := runs[run+" "+unitKey]
	return finished, found, nil
}

func TestARedEarlierAttemptIsNeverCarried(t *testing.T) {
	h := newHarness()
	h.script("u", futureTree, passed())
	runs := runsByRun{"run-1 u": failedWith("TestB")}
	loop := Loop{Runs: runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: time.Now}
	job := censusJob(PlanUnit{UnitKey: "u"})
	job.Run, job.Earlier = "run-2", []string{"run-1"}
	post, err := loop.JudgeFuture(job)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(post.Verdicts[0]), "carried") || len(h.fabric.Asked) != 1 {
		t.Fatalf("record %s, placements %v: a red earlier attempt must run again, not carry", post.Verdicts[0], h.fabric.Asked)
	}
}

func TestAPhaseIsDecidedByItsExitLikeTheBoxsStage(t *testing.T) {
	exited := func(status string, exit int) Finished {
		return Finished{Attempt: Attempt{Status: status, Exit: exit}}
	}
	cases := []struct {
		name   string
		first  Finished
		script []Finished
		status string
		cause  string
		asked  int
	}{
		{"exit 0 passes with no test log", exited(Passed, 0), nil, Passed, "", 0},
		{"a nonzero exit is the change's red, never rerun alone", exited(Failed, 1), nil, Failed, CauseChange, 0},
		{"exit 2 is void, placed again, and its pass stands", exited(Failed, PhaseCantJudge), []Finished{exited(Passed, 0)}, Passed, "", 1},
		{"exit 2 past its retries stays void", exited(Failed, PhaseCantJudge), []Finished{exited(Failed, 2), exited(Failed, 2)}, Void, CauseInfra, 2},
		{"a runner kill is infra like any unit's", Finished{Attempt: Attempt{Status: Broken}, Infra: InfraKill}, []Finished{exited(Passed, 0)}, Passed, "", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.runs["vet"] = c.first
			h.script("vet", futureTree, c.script...)
			loop := censusLoop(h)
			loop.RequireTestLog = true
			post, err := loop.JudgeFuture(censusJob(PlanUnit{UnitKey: "vet", Kind: KindPhase}))
			if err != nil {
				t.Fatal(err)
			}
			record := recordOf(t, post, "vet")
			if record.Status != c.status || record.Cause != c.cause || len(h.fabric.Asked) != c.asked {
				t.Fatalf("%+v, placements %v; want %s %s with %d placements", record, h.fabric.Asked, c.status, c.cause, c.asked)
			}
			if c.status != Void && !strings.Contains(string(post.Verdicts[0]), `"rule":"judge-v1 phase"`) {
				t.Fatalf("record %s lacks the phase rule", post.Verdicts[0])
			}
		})
	}
}

func TestAnAttemptOnAnotherRunnerThanItsKeyIsVoid(t *testing.T) {
	key, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	on := func(runner string, status string) Finished {
		finished := passed()
		finished.Attempt.Status, finished.Attempt.RunnerSha256 = status, runner
		return finished
	}
	cases := []struct {
		name    string
		first   Finished
		script  []Finished
		require bool
		status  string
		asked   int
	}{
		{"its key's runner passes", on(key, Passed), nil, false, Passed, 0},
		{"another runner is void and placed again, and the right runner's pass stands", on(other, Passed), []Finished{on(key, Passed)}, false, Passed, 1},
		{"another runner past the retries stays void", on(other, Passed), []Finished{on(other, Passed), on(other, Passed)}, false, Void, 2},
		{"an unreported runner is accepted before the switch", on(RunnerUnreported, Passed), nil, false, Passed, 0},
		{"an unreported runner is void after the switch", on(RunnerUnreported, Passed), []Finished{on(RunnerUnreported, Passed), on(RunnerUnreported, Passed)}, true, Void, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.runs["u"] = c.first
			h.script("u", futureTree, c.script...)
			loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: time.Now, RequireRunner: c.require}
			post, err := loop.JudgeFuture(censusJob(PlanUnit{UnitKey: "u", Runner: key}))
			if err != nil {
				t.Fatal(err)
			}
			if record := recordOf(t, post, "u"); record.Status != c.status || len(h.fabric.Asked) != c.asked {
				t.Fatalf("%+v with placements %v, want %s with %d", record, h.fabric.Asked, c.status, c.asked)
			}
		})
	}
	// An alone rerun on another runner can't decide a cause either.
	decision, err := Decide(Evidence{First: Attempt{Status: Failed, RunnerSha256: key}, KeyRunner: key,
		Candidate: &Rerun{Status: Passed, RunnerSha256: other}, Main: &Rerun{Status: Passed, RunnerSha256: key}})
	if err != nil || decision.Decided || decision.Status != Void {
		t.Fatalf("decision %+v (%v), want void from the candidate rerun on another runner", decision, err)
	}
}

func TestTheRecordNamesAnUnreportedRunner(t *testing.T) {
	finished, _ := FinishedFromEvents([]protocol.Event{started(), {Type: "finished", Status: "passed"}})
	reported, _ := FinishedFromEvents([]protocol.Event{{Type: "started", RunnerSha256: strings.Repeat("a", 64)}, {Type: "finished", Status: "passed"}})
	if finished.Attempt.RunnerSha256 != RunnerUnreported || reported.Attempt.RunnerSha256 != strings.Repeat("a", 64) {
		t.Fatalf("runners %q and %q", finished.Attempt.RunnerSha256, reported.Attempt.RunnerSha256)
	}
	encoded, _ := Verdict{Attempts: []Attempt{finished.Attempt}}.Canonical()
	if !strings.Contains(string(encoded), `"runnerSha256":"unreported"`) {
		t.Fatalf("record %s doesn't name the gap", encoded)
	}
}

// Release's ruling (Oct 10 02:43Z): an attempt that ran on a warm shared cache is evidence, never the verdict. It's
// placed again before the run decides, and the cold attempt decides by the same rules: a cold pass passes, and a
// cold red goes to the alone reruns. An attempt not on the warm list is judged as today.
func TestAWarmAttemptIsPlacedAgainColdBeforeItDecides(t *testing.T) {
	cases := []struct {
		name   string
		warm   bool
		setup  func(harness)
		status string
		cause  string
		asked  int
		run    string
	}{
		{"a warm pass is placed again and its cold pass decides", true, func(h harness) {
			h.runs["u"] = passed()
			h.script("u", futureTree, passed())
		}, Passed, "", 1, "green"},
		{"a warm pass whose cold run fails goes to the alone reruns", true, func(h harness) {
			h.runs["u"] = passed()
			h.script("u", futureTree, failedWith("TestB"), failedWith("TestB"))
			h.script("u", baseTree, passed())
		}, Failed, CauseChange, 3, "red"},
		{"a warm red is placed again, and a cold pass is a pass, not a flake", true, func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, passed())
		}, Passed, "", 1, "green"},
		{"an attempt off the list is judged as today", false, func(h harness) { h.runs["u"] = passed() }, Passed, "", 0, "green"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			c.setup(h)
			asked := []string{}
			loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: time.Now,
				Warm: func(run string, unit PlanUnit, _ Attempt) (bool, error) {
					asked = append(asked, run+" "+unit.UnitKey)
					return c.warm, nil
				}}
			post, err := loop.JudgeFuture(Job{Record: ChangeRecord{Change: "chg_A", Sha: futureTree, Base: baseTree}, Change: "chg_A", Future: futureTree, Base: baseTree, Run: "run-1", Plan: []PlanUnit{{UnitKey: "u"}}})
			if err != nil {
				t.Fatal(err)
			}
			record := recordOf(t, post, "u")
			if record.Status != c.status || record.Cause != c.cause || len(h.fabric.Asked) != c.asked || post.Decision.Status != c.run {
				t.Fatalf("record %+v, %d placements, run %s; want %s %s, %d, %s", record, len(h.fabric.Asked), post.Decision.Status, c.status, c.cause, c.asked, c.run)
			}
			if len(asked) != 1 || asked[0] != "run-1 u" {
				t.Fatalf("warm asked %v, want once for run-1's attempt of u", asked)
			}
			if c.warm && len(post.Quarantine) != 0 {
				t.Fatalf("quarantined %v after a warm attempt", post.Quarantine)
			}
		})
	}
}

// Release's ruling (Oct 10 02:49Z, settled 02:54Z): on a runner whose workers keep a shared Go cache, a test attempt
// counts only from a machine a cold pool names, at or after the latest coldSince among the pools naming it. Two pools
// report Cloud, so a Cloud attempt between their two coldSince times is warm.
var warmRunnerPools = []PoolEntry{
	{Name: "codex-strict", Runner: "8a70", MemoryMegabytes: 16384, Cpus: 4},
	{Name: "box-strict-8a70-cold", Runner: "8a70", MemoryMegabytes: 65536, Cpus: 16, Cold: true, Machines: []string{"Cloud"}, ColdSince: "2026-10-10T02:33:00Z"},
	{Name: "box-strict-8a70", Runner: "8a70", MemoryMegabytes: 16384, Cpus: 4, Cold: true, Machines: []string{"Cloud"}, ColdSince: "2026-10-10T02:44:10Z"},
}

func TestAWarmAttemptIsReadFromThePoolTable(t *testing.T) {
	at := func(machine, started string) Attempt { return Attempt{Machine: machine, StartedAt: started} }
	for _, c := range []struct {
		name    string
		pools   []PoolEntry
		attempt Attempt
		warm    bool
	}{
		{"Cloud after both pools ran cold", warmRunnerPools, at("Cloud", "2026-10-10T02:50:00Z"), false},
		{"Cloud between the two coldSince times", warmRunnerPools, at("Cloud", "2026-10-10T02:40:00Z"), true},
		{"a Codex instance no cold pool names", warmRunnerPools, at("cb2a541fac2d", "2026-10-10T02:50:00Z"), true},
		{"a machine a pool not marked cold names", append([]PoolEntry{{Name: "w", Machines: []string{"Cloud"}, ColdSince: "2026-10-10T02:00:00Z"}}, warmRunnerPools...), at("Cloud", "2026-10-10T02:50:00Z"), true},
		{"a cold pool with no coldSince", []PoolEntry{{Name: "c", Cold: true, Machines: []string{"Cloud"}}}, at("Cloud", "2026-10-10T02:50:00Z"), true},
		{"an unreadable started time", warmRunnerPools, at("Cloud", ""), true},
	} {
		if why := WarmAttempt(c.pools, c.attempt); (why != "") != c.warm {
			t.Errorf("%s: warm %q, want warm %v", c.name, why, c.warm)
		}
	}
	unit := PlanUnit{UnitKey: "u", Kind: "test", Runner: "8a70"}
	if WarmUnit(warmRunnerPools, map[string]bool{"8a70": true}, PlanUnit{UnitKey: "p", Kind: "phase", Runner: "8a70"}, at("x", "")) != "" {
		t.Error("a phase unit was judged by the test units' cache rule")
	}
	if WarmUnit(warmRunnerPools, map[string]bool{"ed74": true}, unit, at("x", "")) != "" {
		t.Error("a unit off the warm runners was read warm")
	}
	if names := ColdPools(warmRunnerPools); len(names) != 2 || names[0].Name != "box-strict-8a70-cold" || len(warmRunnerPools) != 3 {
		t.Errorf("cold pools %v, want the two marked cold, the table untouched", names)
	}
}

// A warm pass never decides, and neither does one carried from an earlier attempt: each is placed again, and the
// cold attempt decides.
func TestAWarmPassNeverDecidesEvenCarried(t *testing.T) {
	codex := Finished{Attempt: Attempt{Status: Passed, Machine: "cb2a541fac2d", StartedAt: "2026-10-10T02:20:00Z"}, Tests: []TestOutcome{outcome("TestA", "pass")}}
	cloud := codex
	cloud.Attempt.Machine, cloud.Attempt.StartedAt = "Cloud", "2026-10-10T02:50:00Z"
	for _, c := range []struct {
		name    string
		runs    runsByRun
		earlier []string
		placed  int
	}{
		{"a warm Codex pass is placed again", runsByRun{"run-2 u": codex}, nil, 1},
		{"a warm pass carried from an earlier attempt is placed again", runsByRun{"run-1 u": codex}, []string{"run-1"}, 1},
		{"a cold Cloud pass decides", runsByRun{"run-2 u": cloud}, nil, 0},
		{"a newer warm pass is skipped for an older cold one", runsByRun{"run-1 u": codex, "run-0 u": cloud}, []string{"run-1", "run-0"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.script("u", futureTree, failedWith("TestB"), failedWith("TestB"))
			h.script("u", baseTree, passed())
			loop := Loop{Runs: c.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Reused: stubReused{}, Now: time.Now,
				Warm: func(_ string, unit PlanUnit, attempt Attempt) (bool, error) {
					return WarmUnit(warmRunnerPools, map[string]bool{"8a70": true}, unit, attempt) != "", nil
				}}
			job := censusJob(PlanUnit{UnitKey: "u", Kind: "test", Runner: "8a70"})
			job.Run, job.Earlier = "run-2", c.earlier
			post, err := loop.JudgeFuture(job)
			if err != nil {
				t.Fatal(err)
			}
			if c.placed == 0 {
				if len(h.fabric.Asked) != 0 || post.Decision.Status != "green" {
					t.Fatalf("placements %v, run %s: a cold pass decides", h.fabric.Asked, post.Decision.Status)
				}
				return
			}
			// Its cold attempt fails, so the warm pass didn't decide it: the alone reruns make it the change's red.
			if len(h.fabric.Asked) < c.placed || post.Decision.Status != "red" {
				t.Fatalf("placements %v, run %s: the warm pass decided", h.fabric.Asked, post.Decision.Status)
			}
		})
	}
}

func slowPassed(wall float64) Finished {
	finished := passed()
	finished.Attempt.WallSeconds = wall
	return finished
}

// Through the loop (#ccewvra): a slow pass's record carries its warning, a branch that made it slow is red with both
// walls in its kick, a verify's slow pass reruns nothing, and a fast pass's record has no warnings field at all.
func TestASlowPassThroughTheLoop(t *testing.T) {
	cases := []struct {
		name   string
		base   string
		alone  map[string]Finished // by tree
		run    string
		asked  int
		record string // must be in the unit's record
	}{
		{"a branch that made it slow is red", baseTree, map[string]Finished{futureTree: slowPassed(80), baseTree: slowPassed(30)}, "red", 2,
			`"warnings":[{"baseWallSeconds":30,"budgetSeconds":60,"kind":"overBudget","wallSeconds":74}]`},
		{"a branch that didn't is green with a warning", baseTree, map[string]Finished{futureTree: slowPassed(80), baseTree: slowPassed(75)}, "green", 2,
			`"warnings":[{"baseWallSeconds":75,"budgetSeconds":60,"kind":"overBudget","wallSeconds":74}]`},
		{"a verify's slow pass is green with a warning, nothing rerun", futureTree, nil, "green", 0,
			`"warnings":[{"budgetSeconds":60,"kind":"overBudget","wallSeconds":74}]`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			h.runs["u"] = slowPassed(74)
			for tree, finished := range c.alone {
				h.script("u", tree, finished)
			}
			loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Blobs: h.blobs, Now: func() time.Time { return time.Date(2026, 10, 10, 22, 0, 0, 0, time.UTC) }}
			post, err := loop.JudgeFuture(Job{Record: ChangeRecord{Change: "chg_A", Sha: futureTree, Base: c.base, Owner: "system_adamic_library"}, Change: "chg_A",
				Future: futureTree, Base: c.base, Run: "run-1", Plan: []PlanUnit{{UnitKey: "u", Kind: KindTest}}})
			if err != nil {
				t.Fatal(err)
			}
			if post.Decision.Status != c.run || len(h.fabric.Asked) != c.asked {
				t.Fatalf("run %s with placements %v, want %s and %d", post.Decision.Status, h.fabric.Asked, c.run, c.asked)
			}
			if !strings.Contains(string(post.Verdicts[0]), c.record) {
				t.Fatalf("record %s lacks %s", post.Verdicts[0], c.record)
			}
			kick, kicked := post.Decision.Kicks["u"]
			if kicked != (c.run == "red") || (kicked && !strings.Contains(kick.Why, "80.0 s on the candidate and 30.0 s on main's base")) {
				t.Fatalf("kicks %+v", post.Decision.Kicks)
			}
		})
	}
	// Only a test unit is held to the run budget: a product that builds for minutes is no warning and reruns nothing.
	h := newHarness()
	h.runs["u"] = slowPassed(300)
	if post := h.judge(t, PlanUnit{UnitKey: "u", Kind: "product"}); strings.Contains(string(post.Verdicts[0]), "warnings") || len(h.fabric.Asked) != 0 {
		t.Fatalf("a slow product's record %s, placements %v: want no warning and nothing rerun", post.Verdicts[0], h.fabric.Asked)
	}
	h = newHarness()
	h.runs["u"] = slowPassed(12)
	post := h.judge(t, PlanUnit{UnitKey: "u", Kind: KindTest})
	if strings.Contains(string(post.Verdicts[0]), "warnings") || len(h.fabric.Asked) != 0 {
		t.Fatalf("a fast pass's record %s, placements %v: want no warnings field and nothing rerun", post.Verdicts[0], h.fabric.Asked)
	}
}
