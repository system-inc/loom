package runner

import (
	"bytes"
	"encoding/json"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/protocol"
)

// liveUnit is a unit as `loom top` shows it: its run and id, its first package and how many it has, its shard (the
// first package's -run, or its phase), and its deadline from now.
func liveUnit(unit protocol.Unit, phase string, now time.Time) *livestatus.Unit {
	live := &livestatus.Unit{Run: unit.Run, Unit: unit.Unit, Phase: phase, StartedAt: now}
	if unit.TimeoutSeconds > 0 {
		live.Deadline = now.Add(time.Duration(unit.TimeoutSeconds) * time.Second)
	}
	if job := unit.Test; job != nil {
		live.Packages = len(job.Packages)
		if len(job.Packages) > 0 {
			live.Package = strings.TrimPrefix(job.Packages[0].Package, protocol.AdamicModule+"/")
			live.Shard = job.Packages[0].Run
		}
		if job.Phase != "" {
			live.Shard = "phase " + job.Phase
		}
		if job.Runner != "" {
			live.Runner = shortSum(job.Runner)
		}
	} else if len(unit.Argv) > 0 {
		live.Package = strings.Join(unit.Argv, " ")
	}
	return live
}

func shortSum(sum string) string {
	if len(sum) > 12 {
		return sum[:12]
	}
	return sum
}

// phase says where the unit is on its live status, when it has one.
func (run *unitRun) phase(phase string) {
	run.live.Update(func(status *livestatus.Status) {
		if status.Unit != nil {
			status.Unit.Phase = phase
		}
	})
}

// fetched counts a blob the unit had, what it is and whether from the store or the cache, on its live status.
func (run *unitRun) fetched(what string, cached bool, bytes int64) {
	from := livestatus.FromStore
	if cached {
		from = livestatus.FromCache
	}
	run.live.Update(func(status *livestatus.Status) {
		if status.Unit != nil {
			status.Unit.AddFetch(what, from, bytes)
		}
	})
}

// testCounter reads go test -json lines as they are written and counts each top-level test's pass, fail and skip on
// the unit's live status. It never changes what passes through it and never fails a write.
type testCounter struct {
	run     *unitRun
	mutex   sync.Mutex
	partial []byte
}

// countTests is writer, with the unit's tests counted as they pass through it when the unit has a live status.
func (run *unitRun) countTests(writer io.Writer) io.Writer {
	if run.live == nil {
		return writer
	}
	return multiWriter{writer, &testCounter{run: run}}
}

type multiWriter [2]io.Writer

// Write is the first writer's, whose result it returns; the counter only reads.
func (writers multiWriter) Write(content []byte) (int, error) {
	written, err := writers[0].Write(content)
	writers[1].Write(content[:written])
	return written, err
}

var (
	actionPass = []byte(`"Action":"pass"`)
	actionFail = []byte(`"Action":"fail"`)
	actionSkip = []byte(`"Action":"skip"`)
)

func (counter *testCounter) Write(content []byte) (int, error) {
	counter.mutex.Lock()
	defer counter.mutex.Unlock()
	counter.partial = append(counter.partial, content...)
	for {
		end := bytes.IndexByte(counter.partial, '\n')
		if end < 0 {
			break
		}
		counter.count(counter.partial[:end])
		counter.partial = counter.partial[end+1:]
	}
	// A line longer than any go test -json line is only output; dropping it keeps the counter bounded.
	if len(counter.partial) > 1<<20 {
		counter.partial = counter.partial[:0]
	}
	return len(content), nil
}

func (counter *testCounter) count(line []byte) {
	if !bytes.Contains(line, actionPass) && !bytes.Contains(line, actionFail) && !bytes.Contains(line, actionSkip) {
		return
	}
	var event struct {
		Action string
		Test   string
	}
	if json.Unmarshal(line, &event) != nil || event.Test == "" || strings.Contains(event.Test, "/") {
		return
	}
	counter.run.live.Update(func(status *livestatus.Status) {
		if status.Unit == nil {
			return
		}
		switch event.Action {
		case "pass":
			status.Unit.Tests.Passed++
		case "fail":
			status.Unit.Tests.Failed++
		case "skip":
			status.Unit.Tests.Skipped++
		}
	})
}

// startLive starts serve's live status, when it keeps one, with the recent units of the serve before it: serve ends
// every hour and starts again, and the units it ran last hour are still the box's recent ones.
func startLive(options ServeOptions, started time.Time) *livestatus.Writer {
	if options.LiveStatus == "" {
		return nil
	}
	status := livestatus.Status{Kind: livestatus.KindServe, Worker: options.Worker, Pool: path.Base(strings.TrimSuffix(options.Pool, "/")), StartedAt: started}
	if previous, err := livestatus.Read(options.LiveStatus); err == nil && previous.Kind == livestatus.KindServe {
		status.Recent = previous.Recent
	}
	return livestatus.NewWriter(options.LiveStatus, status)
}

// finishLive puts a unit serve ran among its recent ones and counts it: its verdict, its time from started, and the bytes
// it fetched from the store, which the runner that ran it said on its own status (unitPath) when it ran alone and that
// status is this unit's. next is the unit in hand serve shows now, nil when none.
func finishLive(live *livestatus.Writer, unit protocol.Unit, verdict string, started time.Time, next *livestatus.Unit, alone bool, unitPath string) {
	if live == nil {
		return
	}
	if verdict != protocol.StatusPassed && verdict != protocol.StatusFailed {
		verdict = protocol.StatusBroken
	}
	var fetched int64
	if unitPath != "" && alone {
		if ran, err := livestatus.Read(unitPath); err == nil && ran.Unit != nil && ran.Unit.Run == unit.Run && ran.Unit.Unit == unit.Unit {
			fetched = ran.Unit.Fetched()
		}
	}
	now := time.Now()
	live.Update(func(status *livestatus.Status) {
		status.Unit = next
		status.AddRecent(livestatus.Recent{Run: unit.Run, Unit: unit.Unit, Verdict: verdict, FinishedAt: now, Seconds: now.Sub(started).Seconds(), Fetched: fetched})
		status.Totals.Units++
		status.Totals.Fetched += fetched
		switch verdict {
		case protocol.StatusPassed:
			status.Totals.Passed++
		case protocol.StatusFailed:
			status.Totals.Failed++
		default:
			status.Totals.Broken++
		}
	})
}
