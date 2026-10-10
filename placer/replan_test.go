package placer

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
)

// A plan gone stale under Loom asks for a new one (#r12yqbg). Each mutant below must make a test here fail:
//
//	no replan asked after a stale plan's void: TestAPinMoveReplansWithNoHand, TestAPlanWithNoTreeKeyAsksForANewOne
//	a replan asked for a cause a new plan doesn't fix (a unit too big, a mix): TestOnlyAStalePlanAsksForANewOne
//	the stale check reading any unplaced unit as an unserved runner: TestOnlyAStalePlanAsksForANewOne
//	a fresh plan as stale as the last asked about again: TestAFreshPlanAsStaleAsTheLastIsLeftForAHand
//	a replan asked while the void is held (no void posted): TestAHeldVoidAsksNothing
//	an ask Queue refused passing silently: TestARefusedAskIsAnError

var movedRunner = strings.Repeat("9", 64)

// rekeyed is units with every test unit keyed on runner instead, as a plan from before a pin move names the old one.
func rekeyed(t *testing.T, units []judge.PlannedUnitWire, runner string) []judge.PlannedUnitWire {
	t.Helper()
	moved := []judge.PlannedUnitWire{}
	for _, unit := range units {
		var parts planner.KeyParts
		if err := json.Unmarshal(unit.KeyParts, &parts); err != nil {
			t.Fatal(err)
		}
		if parts.Kind == "test" {
			parts.Tools.Runner = runner
			unit = plannedUnit(t, unit.UnitKey, unit.Name, unit.Decision, parts)
		}
		moved = append(moved, unit)
	}
	return moved
}

// asks records every replan the placer asks of Queue, and answers each with err.
type asks struct {
	reasons []string
	err     error
}

func (asked *asks) on(h *harness) *asks {
	h.placer.Unplan = func(future judge.PlannedFuture, reason string) error {
		asked.reasons = append(asked.reasons, future.Future+" "+reason)
		return asked.err
	}
	return asked
}

// futureList is Queue's planned listing, which the test changes between passes as Queue would.
type futureList struct{ futures []judge.PlannedFuture }

func (list *futureList) Planned() ([]judge.PlannedFuture, error) { return list.futures, nil }

// The witness on Oct 10: release 5 moved the pin, and the plan's test unit names a runner no pool serves. The attempt is
// voided as Loom's and the placer asks Queue for a new plan at once; the planner's fresh plan, keyed on the runner the
// pools serve, is placed on the next attempt. No hand.
func TestAPinMoveReplansWithNoHand(t *testing.T) {
	list := &futureList{futures: []judge.PlannedFuture{{Future: tree, Base: base, Attempt: 1, Units: rekeyed(t, everyKind(t), movedRunner)}}}
	ledger := &MemoryLedger{}
	h := newHarness(t, list, ledger)
	asked := (&asks{}).on(h)
	h.placeOnce(t, 0)
	if len(h.voids) != 1 || !strings.Contains(h.voids[0], "no pool takes a test unit on runner 999999999999") {
		t.Fatalf("voids %v", h.voids)
	}
	if len(asked.reasons) != 1 || !strings.HasPrefix(asked.reasons[0], tree+" not placed: test unit ") ||
		!strings.Contains(asked.reasons[0], "its key names runner 999999999999, which no pool serves") || !strings.Contains(asked.reasons[0], "#r12yqbg") {
		t.Fatalf("asked %q", asked.reasons)
	}
	if record, _ := ledger.Find(tree, 1); record.Void == "" || record.Replan == "" {
		t.Fatalf("attempt 1's record %+v", record)
	}
	// Queue relists it planned anew, past the withdrawn plan's attempts (#0zndrgw): placed, nothing more asked.
	list.futures = []judge.PlannedFuture{{Future: tree, Base: base, Attempt: 3, Units: everyKind(t)}}
	h.placeOnce(t, 1)
	if len(h.voids) != 1 || len(asked.reasons) != 1 || h.placements[0].Run != "future-"+tree+"-3" {
		t.Fatalf("voids %v, asked %q, placed %v", h.voids, asked.reasons, h.placements)
	}
}

// A plan from before tree keys names no build, so it never runs one: asked for a new one too.
func TestAPlanWithNoTreeKeyAsksForANewOne(t *testing.T) {
	h := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)}}, &MemoryLedger{})
	newTrees(h)
	asked := (&asks{}).on(h)
	h.placeOnce(t, 0)
	if len(h.voids) != 1 || len(asked.reasons) != 1 || !strings.Contains(asked.reasons[0], "its plan carries no tree key") {
		t.Fatalf("voids %v, asked %q", h.voids, asked.reasons)
	}
}

