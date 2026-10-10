package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func planFixture(t *testing.T) (tree, gateTools string) {
	t.Helper()
	tree, gateTools = t.TempDir(), t.TempDir()
	writeFiles(t, tree, map[string]string{
		"go.mod":        "module example.com/plan\n\ngo 1.22\n",
		"a/a.go":        "package a\n",
		"a/a_test.go":   "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n",
		"b/b.go":        "package b\n",
		"b/b_test.go":   "package b\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {}\n",
		"compiler/c.go": "package compiler\n",
		"cloud/fast-gate/compiler-dependencies.json": `{"version": 1, "packages": {"a": ["compiler"]}}`,
	})
	writeFiles(t, gateTools, map[string]string{
		"cloud/fast-gate/executors.txt": "# no reads\n",
		// run.py's --list-units as the gate tools hold it: the phase units it lists, one per line.
		"cloud/fast-gate/run.py": "import sys\nif '--list-units' in sys.argv:\n    print('build')\n    print('vet')\n    print('census')\n",
	})
	for _, directory := range []string{tree, gateTools} {
		for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "fixture"}} {
			if output, err := exec.Command("git", append([]string{"-C", directory}, arguments...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", arguments, err, output)
			}
		}
	}
	return tree, gateTools
}

func planOf(t *testing.T, tree, gateTools string, index VerdictIndex) map[string]PlannedResult {
	t.Helper()
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	results, err := PlanTree(tree, gateTools, tools, index, false)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]PlannedResult{}
	for _, result := range results {
		byName[result.Name] = result
	}
	return byName
}

// A future's plan holds every tested package; a passed key is reused, and an edit to a package's declared compiler
// package runs it again while its neighbour keeps its verdict.
func TestPlanTreeReusesUnmovedKeysAndRunsMovedOnes(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	first := planOf(t, tree, gateTools, MemoryIndex{})
	if len(first) != 2 || first["example.com/plan/a"].Decision != "run" || first["example.com/plan/b"].Decision != "run" {
		t.Fatalf("a first plan runs both tested packages: %+v", first)
	}
	index := MemoryIndex{}
	for _, result := range first {
		index[result.UnitKey] = Verdict{UnitKey: result.UnitKey, Status: "passed", Run: "run-1"}
	}
	if again := planOf(t, tree, gateTools, index); again["example.com/plan/a"].Decision != "reuse" || again["example.com/plan/b"].Decision != "reuse" {
		t.Fatalf("an unchanged tree reuses every passed key: %+v", again)
	}
	path := filepath.Join(tree, "compiler/c.go")
	os.WriteFile(path, []byte("package compiler\n\nconst Moved = 1\n"), 0o644)
	moved := planOf(t, tree, gateTools, index)
	if moved["example.com/plan/a"].Decision != "run" {
		t.Errorf("editing a's declared compiler package left a reused: %s", moved["example.com/plan/a"].Reason)
	}
	if moved["example.com/plan/b"].Decision != "reuse" {
		t.Errorf("editing a's compiler package ran b, which doesn't declare it: %s", moved["example.com/plan/b"].Reason)
	}
}

// A parity plan is exactly the box record's selection: its packages and no others, every unit run though its key
// passed, a selected package with no tests included, and a split package's unit runs exactly its named tests.
func TestPlanSelectedRunsExactlyTheSelection(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	selection := ParitySelect{Packages: []string{"example.com/plan/a", "example.com/plan/compiler"}, Tests: map[string][]string{"example.com/plan/a": {"TestZ.1", "TestA"}}}
	results, err := PlanSelected(tree, gateTools, tools, selection, ParityInputs{})
	if err != nil {
		t.Fatal(err)
	}
	// compiler has no tests, but the box record ran it (go test passes it), so the plan holds it too.
	if len(results) != 2 || results[0].Name != "example.com/plan/a" || results[1].Name != "example.com/plan/compiler" || results[0].Decision != "run" || results[1].Decision != "run" {
		t.Fatalf("a parity plan of a and the test-less compiler: %+v", results)
	}
	if run := results[0].KeyParts.Select.Run; run != `^(TestA|TestZ\.1)$` {
		t.Errorf("a's unit runs %q, want exactly TestA and TestZ.1", run)
	}
	for _, wrong := range []ParitySelect{
		{Packages: []string{"example.com/plan/missing"}},
		{Packages: []string{"example.com/plan/a"}, Tests: map[string][]string{"example.com/plan/a": {"TestA/sub"}}},
	} {
		if _, err := PlanSelected(tree, gateTools, tools, wrong, ParityInputs{}); err == nil {
			t.Errorf("the selection %+v was planned instead of refused", wrong)
		}
	}
}

