package judge

import (
	"reflect"
	"strings"
	"testing"
)

func futures(verdictsPerFuture ...[]Verdict) []Future {
	changes := []string{"chg_A", "chg_B", "chg_C", "chg_D"}
	result := []Future{}
	for index, verdicts := range verdictsPerFuture {
		result = append(result, Future{Changes: append([]string{}, changes[:index+1]...), Tree: strings.Repeat(string(rune('a'+index)), 40), Verdicts: verdicts})
	}
	return result
}

func blamedChanges(blames []Blame) map[string]string {
	result := map[string]string{}
	for _, blame := range blames {
		result[blame.UnitKey] = blame.Change
	}
	return result
}

func TestBlameReadsTheFutures(t *testing.T) {
	cases := []struct {
		name     string
		futures  []Future
		blamed   map[string]string
		unblamed []string
	}{
		{"every future green blames nobody",
			futures([]Verdict{verdict("u", Passed, "")}, []Verdict{verdict("u", Passed, "")}), map[string]string{}, nil},
		{"red in the first future is the first change's",
			futures([]Verdict{verdict("u", Failed, CauseChange)}), map[string]string{"u": "chg_A"}, nil},
		{"passed before, red after: the change that future adds",
			futures([]Verdict{verdict("u", Passed, "")}, []Verdict{verdict("u", Passed, "")}, []Verdict{verdict("u", Failed, CauseChange)}),
			map[string]string{"u": "chg_C"}, nil},
		{"red at its first red only, never blamed again later",
			futures([]Verdict{verdict("u", Passed, "")}, []Verdict{verdict("u", Failed, CauseChange)}, []Verdict{verdict("u", Failed, CauseChange)}),
			map[string]string{"u": "chg_B"}, nil},
		{"not planned before, red after: the change that future adds",
			futures([]Verdict{verdict("v", Passed, "")}, []Verdict{verdict("v", Passed, ""), verdict("u", Failed, CauseChange)}),
			map[string]string{"u": "chg_B"}, nil},
		{"void before is unblamed, never guessed",
			futures([]Verdict{verdict("u", Void, CauseInfra)}, []Verdict{verdict("u", Failed, CauseChange)}), map[string]string{}, []string{"u"}},
		{"main's red blames nobody in the block",
			futures([]Verdict{verdict("u", Failed, CauseMainRed)}, []Verdict{verdict("u", Failed, CauseMainRed)}), map[string]string{}, nil},
		{"two units, two changes",
			futures([]Verdict{verdict("u", Failed, CauseChange), verdict("v", Passed, "")}, []Verdict{verdict("u", Failed, CauseChange), verdict("v", Failed, CauseChange)}),
			map[string]string{"u": "chg_A", "v": "chg_B"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			blames, unblamed, err := BlameFutures(c.futures)
			if err != nil {
				t.Fatal(err)
			}
			if got := blamedChanges(blames); !reflect.DeepEqual(got, c.blamed) {
				t.Fatalf("blamed %v, want %v", got, c.blamed)
			}
			got := []string{}
			for _, entry := range unblamed {
				got = append(got, entry.UnitKey)
			}
			if len(got) != len(c.unblamed) || (len(got) > 0 && !reflect.DeepEqual(got, c.unblamed)) {
				t.Fatalf("unblamed %v, want %v", got, c.unblamed)
			}
		})
	}
}

func TestFuturesThatArentPrefixesAreRefused(t *testing.T) {
	bad := [][]Future{
		{{Changes: []string{"chg_A", "chg_B"}}},
		{{Changes: []string{"chg_A"}}, {Changes: []string{"chg_X", "chg_B"}}},
	}
	for _, block := range bad {
		if _, _, err := BlameFutures(block); err == nil {
			t.Fatalf("blamed over %+v, want refused", block)
		}
	}
}

func TestTheKickCarriesTheTestTheDiffAndTheRepro(t *testing.T) {
	red := Verdict{UnitKey: "u", Future: strings.Repeat("b", 40), Status: Failed, Cause: CauseChange, Outputs: []string{strings.Repeat("e", 64)},
		Tests: []TestOutcome{outcome("TestA", "pass"), outcome("TestB", "fail")}}
	blame := Blame{Change: "chg_B", UnitKey: "u", Future: 1, Why: "passed in the future before, red here"}
	record := ChangeRecord{Change: "chg_B", Sha: strings.Repeat("2", 40), Base: strings.Repeat("1", 40), Owner: "system_adamic_library"}
	kick, err := KickFor(blame, record, red)
	if err != nil {
		t.Fatal(err)
	}
	if len(kick.Tests) != 1 || kick.Tests[0].Test != "TestB" {
		t.Fatalf("tests %v, want the failing TestB alone", kick.Tests)
	}
	if kick.Diff != strings.Repeat("1", 40)+".."+strings.Repeat("2", 40) || kick.Repro != "loom repro u" || kick.Owner != "system_adamic_library" || len(kick.Outputs) != 1 {
		t.Fatalf("kick %+v", kick)
	}
	if _, err := KickFor(blame, ChangeRecord{Change: "chg_A"}, red); err == nil {
		t.Fatal("kicked with another change's record")
	}
	if _, err := KickFor(blame, record, Verdict{UnitKey: "u", Status: Failed, Cause: CauseMainRed}); err == nil {
		t.Fatal("kicked on main's red")
	}
}
