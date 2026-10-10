package judge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/r2/r2test"
)

// Each mutant below must make a test here fail:
//
//	a unit that never reported in the run given a row, or a row missing its timing, exit, status or queue wait:
//	TestARowSaysWhatTheRunsEventsSayOfItsUnit
//	the timing's fields nested instead of at the row's top level: TestARowSaysWhatTheRunsEventsSayOfItsUnit
//	rows kept before the post, or for a run that wasn't posted, or under another key: TestADecidedRunsRowsAreKeptOnce
//	units/ outside the bucket's writable prefixes: TestRowsGoIntoTheBucket

type stubRows struct {
	puts map[string][]byte
	fail error
}

func (rows *stubRows) Put(key string, content []byte) error {
	if rows.fail != nil {
		return rows.fail
	}
	if rows.puts == nil {
		rows.puts = map[string][]byte{}
	}
	rows.puts[key] = content
	return nil
}

func TestARowSaysWhatTheRunsEventsSayOfItsUnit(t *testing.T) {
	ran, old, reused, silent := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64), strings.Repeat("4", 64)
	future := PlannedFuture{Future: strings.Repeat("f", 40), Units: []PlannedUnitWire{
		{UnitKey: ran, Name: "github.com/system-inc/adamic/bridge/tsgo", KeyParts: json.RawMessage(`{"kind":"test"}`), Decision: "run"},
		{UnitKey: reused, Decision: "reuse"},
		{UnitKey: silent, Decision: "run"},
		{UnitKey: old, Name: "phase lint", KeyParts: json.RawMessage(`{"kind":"phase"}`), Decision: "run"},
	}}
	events := []protocol.Event{
		{Unit: ran, Type: "started", Machine: "Cloud-7b29b4", Time: "2026-10-10T18:30:05.250Z"},
		{Unit: ran, Type: "exit", Code: code(0), WallSeconds: 41.5, UserSeconds: 300.25, SystemSeconds: 20},
		{Unit: ran, Type: "timing", Timing: &protocol.Timing{FetchSeconds: 1.5, TestSeconds: 41.5, StoreBytes: 1 << 20, PeakMegabytes: 2048, ShareCpus: 8, UnitsInHand: 3, Load: 12.5}},
		{Unit: ran, Type: "finished", Status: protocol.StatusPassed, Time: "2026-10-10T18:31:00.000Z"},
		{Unit: old, Type: "started", Machine: "Chonchon-f6285d", Time: "2026-10-10T18:29:00.000Z"},
		{Unit: old, Type: "finished", Status: protocol.StatusFailed, Time: "2026-10-10T18:29:30.000Z"},
	}
	placed := func(run string) (string, bool) { return "2026-10-10T18:28:30Z", run == "future-f-1" }
	rows := GatherRows(future, 1, "future-f-1", events, placed)
	if len(rows) != 2 || rows[0].Unit != ran || rows[1].Unit != old {
		t.Fatalf("rows %+v, want the two units that reported, in plan order", rows)
	}
	first := rows[0]
	if first.Name != "github.com/system-inc/adamic/bridge/tsgo" || first.Kind != "test" || first.Machine != "Cloud-7b29b4" || first.Status != protocol.StatusPassed ||
		first.Placed != "2026-10-10T18:28:30Z" || first.QueueSeconds != 95.25 || first.WallSeconds != 41.5 || first.UserSeconds != 300.25 ||
		first.Timing.PeakMegabytes != 2048 || first.Timing.UnitsInHand != 3 || first.Finished != "2026-10-10T18:31:00.000Z" {
		t.Fatalf("the first row: %+v", first)
	}
	// A runner that sends no timing still leaves a row, its timing's fields left off.
	if rows[1].Kind != "phase" || rows[1].Status != protocol.StatusFailed || rows[1].Timing != (protocol.Timing{}) || rows[1].QueueSeconds != 30 {
		t.Fatalf("the old runner's row: %+v", rows[1])
	}
	content, err := EncodeRows(rows)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	var top map[string]any
	if len(lines) != 2 || json.Unmarshal([]byte(lines[0]), &top) != nil || top["fetchSeconds"] != 1.5 || top["peakMegabytes"] != 2048.0 ||
		strings.Contains(lines[1], "Seconds\":0") {
		t.Fatalf("the rows as JSON lines, the timing's fields at the top:\n%s", content)
	}
	// Without the ledger, a row has no placed time and no queue wait.
	if bare := GatherRows(future, 1, "future-f-1", events, nil); bare[0].Placed != "" || bare[0].QueueSeconds != 0 {
		t.Fatalf("a row without the ledger: %+v", bare[0])
	}
}

