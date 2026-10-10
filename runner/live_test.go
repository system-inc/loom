package runner

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/protocol"
)

// A prebuilt unit's live status says what it fetched, of what kind and from where, and its top-level tests as they
// pass, fail and skip, and ends finished with its verdict; a second unit of the same tree had its blobs from the cache,
// so it fetched only its index from the store. Mutants: a subtest counted; a blob from the cache counted as the store's;
// no verdict at the finish.
func TestARunnersLiveStatusSaysWhatItFetchedAndItsTests(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	options := fixture.options(t)
	options.LiveStatus = livestatus.UnitPath(options.Root)
	result, events, _ := runUnit(t, fixture.unit("^(TestA|TestGate|TestFail)$"), options)
	if result.Status != protocol.StatusFailed {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	status, err := livestatus.Read(options.LiveStatus)
	if err != nil {
		t.Fatal(err)
	}
	unit := status.Unit
	switch {
	case status.Kind != livestatus.KindUnit || unit == nil:
		t.Fatalf("%+v", status)
	case unit.Run != "r-test" || unit.Unit != "unit" || unit.Package != "internal/lower" || unit.Shard != "^(TestA|TestGate|TestFail)$" || unit.Packages != 1:
		t.Fatalf("the unit: %+v", unit)
	case unit.Phase != livestatus.PhaseFinished || unit.Verdict != protocol.StatusFailed:
		t.Fatalf("finished as %q, %q", unit.Phase, unit.Verdict)
	case unit.Tests != (livestatus.Tests{Passed: 2, Failed: 1}):
		t.Fatalf("tests %+v; want TestA and TestGate passed, TestFail failed, and TestGate/Nested not counted", unit.Tests)
	case unit.Deadline.Sub(unit.StartedAt) != 60*time.Second:
		t.Fatalf("deadline %v after its start", unit.Deadline.Sub(unit.StartedAt))
	}
	fetches := map[string]livestatus.Fetch{}
	for _, fetch := range unit.Fetches {
		fetches[fetch.What+" "+fetch.From] = fetch
	}
	for _, want := range []string{"index store", "binaries store", "products store", "chunks store", "modules store"} {
		if fetches[want].Count == 0 || fetches[want].Bytes == 0 {
			t.Errorf("no %s in %+v", want, unit.Fetches)
		}
	}
	if fetches["chunks store"].Count != 3 || len(fetches) != 5 {
		t.Errorf("fetches %+v", unit.Fetches)
	}
	result, events, _ = runUnit(t, fixture.unit("^TestA$"), options)
	if result.Status != protocol.StatusPassed {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	status, _ = livestatus.Read(options.LiveStatus)
	index := int64(0)
	for _, fetch := range status.Unit.Fetches {
		if fetch.From == livestatus.FromStore {
			index += fetch.Bytes
			if fetch.What != "index" {
				t.Errorf("the second unit had %s from the store", fetch.What)
			}
		}
	}
	if status.Unit.Fetched() != index || index == 0 || status.Unit.Tests != (livestatus.Tests{Passed: 1}) || status.Unit.Verdict != protocol.StatusPassed {
		t.Fatalf("the second unit: %+v", status.Unit)
	}
}

// Serve's live status holds the unit in hand while it runs, its runner's own status the same unit, and afterwards the
// recent units newest first with their verdicts and times, the totals, and why serve stopped; the next serve starts
// from the recent units the last one left. Mutants: no unit in hand while it runs; the recent units not carried over.
func TestServesLiveStatusHoldsTheUnitInHandAndTheRecentOnes(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("first", "sleep 2"), pool.unit("second", "exit 3")}
	root := t.TempDir()
	options := pool.serveOptions(t, time.Now().Add(4500*time.Millisecond), time.Second, nil)
	options.LiveStatus = livestatus.ServePath(root)
	options.Unit.LiveStatus = livestatus.UnitPath(root)
	served := make(chan ServeSummary)
	go func() {
		summary, _ := Serve(context.Background(), options)
		served <- summary
	}()
	var inHand, ran livestatus.Status
	for wait := time.Now().Add(2 * time.Second); time.Now().Before(wait); time.Sleep(50 * time.Millisecond) {
		inHand, _ = livestatus.Read(options.LiveStatus)
		ran, _ = livestatus.Read(options.Unit.LiveStatus)
		if inHand.Unit != nil && ran.Unit != nil && ran.Unit.Unit == "first" {
			break
		}
	}
	if inHand.Unit == nil || inHand.Unit.Unit != "first" || inHand.Unit.Phase != livestatus.PhaseOnRunner || inHand.Worker != "codex-1" || inHand.Pool != "codex" {
		t.Fatalf("serve while the first unit runs: %+v %+v", inHand, inHand.Unit)
	}
	if ran.Unit == nil || ran.Unit.Unit != "first" || ran.Unit.Package != "sh -c sleep 2" {
		t.Fatalf("the runner while the first unit runs: %+v", ran.Unit)
	}
	summary := <-served
	status, err := livestatus.Read(options.LiveStatus)
	if err != nil {
		t.Fatal(err)
	}
	if status.Unit != nil || status.Stopped != summary.Stopped || status.Totals != (livestatus.Totals{Units: 2, Passed: 1, Failed: 1}) || status.AskedAt.IsZero() {
		t.Fatalf("after serving: %+v", status)
	}
	if len(status.Recent) != 2 || status.Recent[0].Unit != "second" || status.Recent[0].Verdict != protocol.StatusFailed ||
		status.Recent[1].Unit != "first" || status.Recent[1].Verdict != protocol.StatusPassed || status.Recent[1].Seconds < 2 {
		t.Fatalf("recent %+v", status.Recent)
	}
	options.Deadline = time.Now().Add(1200 * time.Millisecond)
	if _, err := Serve(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	if again, _ := livestatus.Read(options.LiveStatus); len(again.Recent) != 2 || again.Totals.Units != 0 || again.StartedAt.Equal(status.StartedAt) {
		t.Fatalf("the next serve: %+v", again)
	}
	if matches, _ := filepath.Glob(filepath.Join(root, livestatus.Directory, ".partial-*")); len(matches) != 0 {
		t.Fatalf("partial files left: %v", matches)
	}
}
