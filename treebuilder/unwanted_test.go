package treebuilder

import (
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/judge"
)

// A build is stopped once nothing wants its tree (#drrnnkh's question, asked by the tree builder). Each mutant below
// must make a test here fail:
//
//	no build ever stopped: TestABuildWhoseFutureIsWithdrawnIsStoppedAndNeverBuiltAgain
//	a stop recorded failed, or backed off: TestABuildWhoseFutureIsWithdrawnIsStoppedAndNeverBuiltAgain
//	a listing that can't be read stops a build: TestABuildIsNeverStoppedOnAListingItCantRead
//	Judge's requests left out of what's wanted: TestABuildJudgeAsksForIsNeverStopped
//	a still-listed build stopped: TestABuildIsNeverStoppedOnAListingItCantRead
//	an adopted build never asked about: TestAnAdoptedBuildNothingWantsIsStopped

// changingFutures is Queue's listing as it moves: what it lists now, or an error.
type changingFutures struct {
	mutex   sync.Mutex
	futures []judge.PlannedFuture
	err     error
}

func (source *changingFutures) Planned() ([]judge.PlannedFuture, error) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	return source.futures, source.err
}

func (source *changingFutures) set(futures []judge.PlannedFuture, err error) {
	source.mutex.Lock()
	defer source.mutex.Unlock()
	source.futures, source.err = futures, err
}

// stoppable makes the harness's builds run until their child is killed, or until long passes and they build: once
// running, change is called, as the listing moves under a build. killed says whether a kill came.
func stoppable(h *harness, long time.Duration, change func()) *bool {
	killed, kill := false, make(chan struct{})
	var once sync.Once
	h.builder.Kill = func(pid int) {
		once.Do(func() {
			killed = true
			close(kill)
		})
	}
	h.builder.Sleep = func(time.Duration) { time.Sleep(2 * time.Millisecond) }
	h.builder.Build = func(want Want, running func(pid int) error) (*builder.TreePhases, error) {
		h.builds = append(h.builds, want)
		if err := running(4242); err != nil {
			return nil, err
		}
		change()
		select {
		case <-kill:
			return nil, errors.New("build-tree: signal: killed")
		case <-time.After(long):
			h.indexed[want.Tree] = true
			return nil, nil
		}
	}
	return &killed
}

func TestABuildWhoseFutureIsWithdrawnIsStoppedAndNeverBuiltAgain(t *testing.T) {
	source := &changingFutures{futures: []judge.PlannedFuture{future("1", unit(t, "test", "run", keyA))}}
	ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
	h := newHarness(t, source, ledger)
	// Withdrawn mid-build: Queue lists another future, not this one.
	killed := stoppable(h, 10*time.Second, func() { source.set([]judge.PlannedFuture{future("2", unit(t, "test", "reuse", keyB))}, nil) })
	h.buildOnce(t, true)
	newest, _ := ledger.Newest(keyA)
	if !*killed || newest.Event != Stopped || !strings.Contains(newest.Cause, "no future Queue lists planned runs it") || newest.Retry != "" {
		t.Fatalf("killed %v, tree a's newest record %+v", *killed, newest)
	}
	// Never built again while nothing lists it.
	h.buildOnce(t, false)
	if len(h.builds) != 1 {
		t.Fatalf("builds %v after the stop", h.builds)
	}
}

func TestABuildIsNeverStoppedOnAListingItCantRead(t *testing.T) {
	for name, change := range map[string]func(source *changingFutures){
		"unreadable": func(source *changingFutures) { source.set(nil, errors.New("Queue: 502")) },
		"listed":     func(source *changingFutures) {},
	} {
		source := &changingFutures{futures: []judge.PlannedFuture{future("1", unit(t, "test", "run", keyA))}}
		ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
		h := newHarness(t, source, ledger)
		killed := stoppable(h, 100*time.Millisecond, func() { change(source) })
		h.buildOnce(t, true)
		if newest, _ := ledger.Newest(keyA); *killed || newest.Event != Built {
			t.Errorf("%s: killed %v, tree a's newest record %+v", name, *killed, newest)
		}
	}
}

func TestABuildJudgeAsksForIsNeverStopped(t *testing.T) {
	source := &changingFutures{}
	ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
	h := newHarness(t, source, ledger)
	h.builder.Requests = func() ([]Request, error) {
		return []Request{{Tree: keyC, Commit: strings.Repeat("3", 40), Go: "go1.27.1"}}, nil
	}
	killed := stoppable(h, 100*time.Millisecond, func() {})
	h.buildOnce(t, true)
	if newest, _ := ledger.Newest(keyC); *killed || newest.Event != Built {
		t.Fatalf("a base tree Judge asks for, listed by no future: killed %v, newest %+v", *killed, newest)
	}
}

func TestAnAdoptedBuildNothingWantsIsStopped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trees.jsonl")
	ledger := openLedger(t, path, time.Now())
	source := &changingFutures{futures: []judge.PlannedFuture{future("2", unit(t, "test", "run", keyB))}}
	h := newHarness(t, source, ledger)
	if err := ledger.Append(Record{Tree: keyA, Future: strings.Repeat("1", 40), At: h.now.Format(time.RFC3339), Event: Running, Pid: 4242}); err != nil {
		t.Fatal(err)
	}
	alive := true
	h.builder.Alive = func(pid int, tree string) bool { return alive }
	h.builder.Kill = func(pid int) { alive = false }
	h.builder.Bound = time.Hour
	h.builder.Sleep = func(time.Duration) { h.now = h.now.Add(10 * time.Second) }
	h.buildOnce(t, true)
	if newest, _ := ledger.Newest(keyA); alive || newest.Event != Stopped || !strings.Contains(newest.Cause, "stopped mid-build: no future Queue lists") {
		t.Fatalf("alive %v, the adopted tree's newest record %+v", alive, newest)
	}
}
