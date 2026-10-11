package judge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// stubLog serves events after a seq, and counts what it served, to prove each read takes only the new ones.
type stubLog struct {
	events []LogEvent
	served int
}

func (log *stubLog) EventsAfter(after int64) ([]LogEvent, error) {
	found := []LogEvent{}
	for _, event := range log.events {
		if event.Seq > after {
			found = append(found, event)
		}
	}
	log.served += len(found)
	return found, nil
}

const flowPackage = "github.com/system-inc/adamic/internal/flow"

func flowFailing(subtests ...string) []TestOutcome {
	tests := []TestOutcome{{Package: flowPackage, Test: "TestFlowCorpusRemainder", Outcome: "fail"}}
	for _, subtest := range subtests {
		tests = append(tests, TestOutcome{Package: flowPackage, Test: "TestFlowCorpusRemainder/" + subtest, Outcome: "fail"})
	}
	return tests
}

func planned(seq int64, future, unitKey, name string) LogEvent {
	data, _ := json.Marshal(map[string]any{"name": name, "decision": "run"})
	return LogEvent{Seq: seq, Type: "unit.planned", Subject: LogSubject{Future: future, UnitKey: unitKey}, Data: data}
}

func decided(seq int64, future, unitKey, status string, failing []TestOutcome) LogEvent {
	data, _ := json.Marshal(map[string]any{"verdict": map[string]any{"future": future, "unitKey": unitKey, "status": status,
		"tests": map[string]any{"sha256": strings.Repeat("e", 64), "inline": failing}}})
	return LogEvent{Seq: seq, Type: "verdict.decided", Subject: LogSubject{Future: future, UnitKey: unitKey, Run: "future-" + future + "-4"}, Data: data}
}

// Main's records are a verify's unit records at main's commit, matched to a branch's unit by name, never key: the
// newest one that isn't void, read incrementally. Mutants: matched by key (the branch's key differs); a void record
// taken over the failed one before it; a branch candidate's record at another tree counted as main's; the log read whole
// every time.
func TestMainsRecordsAreAVerifysUnitRecordsByName(t *testing.T) {
	main := strings.Repeat("e", 40) // ebdb6c53's shape: a verify's future is the commit it verifies
	branch := strings.Repeat("6", 40)
	log := &stubLog{events: []LogEvent{
		planned(1, main, "mainflow", flowPackage),
		planned(2, main, "mainload", "github.com/system-inc/adamic/internal/load"),
		decided(3, main, "mainflow", Failed, flowFailing("a.a", "b.a")),
		decided(4, main, "mainload", Passed, nil),
		planned(5, branch, "branchflow", flowPackage),
		decided(6, branch, "branchflow", Passed, nil),
	}}
	records := NewLogMainRecords(log)
	tests, found, err := records.Latest(main, PlanUnit{UnitKey: "branchflow", Name: flowPackage})
	if err != nil || !found || len(tests) != 3 || tests[0].Test != "TestFlowCorpusRemainder" {
		t.Fatalf("flow at main: %v %v %v, want its three failures", tests, found, err)
	}
	if tests, found, _ := records.Latest(main, PlanUnit{UnitKey: "branchload", Name: "github.com/system-inc/adamic/internal/load"}); !found || len(tests) != 0 {
		t.Fatalf("load at main: %v %v, want found passed, no failures", tests, found)
	}
	if _, found, _ := records.Latest(branch, PlanUnit{UnitKey: "x", Name: "github.com/system-inc/adamic/internal/load"}); found {
		t.Fatal("a unit with no record at that base was found")
	}
	if _, found, _ := records.Latest(main, PlanUnit{UnitKey: "mainflow"}); found {
		t.Fatal("a unit with no name was matched")
	}
	served := log.served
	// A later void record of flow at main says nothing of main; the failed one before it stands.
	log.events = append(log.events, decided(7, main, "mainflow", Void, nil))
	tests, found, _ = records.Latest(main, PlanUnit{UnitKey: "branchflow", Name: flowPackage})
	if !found || len(tests) != 3 {
		t.Fatalf("after a void: %v %v, want the failed record", tests, found)
	}
	if log.served != served+1 {
		t.Fatalf("served %d events for one new one: the log was read again from the start", log.served-served)
	}
	// A newer verify's pass of flow at main replaces the failed record.
	log.events = append(log.events, planned(8, main, "mainflow2", flowPackage), decided(9, main, "mainflow2", Passed, nil))
	if tests, found, _ := records.Latest(main, PlanUnit{UnitKey: "branchflow", Name: flowPackage}); !found || len(tests) != 0 {
		t.Fatalf("after main's newer pass: %v %v, want passed", tests, found)
	}
}

