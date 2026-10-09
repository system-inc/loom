package planner

import (
	"strings"
	"testing"
)

func keyOf(letter string) string { return strings.Repeat(letter, 64) }

func choicesByName(t *testing.T, units []PlannedUnit, index VerdictIndex, uncached bool) map[string]Choice {
	t.Helper()
	choices, err := Choose(units, index, uncached)
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != len(units) {
		t.Fatalf("the plan holds %d units, want %d: every unit is planned, reused or run", len(choices), len(units))
	}
	byName := map[string]Choice{}
	for _, choice := range choices {
		byName[choice.Name] = choice
	}
	return byName
}

// Only an exact key with a decided passed verdict is reused; a failed or void verdict, a new key, or an index
// answering for another key runs.
func TestChooseReusesOnlyAnExactPassedKey(t *testing.T) {
	t.Parallel()
	index := MemoryIndex{
		keyOf("a"): {UnitKey: keyOf("a"), Status: "passed", Run: "run-1"},
		keyOf("b"): {UnitKey: keyOf("b"), Status: "failed", Run: "run-1"},
		keyOf("c"): {UnitKey: keyOf("c"), Status: "void", Run: "run-1"},
		keyOf("e"): {UnitKey: keyOf("f"), Status: "passed", Run: "run-1"},
	}
	units := []PlannedUnit{{"passed", keyOf("a")}, {"failed", keyOf("b")}, {"void", keyOf("c")}, {"new", keyOf("d")}, {"misfiled", keyOf("e")}}
	choices := choicesByName(t, units, index, false)
	for name, want := range map[string]string{"passed": "reuse", "failed": "run", "void": "run", "new": "run", "misfiled": "run"} {
		if choices[name].Action != want {
			t.Errorf("%s: %s (%s), want %s", name, choices[name].Action, choices[name].Reason, want)
		}
	}
	if choices["passed"].Reused != "run-1" {
		t.Errorf("a reused unit names the run whose verdict it reuses, got %q", choices["passed"].Reused)
	}
}

// Uncached mode, the witness's and main's landing's, reuses nothing even when every key passed.
func TestChooseUncachedRunsEverything(t *testing.T) {
	t.Parallel()
	index := MemoryIndex{keyOf("a"): {UnitKey: keyOf("a"), Status: "passed", Run: "run-1"}}
	for name, choice := range choicesByName(t, []PlannedUnit{{"passed", keyOf("a")}}, index, true) {
		if choice.Action != "run" {
			t.Errorf("%s reused a verdict in uncached mode", name)
		}
	}
}

func TestChooseRefusesAMalformedPlan(t *testing.T) {
	t.Parallel()
	if _, err := Choose([]PlannedUnit{{"keyless", ""}}, MemoryIndex{}, false); err == nil {
		t.Error("a unit with no key was planned")
	}
	if _, err := Choose([]PlannedUnit{{"twice", keyOf("a")}, {"twice", keyOf("b")}}, MemoryIndex{}, false); err == nil {
		t.Error("a unit planned twice was accepted")
	}
}
