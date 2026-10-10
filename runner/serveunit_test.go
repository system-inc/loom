package runner

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A box serves the release its updater installed, so a job keyed on another runner is refused before anything runs:
// the judge would void whatever this runner computed for it (docs/serving.md). The runner the key names, or none,
// runs as before.
func TestAJobNamingAnotherRunnerIsRefusedAsUnfitBeforeAnythingRuns(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	job := goodTestJob()
	job.Runner = strings.Repeat("e", 64)
	result, events, _ := runUnit(t, testJobUnit(job), fixture.options(t))
	if result.Status != protocol.StatusBroken || result.OtherRunner != job.Runner ||
		!strings.Contains(errorPhases(events), "start: refused as unfit: the job's key names runner eeeeeeeeeeee, and this runner is "+selfSha256()[:12]) {
		t.Fatalf("%s, other runner %q, errors %q", result.Status, result.OtherRunner, errorPhases(events))
	}
	if fixture.exists("prepared") || result.Workspace != "" {
		t.Fatal("something ran before the refusal")
	}
	for name, runner := range map[string]string{"its own": selfSha256(), "none": ""} {
		job.Runner = runner
		result, events, _ := runUnit(t, testJobUnit(job), fixture.options(t))
		if result.OtherRunner != "" || strings.Contains(errorPhases(events), "refused as unfit") || !fixture.exists("prepared") {
			t.Fatalf("a job naming %s runner: %s, other runner %q, errors %q", name, result.Status, result.OtherRunner, errorPhases(events))
		}
	}
}

// A pool whose units name another runner holds nothing this box may run, so after one refusal serve asks for nothing
// for RunnerPause, then asks again, rather than taking and refusing every unit the pool holds.
func TestServeStandsDownAfterAUnitNamingAnotherRunner(t *testing.T) {
	job := goodTestJob()
	job.Runner = strings.Repeat("e", 64)
	queue := func(pool *testPool) {
		for _, id := range []string{"first", "second"} {
			unit := testJobUnit(job)
			unit.Unit = id
			unit.Wire = &protocol.Endpoint{Url: pool.server.URL + "/runs/r-test/events"}
			pool.queue = append(pool.queue, unit)
		}
	}
	pool := newTestPool(t)
	queue(pool)
	options := pool.serveOptions(t, time.Now().Add(time.Second+1500*time.Millisecond), time.Second, io.Discard)
	options.RunnerPause = time.Hour
	started := time.Now()
	summary, err := Serve(context.Background(), options)
	if err != nil || summary.Units != 1 || summary.Broken != 1 || summary.Stopped != "at the deadline" ||
		summary.Unfit != "its units name runner eeeeeeeeeeee, and this runner is "+selfSha256()[:12] {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
	if elapsed := time.Since(started); elapsed > 2500*time.Millisecond {
		t.Fatalf("stood down %v, past its deadline", elapsed)
	}
	pool.mutex.Lock()
	if len(pool.taken) != 1 || len(pool.queue) != 1 {
		t.Fatalf("took %v, left %d queued", pool.taken, len(pool.queue))
	}
	if events := pool.events["first"]; len(events) == 0 || events[len(events)-1].Status != protocol.StatusBroken {
		t.Fatalf("the refused unit's stream on the wire: %+v", events)
	}
	pool.mutex.Unlock()
	// A short pause asks again once it is over.
	pool = newTestPool(t)
	queue(pool)
	options = pool.serveOptions(t, time.Now().Add(time.Second+1500*time.Millisecond), time.Second, io.Discard)
	options.RunnerPause = 100 * time.Millisecond
	if summary, err := Serve(context.Background(), options); err != nil || summary.Units != 2 || summary.Unfit != "" {
		t.Fatalf("with a short pause: summary %+v, err %v", summary, err)
	}
}

// A drain is how a release restarts a box's serve (loom-serve's reload): nothing more is asked for, the unit in hand
// runs to its finish, never broken, and serve ends long before its deadline. Idle, it ends at once.
func TestADrainLetsTheUnitInHandFinishAndEnds(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("long", "sleep 1; echo long"), pool.unit("next", "true")}
	drain := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(drain) })
	options := pool.serveOptions(t, time.Now().Add(time.Hour), time.Minute, io.Discard)
	options.Drain = drain
	started := time.Now()
	summary, err := Serve(backstop(t, 15*time.Second), options)
	if err != nil || summary.Units != 1 || summary.Passed != 1 || summary.Broken != 0 || summary.Stopped != "on a drain" || time.Since(started) > 10*time.Second {
		t.Fatalf("summary %+v, err %v after %v", summary, err, time.Since(started))
	}
	pool.mutex.Lock()
	if events := pool.events["long"]; len(events) == 0 || events[len(events)-1].Status != protocol.StatusPassed || len(pool.queue) != 1 {
		t.Fatalf("the wire has %+v for the unit in hand, %d queued", events, len(pool.queue))
	}
	pool.mutex.Unlock()
	// Idle, or standing down on a full disk, a drain ends the wait at once.
	for name, free := range map[string]int64{"idle": 1 << 20, "standing down": 10} {
		pool := newTestPool(t)
		drain := make(chan struct{})
		time.AfterFunc(300*time.Millisecond, func() { close(drain) })
		options := pool.serveOptions(t, time.Now().Add(time.Hour), time.Minute, io.Discard)
		options.Drain, options.UnfitPause = drain, time.Hour
		options.freeMegabytes = func(string) (int64, error) { return free, nil }
		started := time.Now()
		if summary, err := Serve(backstop(t, 10*time.Second), options); err != nil || summary.Stopped != "on a drain" || time.Since(started) > 5*time.Second {
			t.Fatalf("%s: summary %+v, err %v after %v", name, summary, err, time.Since(started))
		}
	}
}

// backstop ends a serve that never drains, so a broken drain fails its test rather than hanging it for an hour.
func backstop(t *testing.T, after time.Duration) context.Context {
	backstopContext, cancel := context.WithTimeout(context.Background(), after)
	t.Cleanup(cancel)
	return backstopContext
}