// A unit no pool can hold (more memory than any has) isn't the plan's staleness: a new plan wouldn't place it, so the
// void stands alone. Nor is a plan with one stale unit and one too big: a replan fixes only half of it.
func TestOnlyAStalePlanAsksForANewOne(t *testing.T) {
	big := everyKind(t)
	big[0].Resources.MemoryMegabytes = 1 << 20
	h := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: big}}, &MemoryLedger{})
	asked := (&asks{}).on(h)
	h.placeOnce(t, 0)
	if len(h.voids) != 1 || len(asked.reasons) != 0 {
		t.Fatalf("too big: voids %v, asked %q", h.voids, asked.reasons)
	}
	mixed := rekeyed(t, everyKind(t), movedRunner)
	// The phase unit, which a phase pool would take but for its size.
	mixed[2].Resources.MemoryMegabytes = 1 << 20
	h = newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: mixed}}, &MemoryLedger{})
	asked = (&asks{}).on(h)
	h.placeOnce(t, 0)
	if len(h.voids) != 1 || len(asked.reasons) != 0 {
		t.Fatalf("stale and too big: voids %v, asked %q", h.voids, asked.reasons)
	}
}

// The pools, not the plan, are what's missing when a fresh plan names the same unserved runner: the placer voids it
// again once its hold window ends, and doesn't ask a second time. A void held inside the window asks nothing either.
func TestAFreshPlanAsStaleAsTheLastIsLeftForAHand(t *testing.T) {
	units := rekeyed(t, everyKind(t), movedRunner)
	list := &futureList{futures: []judge.PlannedFuture{{Future: tree, Base: base, Attempt: 1, Units: units}}}
	h := newHarness(t, list, &MemoryLedger{})
	asked := (&asks{}).on(h)
	h.placeOnce(t, 0)
	list.futures = []judge.PlannedFuture{{Future: tree, Base: base, Attempt: 3, Units: units}}
	h.now = h.now.Add(time.Minute)
	h.placeOnce(t, 0)
	if len(h.voids) != 1 || len(asked.reasons) != 1 {
		t.Fatalf("held inside the window: voids %v, asked %q", h.voids, asked.reasons)
	}
	h.now = h.now.Add(h.placer.UnfitEvery)
	h.placeOnce(t, 0)
	if len(h.voids) != 2 || len(asked.reasons) != 1 {
		t.Fatalf("voided again after the window: voids %v, asked %q", h.voids, asked.reasons)
	}
}

// Queue refusing the ask is an error the pass returns, never silence; the void stands either way.
func TestARefusedAskIsAnError(t *testing.T) {
	h := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: rekeyed(t, everyKind(t), movedRunner)}}, &MemoryLedger{})
	(&asks{err: errors.New("409: future was decided green or red, so its plan stands")}).on(h)
	if _, err := h.placer.PlaceOnce(); err == nil || !strings.Contains(err.Error(), "asking Queue to plan it again") {
		t.Fatalf("PlaceOnce: %v", err)
	}
	if len(h.voids) != 1 {
		t.Fatalf("voids %v", h.voids)
	}
}

// A void held inside its window posts nothing, so nothing is asked of Queue, even for a plan the placer hasn't asked
// about: its last ask was for another plan, and the void it would follow never happened.
func TestAHeldVoidAsksNothing(t *testing.T) {
	units := rekeyed(t, everyKind(t), movedRunner)
	list := &futureList{futures: []judge.PlannedFuture{{Future: tree, Base: base, Attempt: 1, Units: units}}}
	h := newHarness(t, list, &MemoryLedger{})
	asked := (&asks{}).on(h)
	h.placeOnce(t, 0)
	// The same stale units but the reused one keyed anew: another plan, the same unplaced causes, so its void is held.
	other := append([]judge.PlannedUnitWire{}, units...)
	other[3].UnitKey = strings.Repeat("5", 64)
	list.futures = []judge.PlannedFuture{{Future: tree, Base: base, Attempt: 3, Units: other}}
	h.now = h.now.Add(time.Minute)
	h.placeOnce(t, 0)
	if len(h.voids) != 1 || len(asked.reasons) != 1 {
		t.Fatalf("voids %v, asked %q", h.voids, asked.reasons)
	}
}