func TestADecidedRunsRowsAreKeptOnce(t *testing.T) {
	done, running := strings.Repeat("d", 40), strings.Repeat("e", 40)
	unit := strings.Repeat("1", 64)
	units := []PlannedUnitWire{{UnitKey: unit, KeyParts: json.RawMessage(`{"package":"p"}`), Decision: "run"}}
	change := PlannedChange{Change: "chg_A", Sha: done, Base: baseTree, Owner: "system_adamic_library"}
	source := listedFutures{{Future: done, Base: baseTree, Change: change, Units: units}, {Future: running, Base: baseTree, Change: change, Units: units}}
	streams := map[string][]protocol.Event{"future-" + done + "-1": finishedStream(unit, "passed"), "future-" + running + "-1": {{Unit: unit, Type: "started"}}}
	rows := &stubRows{}
	queue := &failingQueue{}
	reported := []string{}
	puller := Puller{
		Source: source,
		RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
		Read:   func(run string) ([]protocol.Event, error) { return streams[run], nil },
		Main:   NoMainRecords{},
		Queue:  queue,
		Loop:   Loop{Blobs: &StubBlobs{}, Reused: stubReused{}, Now: func() time.Time { return time.Date(2026, 10, 10, 23, 58, 0, 0, time.UTC) }},
		Rows:   rows,
		Report: func(line string) { reported = append(reported, line) },
	}
	if judged, err := puller.PullOnce(); err != nil || judged != 1 {
		t.Fatalf("judged %d %v", judged, err)
	}
	key := "units/2026/10/10/future-" + done + "-1.jsonl"
	if len(rows.puts) != 1 || rows.puts[key] == nil || len(queue.held.Posts[done]) != 1 {
		t.Fatalf("rows kept %v, want one object at %s beside the one post", keys(rows.puts), key)
	}
	var row UnitRow
	if err := json.Unmarshal(rows.puts[key], &row); err != nil || row.Unit != unit || row.Status != protocol.StatusPassed {
		t.Fatalf("the kept row %s: %v", rows.puts[key], err)
	}
	// A post that fails keeps no rows; rows that can't be kept never fail a posted pass, and are said.
	rows.puts = nil
	queue.fail = errors.New("queue down")
	if judged, _ := puller.PullOnce(); judged != 0 || len(rows.puts) != 0 {
		t.Fatalf("a failed post judged %d and kept %v", judged, keys(rows.puts))
	}
	queue.fail = nil
	rows.fail = errors.New("bucket down")
	if judged, err := puller.PullOnce(); err != nil || judged != 1 || len(reported) != 1 || !strings.Contains(reported[0], "bucket down") {
		t.Fatalf("judged %d %v, reported %q", judged, err, reported)
	}
}

// failingQueue is a StubQueue that fails every post while fail is set.
type failingQueue struct {
	held StubQueue
	fail error
}

func (queue *failingQueue) PostVerdicts(future string, post FuturePost) error {
	if queue.fail != nil {
		return queue.fail
	}
	return queue.held.PostVerdicts(future, post)
}

func keys(puts map[string][]byte) []string {
	result := []string{}
	for key := range puts {
		result = append(result, key)
	}
	return result
}

func TestRowsGoIntoTheBucket(t *testing.T) {
	fake := r2test.New(t)
	key := RowsKey("future-f-1", time.Date(2026, 10, 10, 23, 59, 0, 0, time.FixedZone("MDT", -6*3600)))
	if key != "units/2026/10/11/future-f-1.jsonl" {
		t.Fatalf("a run decided at 23:59 MDT is kept under %s, not its UTC day's", key)
	}
	bucket := fake.Bucket()
	if err := (BucketRows{Bucket: &bucket}).Put(key, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if held, _ := fake.Object(key); string(held) != "{}\n" {
		t.Fatalf("the bucket holds %q", held)
	}
}