// A parity unit carries what the box record ran with as keyed parts: the gate inputs, ADAMIC_GATE_CHANGED as the
// sha256 of the changed-paths file the strict runner writes (sorted, newline-joined), and the sample. Each moves the key.
func TestAParityUnitKeysWhatTheBoxRanWith(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	selection := ParitySelect{Packages: []string{"example.com/plan/a"}}
	inputs := ParityInputs{GateInputs: strings.Repeat("4", 64), ChangedPaths: []string{"b/b.go", "a/a.go"}}
	keyOf := func(inputs ParityInputs) PlannedResult {
		results, err := PlanSelected(tree, gateTools, tools, selection, inputs)
		if err != nil {
			t.Fatal(err)
		}
		return results[0]
	}
	planned := keyOf(inputs)
	sum := sha256.Sum256([]byte("a/a.go\nb/b.go\n"))
	if planned.KeyParts.GateInputs != inputs.GateInputs || planned.KeyParts.Env["ADAMIC_GATE_CHANGED"] != hex.EncodeToString(sum[:]) || planned.KeyParts.Env["ADAMIC_GATE_UNCACHED"] != "1" {
		t.Fatalf("the parity unit's parts %+v don't hold the gate inputs, the runner's changed-paths file and the switches", planned.KeyParts)
	}
	for name, moved := range map[string]ParityInputs{
		"gate inputs": {GateInputs: strings.Repeat("5", 64), ChangedPaths: inputs.ChangedPaths},
		"paths":       {GateInputs: inputs.GateInputs, ChangedPaths: []string{"a/a.go"}},
		"sample":      {GateInputs: inputs.GateInputs, ChangedPaths: inputs.ChangedPaths, Sample: strings.Repeat("c", 40)},
	} {
		if keyOf(moved).UnitKey == planned.UnitKey {
			t.Errorf("another %s kept the key", name)
		}
	}
	if same := keyOf(ParityInputs{GateInputs: inputs.GateInputs, ChangedPaths: []string{"a/a.go", "b/b.go"}}); same.UnitKey != planned.UnitKey {
		t.Error("the same paths in another order moved the key")
	}
}

// A parity run that names no selection (Release's mutants) plans every tested package with the box's inputs, run.
func TestAParityRunWithoutASelectionPlansEveryTestedPackageWithItsInputs(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	inputs := ParityInputs{GateInputs: strings.Repeat("4", 64), ChangedPaths: []string{"a/a.go"}}
	results, err := PlanSelected(tree, gateTools, Tools{Go: "go1.27.0"}, ParitySelect{}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("planned %d units, want both tested packages", len(results))
	}
	for _, result := range results {
		if result.Decision != "run" || result.KeyParts.GateInputs != inputs.GateInputs || result.KeyParts.Env["ADAMIC_GATE_CHANGED"] == "" {
			t.Errorf("%s: %s, gate inputs %q, env %v", result.Name, result.Decision, result.KeyParts.GateInputs, result.KeyParts.Env)
		}
	}
}

// RunNames reads back exactly the names exactRun wrote, and refuses any pattern exactRun can't have written.
func TestRunNamesIsExactRunsInverse(t *testing.T) {
	t.Parallel()
	names := []string{"TestA", "TestZ.1", "TestWASIUnit03"}
	run, err := exactRun(names)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := RunNames(run); !ok || strings.Join(got, ",") != "TestA,TestWASIUnit03,TestZ.1" {
		t.Fatalf("RunNames(%q) = %v %v", run, got, ok)
	}
	for _, other := range []string{"", "TestA", "^TestA", "^(TestA|Test.*)$", "^()$"} {
		if names, ok := RunNames(other); ok {
			t.Errorf("RunNames(%q) read names %v from a pattern exactRun never writes", other, names)
		}
	}
}

