package judge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

type listedFutures []PlannedFuture

func (futures listedFutures) Planned() ([]PlannedFuture, error) { return futures, nil }

func finishedStream(unit, status string) []protocol.Event {
	return []protocol.Event{{Unit: unit, Type: "started"}, {Unit: unit, Type: "exit", Code: code(map[string]int{"passed": 0, "failed": 1}[status])},
		{Unit: unit, Type: "output", Text: testLine(map[string]string{"passed": "pass", "failed": "fail"}[status], "TestB")},
		{Unit: unit, Type: "finished", Status: status}}
}

func TestThePullerJudgesOnlyFinishedFuturesAndRerunsByKeyParts(t *testing.T) {
	done, running := strings.Repeat("d", 40), strings.Repeat("e", 40)
	unit, reused := strings.Repeat("1", 64), strings.Repeat("2", 64)
	units := []PlannedUnitWire{{UnitKey: unit, KeyParts: json.RawMessage(`{"package":"p"}`), Decision: "run"}, {UnitKey: reused, Decision: "reuse"}}
	change := PlannedChange{Change: "chg_A", Sha: done, Base: baseTree, Owner: "system_adamic_library"}
	source := listedFutures{{Future: done, Base: baseTree, Change: change, Units: units}, {Future: running, Base: baseTree, Change: change, Units: units}}
	streams := map[string][]protocol.Event{"future-" + done + "-1": finishedStream(unit, "failed"), "future-" + running + "-1": {{Unit: unit, Type: "started"}}}
	reruns := []string{}
	queue := &StubQueue{}
	puller := Puller{
		Source: source,
		RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
		Read:   func(run string) ([]protocol.Event, error) { return streams[run], nil },
		Rerun: func(keyParts json.RawMessage, sha string) ([]protocol.Event, error) {
			reruns = append(reruns, string(keyParts)+"@"+sha[:1])
			if sha == baseTree {
				return finishedStream("job-on-base", "passed"), nil
			}
			return finishedStream("job-on-candidate", "failed"), nil
		},
		Main:  NoMainRecords{},
		Queue: queue,
		Loop:  Loop{Now: func() time.Time { return time.Date(2026, 10, 9, 23, 58, 0, 0, time.UTC) }},
	}
	judged, err := puller.PullOnce()
	if err != nil || judged != 1 {
		t.Fatalf("judged %d %v, want the one finished future", judged, err)
	}
	if len(queue.Posts[running]) != 0 || len(queue.Posts[done]) != 1 {
		t.Fatalf("posts %v, want only the finished future's", queue.Posts)
	}
	post := queue.Posts[done][0]
	if post.Decision.Status != "red" || post.Run != "future-"+done+"-1" || len(post.Decision.Kicks) != 1 {
		t.Fatalf("post %+v", post.Decision)
	}
	if strings.Join(reruns, ",") != `{"package":"p"}@d,{"package":"p"}@b` {
		t.Fatalf("reruns %v, want the unit's keyParts on the candidate then on main's base", reruns)
	}
}

func TestNoMainRecordsNeverExcusesAFailure(t *testing.T) {
	if _, found, err := (NoMainRecords{}).Latest(baseTree, strings.Repeat("1", 64)); found || err != nil {
		t.Fatal("the stand-in claimed a main record")
	}
}

func TestVoidOnePostsTheListedAttemptOnlyAndNeverReruns(t *testing.T) {
	stopped := strings.Repeat("d", 40)
	unit, never := strings.Repeat("1", 64), strings.Repeat("3", 64)
	units := []PlannedUnitWire{{UnitKey: unit, Decision: "run"}, {UnitKey: never, Decision: "run"}}
	source := listedFutures{{Future: stopped, Base: baseTree, Change: PlannedChange{Change: "chg_A"}, Units: units}}
	queue := &StubQueue{}
	puller := Puller{
		Source: source,
		RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-" + string(rune('0'+attempt)) },
		Read:   func(run string) ([]protocol.Event, error) { return finishedStream(unit, "passed"), nil },
		Rerun: func(json.RawMessage, string) ([]protocol.Event, error) {
			t.Fatal("a void reran a unit")
			return nil, nil
		},
		Main: NoMainRecords{}, Queue: queue, Loop: Loop{Now: time.Now},
	}
	if _, err := puller.VoidOne(stopped, 2, "stopped"); err == nil {
		t.Fatal("voided attempt 2 while Queue lists attempt 1")
	}
	if _, err := puller.VoidOne(strings.Repeat("e", 40), 1, "stopped"); err == nil {
		t.Fatal("voided a future Queue doesn't list")
	}
	post, err := puller.VoidOne(stopped, 1, "an operator's stop killed its loom run")
	if err != nil {
		t.Fatal(err)
	}
	if post.Run != "future-"+stopped+"-1" || post.Decision.Status != Void || len(queue.Posts[stopped]) != 1 {
		t.Fatalf("post %+v, want run -1 void, posted once", post)
	}
}

