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