// Proof 3's by-key plan: against the parent's keys made with the future's own parity parts, the unit the change
// reaches runs, the rest reuse, and every unit carries exactly the key its uncached parity run used, so the witness
// compare matches each reused unit to its uncached verdict.
func TestPlanByKeyReusesOnTheUncachedRunsKeys(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	inputs := ParityInputs{GateInputs: strings.Repeat("4", 64), ChangedPaths: []string{"a/a.go"}}
	path := filepath.Join(tree, "a/a.go")
	original, _ := os.ReadFile(path)
	checkout := func(sha string) (string, func(), error) {
		content := original
		if sha == "mutant" {
			content = append(append([]byte{}, original...), "\nconst Mutated = 1\n"...)
		}
		return tree, func() {}, os.WriteFile(path, content, 0o644)
	}
	byKey, err := PlanByKey(checkout, "parent-sha-0000", "mutant", gateTools, tools, ParitySelect{}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	uncached, err := PlanSelected(tree, gateTools, tools, ParitySelect{}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	witnessKey := map[string]string{}
	for _, unit := range uncached {
		witnessKey[unit.Name] = unit.UnitKey
	}
	decisions := map[string]string{}
	for _, unit := range byKey {
		decisions[unit.Name] = unit.Decision
		if unit.UnitKey != witnessKey[unit.Name] {
			t.Errorf("%s's by-key key %s isn't its uncached run's %s", unit.Name, unit.UnitKey, witnessKey[unit.Name])
		}
	}
	if decisions["example.com/plan/a"] != "run" || decisions["example.com/plan/b"] != "reuse" {
		t.Fatalf("by-key decisions %v: want a run (the change reaches it) and b reused", decisions)
	}
}

// A change's plan keys the gate inputs on every unit and the change's paths only on the units whose tests import
// internal/gatesample, read from go list. Mutant: a reader keyed without ADAMIC_GATE_CHANGED is refused.
func TestAChangeKeysGateInputsEverywhereAndItsPathsOnlyOnGateSampleReaders(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	writeFiles(t, tree, map[string]string{
		"internal/gatesample/sample.go": "package gatesample\n\nconst Stride = 1\n",
		"c/c.go":                        "package c\n",
		"c/c_test.go":                   "package c\n\nimport (\n\t\"testing\"\n\n\t\"example.com/plan/internal/gatesample\"\n)\n\nfunc TestC(t *testing.T) { _ = gatesample.Stride }\n",
	})
	if output, err := exec.Command("git", "-C", tree, "add", ".").CombinedOutput(); err != nil {
		t.Fatalf("git add: %v %s", err, output)
	}
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	inputs := ParityInputs{GateInputs: strings.Repeat("4", 64), ChangedPaths: []string{"a/a.go"}}
	results, err := PlanChange(tree, gateTools, tools, MemoryIndex{}, false, inputs)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		reader := result.Name == "example.com/plan/c"
		if result.KeyParts.GateInputs != inputs.GateInputs || (result.KeyParts.Env["ADAMIC_GATE_CHANGED"] != "") != reader {
			t.Errorf("%s: gate inputs %q, env %v (a gatesample reader: %v)", result.Name, result.KeyParts.GateInputs, result.KeyParts.Env, reader)
		}
	}
	dropped := func(tree, gateTools string, unit Unit, tools Tools, compilerPackages []string) (KeyParts, error) {
		parts, err := KeyFor(tree, gateTools, unit, tools, compilerPackages)
		delete(parts.Env, "ADAMIC_GATE_CHANGED")
		return parts, err
	}
	if _, err := planTree(tree, gateTools, tools, MemoryIndex{}, false, dropped, nil, &inputs); err == nil {
		t.Error("a gatesample reader keyed without ADAMIC_GATE_CHANGED was planned")
	}
}

// A future's phase units are run.py's own list at the gate tools (but census, the judge's step) plus gofmt, each kind phase, run, keyed on the
// tree's object and the tools commit, with gofmt alone carrying the change's paths. Another tree moves every phase key.
func TestPhaseUnitsAreRunPysListPlusGofmtKeyedOnTheWholeTree(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	inputs := ParityInputs{GateInputs: strings.Repeat("4", 64), ChangedPaths: []string{"a/a.go"}}
	phases, err := PhaseUnits(tree, gateTools, strings.Repeat("b", 40), strings.Repeat("c", 40), tools, inputs)
	if err != nil {
		t.Fatal(err)
	}
	names, keys := []string{}, map[string]string{}
	for _, phase := range phases {
		names = append(names, phase.Name)
		keys[phase.Name] = phase.UnitKey
		gofmt := phase.Name == "phase:gofmt"
		if phase.KeyParts.Kind != "phase" || phase.Decision != "run" || phase.KeyParts.GateInputs != inputs.GateInputs || (phase.KeyParts.Env["ADAMIC_GATE_CHANGED"] != "") != gofmt {
			t.Errorf("%s: %+v, decision %s", phase.Name, phase.KeyParts, phase.Decision)
		}
	}
	if strings.Join(names, ",") != "phase:build,phase:vet,phase:gofmt" {
		t.Fatalf("phase units %v, want run.py's build and vet, then gofmt", names)
	}
	writeFiles(t, tree, map[string]string{"z.txt": "another tree\n"})
	for _, arguments := range [][]string{{"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "moved"}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	moved, err := PhaseUnits(tree, gateTools, strings.Repeat("b", 40), strings.Repeat("c", 40), tools, inputs)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range moved {
		if keys[phase.Name] == phase.UnitKey {
			t.Errorf("%s kept its key on another tree", phase.Name)
		}
	}
}