func TestTheBackstopVoidsOnlyARunQuietForFortyFiveMinutes(t *testing.T) {
	now := time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)
	at := func(minutesAgo int) string { return now.Add(-time.Duration(minutesAgo) * time.Minute).Format(time.RFC3339Nano) }
	tree := strings.Repeat("d", 40)
	done, open := strings.Repeat("1", 64), strings.Repeat("3", 64)
	units := []PlannedUnitWire{{UnitKey: done, Decision: "run"}, {UnitKey: open, Decision: "run"}}
	cases := []struct {
		name   string
		events []protocol.Event
		voided bool
	}{
		{"its last unit finished 44 minutes ago, after a start 100 minutes ago: never voided", []protocol.Event{
			{Unit: done, Type: "started", Time: at(100)}, {Unit: open, Type: "started", Time: at(99)}, {Unit: done, Type: "finished", Status: "passed", Time: at(44)}}, false},
		{"its newest event is 46 minutes old: void", []protocol.Event{
			{Unit: done, Type: "started", Time: at(100)}, {Unit: open, Type: "started", Time: at(99)}, {Unit: done, Type: "finished", Status: "passed", Time: at(46)}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			queue := &StubQueue{}
			puller := NewPuller(Puller{
				Source: listedFutures{{Future: tree, Base: baseTree, Change: PlannedChange{Change: "chg_A"}, Units: units}},
				RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-1" },
				Read:   func(string) ([]protocol.Event, error) { return c.events, nil },
				Rerun: func(json.RawMessage, string) ([]protocol.Event, error) {
					t.Fatal("the backstop placed a unit")
					return nil, nil
				},
				Main: NoMainRecords{}, Queue: queue, Loop: Loop{Now: func() time.Time { return now }}, Stale: StaleAfter,
			})
			judged, err := puller.PullOnce()
			if err != nil {
				t.Fatal(err)
			}
			if voided := len(queue.Posts[tree]) == 1; voided != c.voided || judged != map[bool]int{true: 1, false: 0}[c.voided] {
				t.Fatalf("voided %v (judged %d), want %v", voided, judged, c.voided)
			}
			if c.voided {
				post := queue.Posts[tree][0]
				if post.Decision.Status != Void || !strings.HasPrefix(post.Decision.Problems[0], "run future-"+tree+"-1 void: silent: no event for 46 min with 1 units open") {
					t.Fatalf("decision %+v", post.Decision)
				}
				if record := recordOf(t, post, open); record.Infra != InfraSilent {
					t.Fatalf("open unit %+v, want void silent", record)
				}
			}
		})
	}
}

func TestARunWithNoEventsIsAgedFromTheFirstPassThatSawIt(t *testing.T) {
	now := time.Date(2026, 10, 10, 2, 0, 0, 0, time.UTC)
	tree := strings.Repeat("d", 40)
	queue := &StubQueue{}
	puller := NewPuller(Puller{
		Source: listedFutures{{Future: tree, Change: PlannedChange{Change: "chg_A"}, Units: []PlannedUnitWire{{UnitKey: strings.Repeat("1", 64), Decision: "run"}}}},
		RunOf:  func(tree string, attempt int) string { return "future-" + tree + "-1" },
		Read:   func(string) ([]protocol.Event, error) { return nil, nil },
		Main:   NoMainRecords{}, Queue: queue, Loop: Loop{Now: func() time.Time { return now }}, Stale: StaleAfter,
	})
	for _, minutes := range []int{0, 44, 45} {
		now = time.Date(2026, 10, 10, 2, minutes, 0, 0, time.UTC)
		if _, err := puller.PullOnce(); err != nil {
			t.Fatal(err)
		}
		if posted := len(queue.Posts[tree]); posted != map[bool]int{true: 1, false: 0}[minutes == 45] {
			t.Fatalf("at minute %d, %d posts", minutes, posted)
		}
	}
	off := puller
	off.Stale = 0
	if why := off.stale("run", nil, 1); why != "" {
		t.Fatalf("a backstop turned off said %q", why)
	}
}
