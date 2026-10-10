package placer

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/treebuilder"
)

var treeKey = strings.Repeat("7", 64)

// withTree is units with the tree key on each test and product unit, as the planner posts them.
func withTree(t *testing.T, units []judge.PlannedUnitWire, tree string) []judge.PlannedUnitWire {
	t.Helper()
	carried := []judge.PlannedUnitWire{}
	for _, unit := range units {
		var parts planner.KeyParts
		if err := json.Unmarshal(unit.KeyParts, &parts); err != nil {
			t.Fatal(err)
		}
		if planner.RunsTreeBuild(parts.Kind) {
			unit.Tree = tree
		}
		carried = append(carried, unit)
	}
	return carried
}

// trees is the store's indexes and the tree builder's records, as TreeState reads them, with every read counted.
type trees struct {
	indexed map[string]bool
	newest  map[string]treebuilder.Record
	reads   int
}

func (state *trees) read(tree string) (TreeState, error) {
	state.reads++
	newest, found := state.newest[tree]
	return TreeState{Indexed: state.indexed[tree], Newest: newest, Found: found}, nil
}

func newTrees(h *harness) *trees {
	state := &trees{indexed: map[string]bool{}, newest: map[string]treebuilder.Record{}}
	h.placer.Trees, h.placer.TreeState = true, state.read
	return state
}

// With the gate set, an attempt waits, held (neither started nor voided), until its tree's index is up, then is
// placed with every test and product unit's job naming the tree and no phase's. Mutants: Tree never set; an attempt
// placed before its index; a phase job naming the tree.
func TestAnAttemptIsHeldUntilItsTreeIsUpThenPlacedNamingIt(t *testing.T) {
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: withTree(t, everyKind(t), treeKey)}}
	ledger := &MemoryLedger{}
	h := newHarness(t, source, ledger)
	state := newTrees(h)
	h.placeOnce(t, 0)
	h.now = h.now.Add(TreeWaitBound - time.Minute)
	h.placeOnce(t, 0)
	if len(h.placements) != 0 || len(h.voids) != 0 {
		t.Fatalf("while its tree builds: placements %d, voids %v", len(h.placements), h.voids)
	}
	if record, found := ledger.Find(tree, 1); !found || record.Held == "" || record.placed() || record.At != "2026-10-10T09:00:00Z" {
		t.Fatalf("the held attempt's record is %+v", record)
	}
	state.indexed[treeKey] = true
	h.placeOnce(t, 1)
	placement := h.placements[0]
	test, _ := unitOf(t, placement, testKey)
	product, _ := unitOf(t, placement, productKey)
	phase, _ := unitOf(t, placement, phaseKey)
	if test.Test.Tree != treeKey || product.Test.Tree != treeKey || phase.Test.Tree != "" {
		t.Fatalf("trees named: test %q, product %q, phase %q", test.Test.Tree, product.Test.Tree, phase.Test.Tree)
	}
	if record, _ := ledger.Find(tree, 1); !record.placed() || len(record.Placed) != 3 {
		t.Fatalf("the placed attempt's record is %+v", record)
	}
}

// With the gate unset, no job names a tree and nothing waits on one, whatever the plan carries and the store holds:
// an older runner refuses a job carrying one. Mutant: the gate not read.
func TestWithTheGateOffNoJobNamesATree(t *testing.T) {
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: withTree(t, everyKind(t), treeKey)}}
	h := newHarness(t, source, &MemoryLedger{})
	state := newTrees(h)
	h.placer.Trees = false
	h.placeOnce(t, 1)
	for _, unit := range h.placements[0].Job.Units {
		if unit.Test.Tree != "" {
			t.Fatalf("unit %s names tree %s with the gate off", unit.Id, unit.Test.Tree)
		}
	}
	if state.reads != 0 {
		t.Fatalf("the gate off read %d trees", state.reads)
	}
	// A plan from before tree keys places as before too.
	other := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)}}, &MemoryLedger{})
	other.placeOnce(t, 1)
}

