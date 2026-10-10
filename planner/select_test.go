package planner

import (
	"os"
	"path/filepath"
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

// Mutant (Release, Oct 10 02:43Z): a key whose pass ran on a warm shared cache has a passed verdict, so Choose reuses
// it; listed in the no-reuse file, the plan runs it and names why, while an unlisted passed key still reuses.
func TestUnreusedRunsAListedPassedKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "no-reuse")
	content := "# warm per-worker cache\n" + keyOf("a") + " ran on cloud-box-2's warm cache\n\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	noReuse, err := LoadNoReuse(path)
	if err != nil {
		t.Fatal(err)
	}
	index := MemoryIndex{
		keyOf("a"): {UnitKey: keyOf("a"), Status: "passed", Run: "run-warm"},
		keyOf("b"): {UnitKey: keyOf("b"), Status: "passed", Run: "run-cold"},
	}
	choices, err := Choose([]PlannedUnit{{"warm", keyOf("a")}, {"cold", keyOf("b")}}, index, false)
	if err != nil {
		t.Fatal(err)
	}
	if choices[1].Name != "warm" || choices[1].Action != "reuse" {
		t.Fatalf("the mutant isn't live: Choose alone should reuse the warm pass, got %+v", choices[1])
	}
	byName := map[string]Choice{}
	for _, choice := range Unreused(choices, noReuse) {
		byName[choice.Name] = choice
	}
	if warm := byName["warm"]; warm.Action != "run" || warm.Reused != "" || !strings.Contains(warm.Reason, "cloud-box-2") {
		t.Errorf("a listed key reused a warm pass: %+v", warm)
	}
	if cold := byName["cold"]; cold.Action != "reuse" || cold.Reused != "run-cold" {
		t.Errorf("an unlisted passed key stopped reusing: %+v", cold)
	}
}

func TestLoadNoReuse(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	if noReuse, err := LoadNoReuse(filepath.Join(directory, "missing")); err != nil || len(noReuse) != 0 {
		t.Errorf("a missing file lists no keys, got %v, %v", noReuse, err)
	}
	for name, line := range map[string]string{"no why": keyOf("a"), "short key": "abc why", "upper": strings.Repeat("A", 64) + " why"} {
		path := filepath.Join(directory, strings.ReplaceAll(name, " ", "-"))
		if err := os.WriteFile(path, []byte(line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadNoReuse(path); err == nil {
			t.Errorf("%s: %q was accepted", name, line)
		}
	}
}
