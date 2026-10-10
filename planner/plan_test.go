package planner

import (
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
	writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/executors.txt": "# no reads\n"})
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
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
// passed, and a split package's unit runs exactly its named tests.
func TestPlanSelectedRunsExactlyTheSelection(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	selection := ParitySelect{Packages: []string{"example.com/plan/a"}, Tests: map[string][]string{"example.com/plan/a": {"TestZ.1", "TestA"}}}
	results, err := PlanSelected(tree, gateTools, tools, selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Name != "example.com/plan/a" || results[0].Decision != "run" {
		t.Fatalf("a parity plan of a alone: %+v", results)
	}
	if run := results[0].KeyParts.Select.Run; run != `^(TestA|TestZ\.1)$` {
		t.Errorf("a's unit runs %q, want exactly TestA and TestZ.1", run)
	}
	for _, wrong := range []ParitySelect{
		{Packages: []string{"example.com/plan/missing"}},
		{Packages: []string{"example.com/plan/a"}, Tests: map[string][]string{"example.com/plan/a": {"TestA/sub"}}},
	} {
		if _, err := PlanSelected(tree, gateTools, tools, wrong); err == nil {
			t.Errorf("the selection %+v was planned instead of refused", wrong)
		}
	}
}