// Loom's proof (Oct 11 02:02Z): landable-4's internal/flow fails TestFlowCorpusRemainder with its corpus subtests on the
// branch, alone on the branch and alone on main's base, the same way main's verify recorded it: main's red, excused, and
// the run green. One more failure than main's record, or a second top-level test, is the branch's. Mutants: main's
// records unread (NoMainRecords: red); subtests counted as separate tests (red); the subset unchecked (an extra
// failure excused).
func TestAFailureMainsVerifySharesIsExcusedAsMainsRed(t *testing.T) {
	main := strings.Repeat("e", 40)
	cases := []struct {
		name   string
		branch []TestOutcome // the failures on the branch, first attempt and alone
		cause  string
		run    string
	}{
		{"flow fails the same way on both sides", flowFailing("a.a", "b.a"), CauseMainRed, "green"},
		{"flow fails fewer of the same subtests", flowFailing("a.a"), CauseMainRed, "green"},
		{"flow fails a subtest main's record doesn't", flowFailing("a.a", "c.a"), CauseChange, "red"},
		{"flow fails a second top-level test too", append(flowFailing("a.a"), TestOutcome{Package: flowPackage, Test: "TestOther", Outcome: "fail"}), CauseChange, "red"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			log := &stubLog{events: []LogEvent{planned(1, main, "mainflow", flowPackage), decided(2, main, "mainflow", Failed, flowFailing("a.a", "b.a"))}}
			failedFlow := Finished{Attempt: Attempt{Status: Failed, Exit: 1}, Tests: c.branch}
			h := newHarness()
			h.runs["u"] = failedFlow
			h.script("u", futureTree, failedFlow)
			h.script("u", main, Finished{Attempt: Attempt{Status: Failed, Exit: 1}, Tests: flowFailing("a.a", "b.a")})
			loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: NewLogMainRecords(log), Queue: h.queue, Blobs: h.blobs, Now: func() time.Time { return time.Date(2026, 10, 11, 2, 30, 0, 0, time.UTC) }}
			post, err := loop.JudgeFuture(Job{Record: ChangeRecord{Change: "chg_2k1w30yx", Sha: futureTree, Base: main, Owner: "system_adamic_loom"}, Change: "chg_2k1w30yx",
				Future: futureTree, Base: main, Run: "run-1", Plan: []PlanUnit{{UnitKey: "u", Name: flowPackage, Kind: KindTest}}})
			if err != nil {
				t.Fatal(err)
			}
			if record := recordOf(t, post, "u"); record.Status != Failed || record.Cause != c.cause {
				t.Fatalf("record %+v, want failed %s", record, c.cause)
			}
			if post.Decision.Status != c.run || (c.cause == CauseMainRed) != (len(post.Decision.Excused) == 1) {
				t.Fatalf("decision %+v, want %s", post.Decision, c.run)
			}
		})
	}
}

// A listed unit carries its plan's name to main's records. Mutant: the name left off (nothing is ever matched).
func TestAListedUnitCarriesItsNameToMainsRecords(t *testing.T) {
	if unit := planUnitOf(PlannedUnitWire{UnitKey: "u", Name: flowPackage, KeyParts: json.RawMessage(`{"kind":"test"}`)}); unit.Name != flowPackage || unit.Kind != KindTest {
		t.Fatalf("planned unit %+v", unit)
	}
}
