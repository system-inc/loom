package placer

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/system-inc/loom/judge"
)

// A run no live change wants stops (#drrnnkh). Each mutant below must make a test here fail:
//
//	no run ever stopped: TestAWithdrawnBranchsRunStopsAndItsFutureIsNeverPlacedAgain
//	a run stopped while its change is live and tested there: TestARunItsLiveChangeStillWantsKeepsRunning
//	a run whose change moved to another future kept: TestARunWhoseChangeMovedToAnotherFutureStops
//	a run stopped on a change Queue couldn't say anything about: TestNothingStopsOnAGuess
//	a stop that failed recorded as stopped: TestNothingStopsOnAGuess

const branch = "chg_bbbbbbbbbbbbbbbbbbbbbbbbbb"

// changes is Queue's answer about each change, as ChangeOf reads it, with every change asked recorded.
type changes struct {
	states  map[string][2]string
	err     error
	asked   []string
	stopped []string
	stopErr error
}

func (known *changes) on(h *harness) *changes {
	h.placer.ChangeOf = func(change string) (string, string, error) {
		known.asked = append(known.asked, change)
		if known.err != nil {
			return "", "", known.err
		}
		state := known.states[change]
		return state[0], state[1], nil
	}
	h.placer.Stop = func(run string) error {
		if known.stopErr != nil {
			return known.stopErr
		}
		known.stopped = append(known.stopped, run)
		return nil
	}
	return known
}

// placedThenGone places attempt 1 of the future for branch, then has Queue stop listing it.
func placedThenGone(t *testing.T, known *changes) (*harness, *futureList) {
	list := &futureList{futures: []judge.PlannedFuture{{Future: tree, Base: base, Attempt: 1, Change: judge.PlannedChange{Change: branch}, Units: everyKind(t)}}}
	h := newHarness(t, list, &MemoryLedger{})
	known.on(h)
	h.placeOnce(t, 1)
	if len(known.asked) != 0 || len(known.stopped) != 0 {
		t.Fatalf("a listed future's run was questioned: asked %q, stopped %q", known.asked, known.stopped)
	}
	list.futures = nil
	return h, list
}

func TestAWithdrawnBranchsRunStopsAndItsFutureIsNeverPlacedAgain(t *testing.T) {
	known := &changes{states: map[string][2]string{branch: {"withdrawn", tree}}}
	h, _ := placedThenGone(t, known)
	h.placeOnce(t, 0)
	if !reflect.DeepEqual(known.stopped, []string{"future-" + tree + "-1"}) {
		t.Fatalf("stopped %q", known.stopped)
	}
	record, _ := h.placer.Ledger.Find(tree, 1)
	if record.Stopped != "change "+branch+" is withdrawn: no live change lists its future" || len(h.placer.Ledger.Running()) != 0 {
		t.Fatalf("the record %+v, running %d", record, len(h.placer.Ledger.Running()))
	}
	// Stopped once, and nothing placed for it again.
	h.placeOnce(t, 0)
	if len(known.stopped) != 1 || len(h.placements) != 1 {
		t.Fatalf("stopped %q, placed %d", known.stopped, len(h.placements))
	}
}

// Decided green and waiting to land, a future is unlisted while its change is live and still tested there: its run
// keeps going. So does one placed before runs named their change.
func TestARunItsLiveChangeStillWantsKeepsRunning(t *testing.T) {
	for _, state := range []string{"queued", "building", "testing"} {
		known := &changes{states: map[string][2]string{branch: {state, tree}}}
		h, _ := placedThenGone(t, known)
		h.placeOnce(t, 0)
		if len(known.stopped) != 0 || len(h.placer.Ledger.Running()) != 1 {
			t.Errorf("%s: stopped %q", state, known.stopped)
		}
	}
	known := &changes{states: map[string][2]string{branch: {"withdrawn", tree}}}
	h, _ := placedThenGone(t, known)
	record, _ := h.placer.Ledger.Find(tree, 1)
	record.Change = ""
	h.placer.Ledger.Append(record)
	h.placeOnce(t, 0)
	if len(known.stopped) != 0 || len(known.asked) != 0 {
		t.Fatalf("a run with no change named: asked %q, stopped %q", known.asked, known.stopped)
	}
}

// A branch resubmitted on a new sha is tested in a new future: the old one's run stops though the branch is live.
func TestARunWhoseChangeMovedToAnotherFutureStops(t *testing.T) {
	known := &changes{states: map[string][2]string{branch: {"queued", strings.Repeat("f", 40)}}}
	h, _ := placedThenGone(t, known)
	h.placeOnce(t, 0)
	if len(known.stopped) != 1 {
		t.Fatalf("stopped %q", known.stopped)
	}
	if record, _ := h.placer.Ledger.Find(tree, 1); !strings.Contains(record.Stopped, "is tested in ffffffffffff now") {
		t.Fatalf("the record %+v", record)
	}
}

// Queue unreadable, or a stop that failed: nothing is recorded as stopped, the pass says why, and the next pass tries
// again.
func TestNothingStopsOnAGuess(t *testing.T) {
	known := &changes{err: errors.New("GET /changes: 503")}
	h, _ := placedThenGone(t, known)
	if _, err := h.placer.PlaceOnce(); err == nil || !strings.Contains(err.Error(), "503") || len(known.stopped) != 0 {
		t.Fatalf("Queue unreadable: %v, stopped %q", err, known.stopped)
	}
	known.err, known.states, known.stopErr = nil, map[string][2]string{branch: {"withdrawn", tree}}, errors.New("ps: no such file")
	if _, err := h.placer.PlaceOnce(); err == nil || !strings.Contains(err.Error(), "stopping run") || len(h.placer.Ledger.Running()) != 1 {
		t.Fatalf("a failed stop: %v, running %d", err, len(h.placer.Ledger.Running()))
	}
	known.stopErr = nil
	h.placeOnce(t, 0)
	if len(known.stopped) != 1 || len(h.placer.Ledger.Running()) != 0 {
		t.Fatalf("tried again: stopped %q", known.stopped)
	}
}
