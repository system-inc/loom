package judge

import (
	"reflect"
	"testing"
)

func TestTheWitnessNamesAReusedKeyThatRedsUncached(t *testing.T) {
	plan := []PlannedUnitWire{
		{UnitKey: "reused-fine", Decision: "reuse"},
		{UnitKey: "reused-red", Decision: "reuse"},
		{UnitKey: "reused-unseen", Decision: "reuse"},
		{UnitKey: "reused-void", Decision: "reuse"},
		{UnitKey: "run-red", Decision: "run"},
		{UnitKey: "run-fine", Decision: "run"},
	}
	witness := []Verdict{
		verdict("reused-fine", Passed, ""), verdict("reused-red", Failed, CauseChange), verdict("reused-void", Void, CauseInfra),
		verdict("run-red", Failed, CauseChange), verdict("run-fine", Passed, ""),
	}
	report := CompareWitness(plan, witness)
	want := WitnessReport{KeyFaults: []string{"reused-red"}, Unwitnessed: []string{"reused-unseen"}, Reds: []string{"run-red"}, Voids: []string{"reused-void"}}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("report %+v, want %+v", report, want)
	}
	if report.Clean() {
		t.Fatal("a witness with a key fault reads clean")
	}
}

func TestAWitnessThatCheckedEveryReuseAndFoundNothingIsClean(t *testing.T) {
	plan := []PlannedUnitWire{{UnitKey: "a", Decision: "reuse"}, {UnitKey: "b", Decision: "run"}}
	report := CompareWitness(plan, []Verdict{verdict("a", Passed, ""), verdict("b", Failed, CauseChange)})
	if !report.Clean() || len(report.Reds) != 1 {
		t.Fatalf("report %+v: a run unit's red is an ordinary red, not a key fault", report)
	}
}

func TestAWitnessVoidKeepsItFromCounting(t *testing.T) {
	plan := []PlannedUnitWire{{UnitKey: "a", Decision: "reuse"}}
	if report := CompareWitness(plan, []Verdict{verdict("a", Void, CauseInfra)}); report.Clean() {
		t.Fatalf("report %+v: a void witness can't vouch for the reuse", report)
	}
}
