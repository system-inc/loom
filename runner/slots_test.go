package runner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A box serve running several units at once (#ef2rgaq). Each mutant below must make a test here fail:
//
//	the shares in hand not held to the machine's threads, or to its memory: TestServeRunsUnitsAtOnceWithinTheMachine
//	a declared share ignored, or not said on the started event: TestADeclaredShareIsHeldAndSaid
//	another unit asked for while the machine is past its busy target: TestServeAsksForNoMoreWhileTheMachineIsBusy
//	a unit that can't run beside others started beside them: TestAUnitThatCantRunBesideOthersRunsAlone
//	a trim run while a unit holds the root: TestATrimNeverRunsUnderALiveUnit
//	TMPDIR or the cycle ledger's output left the instance's, shared by units at once:
//	TestEachTestUnitHasATemporaryDirectoryAndLedgerOutputOfItsOwn

// span is when a unit ran, from its started and finished events on the wire.
type span struct {
	unit            string
	started, ended  time.Time
	cpus, megabytes int
}

// spans reads each unit's span from the pool's events.
func (pool *testPool) spans(t *testing.T, ids ...string) []span {
	t.Helper()
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	spans := []span{}
	for _, id := range ids {
		events := pool.events[id]
		if len(events) < 2 || events[0].Type != "started" || events[len(events)-1].Type != "finished" {
			t.Fatalf("unit %s's stream: %+v", id, events)
		}
		started, err := time.Parse(timeLayout, events[0].Time)
		if err != nil {
			t.Fatal(err)
		}
		ended, err := time.Parse(timeLayout, events[len(events)-1].Time)
		if err != nil {
			t.Fatal(err)
		}
		spans = append(spans, span{unit: id, started: started, ended: ended, cpus: events[0].Cpus, megabytes: events[0].MemoryMegabytes})
	}
	return spans
}

// mostAtOnce is the most spans that overlapped at any moment.
func mostAtOnce(spans []span) int {
	type edge struct {
		at    time.Time
		delta int
	}
	edges := []edge{}
	for _, span := range spans {
		edges = append(edges, edge{span.started, 1}, edge{span.ended, -1})
	}
	// An end at the same moment as a start comes first: back to back isn't at once.
	slices.SortFunc(edges, func(left, right edge) int {
		if compared := left.at.Compare(right.at); compared != 0 {
			return compared
		}
		return left.delta - right.delta
	})
	most, now := 0, 0
	for _, edge := range edges {
		now += edge.delta
		most = max(most, now)
	}
	return most
}

// slotsOptions are a serve of several units at once on a planted machine, idle unless busy says otherwise, where
// every unit may run beside others unless alongside says otherwise.
func (pool *testPool) slotsOptions(t *testing.T, cpus, megabytes, units, unitCpus, unitMegabytes int) ServeOptions {
	options := pool.serveOptions(t, time.Now().Add(time.Minute), time.Second, io.Discard)
	options.Units, options.UnitCpus, options.UnitMemoryMegabytes = units, unitCpus, unitMegabytes
	options.machine = &machine{name: "box", cpus: cpus, memoryMegabytes: megabytes}
	options.busy = func() (float64, bool) { return 0.1, true }
	options.alongside = func(protocol.Unit) bool { return true }
	options.Drain = drainWhenEmpty(pool)
	return options
}

// drainWhenEmpty drains a serve once its pool's queue is empty, so a test serve ends when its units do.
func drainWhenEmpty(pool *testPool) chan struct{} {
	drain := make(chan struct{})
	go func() {
		for {
			pool.mutex.Lock()
			empty := len(pool.queue) == 0
			pool.mutex.Unlock()
			if empty {
				close(drain)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	return drain
}

func TestServeRunsUnitsAtOnceWithinTheMachine(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	for _, held := range []struct {
		name                      string
		cpus, megabytes, unitCpus int
		unitMegabytes, wantAtOnce int
	}{
		// 16 threads hold four units of 4 cpus; the memory holds all eight.
		{"threads", 16, 1 << 20, 4, 1024, 4},
		// 40000 MB holds two units of 16384 MB under its nine tenths; the threads hold all eight.
		{"memory", 64, 40000, 1, 16384, 2},
	} {
		t.Run(held.name, func(t *testing.T) {
			pool := newTestPool(t)
			for _, id := range ids {
				pool.queue = append(pool.queue, pool.unit(id, "sleep 1"))
			}
			started := time.Now()
			summary, err := Serve(context.Background(), pool.slotsOptions(t, held.cpus, held.megabytes, 8, held.unitCpus, held.unitMegabytes))
			if err != nil || summary.Units != 8 || summary.Passed != 8 {
				t.Fatalf("summary %+v, %v", summary, err)
			}
			if most := mostAtOnce(pool.spans(t, ids...)); most != held.wantAtOnce {
				t.Fatalf("%d units at once, want %d", most, held.wantAtOnce)
			}
			// Eight one-second units, wantAtOnce at a time, take well under eight seconds.
			if elapsed := time.Since(started); elapsed > time.Duration(8/held.wantAtOnce+2)*time.Second {
				t.Fatalf("eight units took %v", elapsed)
			}
		})
	}
}

func TestADeclaredShareIsHeldAndSaid(t *testing.T) {
	pool := newTestPool(t)
	big := pool.unit("big", "sleep 1")
	big.Resources = protocol.Resources{Cpus: 3, MemoryMegabytes: 30000}
	pool.queue = []protocol.Unit{big, pool.unit("a", "sleep 1"), pool.unit("b", "sleep 1")}
	// 36000 MB of shares: the big one leaves no room for a default one beside it, and the two default ones fit together.
	summary, err := Serve(context.Background(), pool.slotsOptions(t, 64, 40000, 8, 1, 16384))
	if err != nil || summary.Passed != 3 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	spans := pool.spans(t, "big", "a", "b")
	if mostAtOnce(spans[:2]) != 1 || mostAtOnce(spans[1:]) != 2 {
		t.Fatalf("the big unit ran beside another, or the two small ones apart: %+v", spans)
	}
	if spans[0].cpus != 3 || spans[0].megabytes != 30000 || spans[1].cpus != 1 || spans[1].megabytes != 16384 {
		t.Fatalf("started events say %d cpus %d MB and %d cpus %d MB", spans[0].cpus, spans[0].megabytes, spans[1].cpus, spans[1].megabytes)
	}
}

func TestServeAsksForNoMoreWhileTheMachineIsBusy(t *testing.T) {
	pool := newTestPool(t)
	ids := []string{"a", "b", "c"}
	for _, id := range ids {
		pool.queue = append(pool.queue, pool.unit(id, "sleep 1"))
	}
	options := pool.slotsOptions(t, 64, 1<<20, 8, 1, 1024)
	options.busy = func() (float64, bool) { return 0.85, true }
	summary, err := Serve(context.Background(), options)
	if err != nil || summary.Passed != 3 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	if most := mostAtOnce(pool.spans(t, ids...)); most != 1 {
		t.Fatalf("%d units at once on a machine past its busy target", most)
	}
}

func TestAUnitThatCantRunBesideOthersRunsAlone(t *testing.T) {
	pool := newTestPool(t)
	ids := []string{"a", "b", "alone", "c", "d"}
	for _, id := range ids {
		pool.queue = append(pool.queue, pool.unit(id, "sleep 1"))
	}
	options := pool.slotsOptions(t, 64, 1<<20, 8, 1, 1024)
	options.alongside = func(unit protocol.Unit) bool { return unit.Unit != "alone" }
	summary, err := Serve(context.Background(), options)
	if err != nil || summary.Passed != 5 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	spans := pool.spans(t, ids...)
	for index, other := range spans {
		if index != 2 && mostAtOnce([]span{spans[2], other}) != 1 {
			t.Fatalf("the unit that runs alone ran beside %s: %+v", other.unit, spans)
		}
	}
	if whole := describeMachine().cpus; spans[2].cpus != whole {
		t.Fatalf("the unit that runs alone said %d cpus, not the whole machine's %d", spans[2].cpus, whole)
	}
	if mostAtOnce(spans) < 2 {
		t.Fatalf("no units ran at once: %+v", spans)
	}
}

func TestATrimNeverRunsUnderALiveUnit(t *testing.T) {
	root := t.TempDir()
	trims := 0
	trim := func() { trims++ }
	// The first unit finds the root free and trims; the second, beside it, doesn't.
	first, err := holdRoot(context.Background(), root, false, trim)
	if err != nil || trims != 1 {
		t.Fatalf("the first unit: %v, %d trims", err, trims)
	}
	second, err := holdRoot(context.Background(), root, false, trim)
	if err != nil || trims != 1 {
		t.Fatalf("a unit beside another: %v, %d trims", err, trims)
	}
	if trimAlone(root, trim) || trims != 1 {
		t.Fatalf("serve's trim ran under live units: %d trims", trims)
	}
	// A checkout unit holds the root alone, so it waits for both.
	waiting, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := holdRoot(waiting, root, true, nil); err == nil {
		t.Fatal("a checkout unit held the root beside two prebuilt units")
	}
	first()
	second()
	if !trimAlone(root, trim) || trims != 2 {
		t.Fatalf("serve's trim didn't run on a free root: %d trims", trims)
	}
	alone, err := holdRoot(context.Background(), root, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer alone()
	if trimAlone(root, trim) || trims != 2 {
		t.Fatalf("serve's trim ran under a checkout unit: %d trims", trims)
	}
}

func TestEachTestUnitHasATemporaryDirectoryAndLedgerOutputOfItsOwn(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	shared := filepath.Join(fixture.directory, "shared")
	os.MkdirAll(shared, 0o755)
	seen := filepath.Join(fixture.directory, "seen")
	os.MkdirAll(seen, 0o755)
	bin := filepath.Join(fixture.directory, "bin")
	os.WriteFile(filepath.Join(bin, "go"), []byte(`#!/bin/bash
printf '%s %s\n' "${TMPDIR}" "${ADAMIC_CYCLE_LEDGER_OUTPUT}" > "`+seen+`/go-$$"
case "$*" in *-exec*) exit 0 ;; esac
printf '{"Action":"pass","Package":"%s","Test":"TestA"}\n' "${!#}"
`), 0o755)
	prepareScript = []byte(`#!/bin/bash
mkdir -p "$1"
printf 'PATH=%s\0HOME=%s\0TMPDIR=%s\0ADAMIC_CYCLE_LEDGER_OUTPUT=%s\0' "` + bin + `:/usr/bin:/bin" "${HOME}" "` + shared + `" "` + shared + `/cycle-ledger-output.json" > "$5"
`)
	owned := map[string]bool{}
	for range 2 {
		before, _ := os.ReadDir(seen)
		result, events, _ := runUnit(t, testJobUnit(goodTestJob()), fixture.options(t))
		if result.Status != protocol.StatusPassed {
			t.Fatalf("the unit %s; errors %q", result.Status, errorPhases(events))
		}
		entries, _ := os.ReadDir(seen)
		for _, entry := range entries[len(before):] {
			line, _ := os.ReadFile(filepath.Join(seen, entry.Name()))
			fields := strings.Fields(string(line))
			if len(fields) != 2 || !strings.Contains(fields[0], "loom-unit-") || !strings.Contains(fields[1], "loom-unit-") || strings.HasPrefix(fields[1], shared) {
				t.Fatalf("go ran with TMPDIR and the ledger output %q: not the unit's own", line)
			}
			owned[filepath.Dir(fields[0])] = true
		}
	}
	if len(owned) != 2 {
		t.Fatalf("two units shared a temporary directory: %v", owned)
	}
}