// A tree that never comes is voided as Loom's past the bound, naming the tree and what the builder last said, and the
// wait survives a restart: it's counted from the ledger's held record, never from the new placer's start. Mutants: no
// bound; the wait counted from each pass.
func TestATreeThatNeverComesIsVoidedPastTheBoundNamed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placed.jsonl")
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: withTree(t, everyKind(t), treeKey)}}
	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, source, ledger)
	newTrees(h)
	h.placeOnce(t, 0)
	ledger.Close()
	reopened, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := newHarness(t, source, reopened)
	state := newTrees(restarted)
	state.newest[treeKey] = treebuilder.Record{Tree: treeKey, Event: treebuilder.Refused, At: "2026-10-10T09:05:00Z", Cause: "the cache base has 120.0 GB free, under its 200 GB floor"}
	restarted.now = h.now.Add(TreeWaitBound - time.Second)
	restarted.placeOnce(t, 0)
	if len(restarted.voids) != 0 {
		t.Fatalf("voided before the bound: %v", restarted.voids)
	}
	restarted.now = h.now.Add(TreeWaitBound)
	restarted.placeOnce(t, 0)
	if len(restarted.voids) != 1 || !strings.Contains(restarted.voids[0], "its tree "+treeKey+"'s index wasn't in the store within 30m0s") ||
		!strings.Contains(restarted.voids[0], "under its 200 GB floor") || len(restarted.placements) != 0 {
		t.Fatalf("voids %v, placements %d", restarted.voids, len(restarted.placements))
	}
	if record, _ := reopened.Find(tree, 1); record.Void == "" || !record.placed() {
		t.Fatalf("the voided attempt's record is %+v", record)
	}
	if TreeWaitBound >= judge.StaleAfter {
		t.Fatalf("the bound %v isn't under the judge's %v backstop", TreeWaitBound, judge.StaleAfter)
	}
}

// A tree whose build failed on Workshop voids the attempt at once as Loom's, naming why, while the failure stands; one
// past RetryAfter is being built again, so the attempt waits. A plan naming no tree, or two, is voided, named. Mutants:
// a standing failure waited on; a stale one voided.
func TestAFailedTreeOrAPlanNamingNoOneTreeIsVoidedNamed(t *testing.T) {
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: withTree(t, everyKind(t), treeKey)}}
	h := newHarness(t, source, &MemoryLedger{})
	state := newTrees(h)
	failed := treebuilder.Record{Tree: treeKey, Event: treebuilder.Failed, At: h.now.Add(-time.Minute).Format(time.RFC3339), Cause: "checking it out keyless: 502"}
	state.newest[treeKey] = failed
	h.placeOnce(t, 0)
	if len(h.voids) != 1 || !strings.Contains(h.voids[0], "its tree "+treeKey+" wasn't built on Workshop") || !strings.Contains(h.voids[0], "502") {
		t.Fatalf("voids %v", h.voids)
	}
	stale := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: withTree(t, everyKind(t), treeKey)}}, &MemoryLedger{})
	staleState := newTrees(stale)
	failed.At = stale.now.Add(-treebuilder.RetryAfter).Format(time.RFC3339)
	staleState.newest[treeKey] = failed
	stale.placeOnce(t, 0)
	if len(stale.voids) != 0 {
		t.Fatalf("a failure the builder is retrying voided: %v", stale.voids)
	}

	units := withTree(t, everyKind(t), treeKey)
	units[1].Tree = strings.Repeat("8", 64)
	for name, plan := range map[string][]judge.PlannedUnitWire{"two trees": units, "no tree": everyKind(t)} {
		other := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: plan}}, &MemoryLedger{})
		otherState := newTrees(other)
		otherState.indexed[treeKey] = true
		other.placeOnce(t, 0)
		want := map[string]string{"two trees": "its units carry 2 tree keys", "no tree": "its plan carries no tree key"}[name]
		if len(other.voids) != 1 || !strings.Contains(other.voids[0], want) {
			t.Fatalf("%s: voids %v", name, other.voids)
		}
	}
}
