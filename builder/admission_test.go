package builder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/system-inc/loom/planner"
)

// run runs count jobs of work through paced under pace and gauge at the target 0.8, each holding for hold, and returns
// how many ran at once at most, how many readings the gauge had given when that many first ran, and how many ran in
// all. now is how many are running, for a gauge that reads what the jobs fill. It fails the test, rather than hang it,
// when paced never ends.
func run(t *testing.T, count int, pace admission, gauge func(now *atomic.Int64) Gauge, hold time.Duration) (peak, readings, ran int64) {
	t.Helper()
	var now, most, read, readAtMost, done atomic.Int64
	var counted Gauge
	if gauge != nil {
		inner := gauge(&now)
		counted = func() (float64, float64, bool) {
			busy, available, ok := inner()
			read.Add(1)
			return busy, available, ok
		}
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		paced(count, pace, counted, 0.8, nil, func(int) {
			value := now.Add(1)
			for {
				old := most.Load()
				if value <= old {
					break
				}
				if most.CompareAndSwap(old, value) {
					readAtMost.Store(read.Load())
					break
				}
			}
			time.Sleep(hold)
			now.Add(-1)
			done.Add(1)
		}, nil)
	}()
	select {
	case <-finished:
	case <-time.After(20 * time.Second):
		t.Fatalf("paced never finished: %d of %d ran", done.Load(), count)
	}
	return most.Load(), readAtMost.Load(), done.Load()
}

// steady is a gauge that reads the same whatever runs, each reading taking took.
func steady(busy, available float64, ok bool, took time.Duration) func(*atomic.Int64) Gauge {
	return func(*atomic.Int64) Gauge {
		return func() (float64, float64, bool) {
			time.Sleep(took)
			return busy, available, ok
		}
	}
}

// A hit-heavy tree's product tests barely move the machine, so they ramp past the old fixed ceiling of jobs() to
// ProductJobs, the headroom's worth of starts to a reading. Mutants: paced's reading under the target bounded by
// pace.blind, not pace.ceiling (8 at once); productAdmission's ceiling build.jobs() (8 at once); startsOn returning 1
// (a reading for every start on the way up).
func TestAHitHeavyTreesProductTestsRampPastTheOldCeiling(t *testing.T) {
	build := TreeBuild{Compile: 60, Jobs: 8}
	peak, readings, ran := run(t, 120, build.productAdmission(), steady(0.1, 0.9, true, 5*time.Millisecond), 400*time.Millisecond)
	if ran != 120 || peak != 60 {
		t.Fatalf("a calm machine ran %d at once (%d in all), not its ceiling of 60 (the old fixed ceiling was %d)", peak, ran, build.jobs())
	}
	if readings > 20 {
		t.Fatalf("a calm machine took %d readings to reach 60 at once", readings)
	}
}

// A build-heavy tree's product tests each fill their share of compile, so the machine reads what is running (over a
// window, as ProcGauge reads it): a reading lets start only what the headroom under the target holds, and the build
// holds at the target, the first start at or over it and none past. Mutants: starts counting the headroom to all of
// the machine (1 - busy), one past it; starts counting each start as one thread, not its share.
func TestABuildHeavyTreesProductTestsHoldAtTheTarget(t *testing.T) {
	build := TreeBuild{Compile: 60, Jobs: 8}
	share := float64(build.share())
	filling := func(now *atomic.Int64) Gauge {
		return func() (float64, float64, bool) {
			// What paced started a moment ago is running by the time the window opens.
			time.Sleep(2 * time.Millisecond)
			first := now.Load()
			time.Sleep(3 * time.Millisecond)
			return float64(min(first, now.Load())) * share / 60, 0.9, true
		}
	}
	// 7 shares of 7 threads are 49 of 60, the first at or over 0.8.
	if peak, _, ran := run(t, 40, build.productAdmission(), filling, 150*time.Millisecond); ran != 40 || peak != 7 {
		t.Fatalf("a machine filled by what runs ran %d at once (%d in all), not 7, the first at the target", peak, ran)
	}
}

// ProductJobs bounds the product tests at once however low the gauge reads, so a gauge that lags behind a burst of
// starts (here one that reads idle whatever runs) can't fork more; zero means compile. Mutants: paced's room not
// bounded by pace.ceiling; productJobs ignoring ProductJobs; productJobs defaulting past compile.
func TestTheProductCeilingHoldsHoweverLowTheGaugeReads(t *testing.T) {
	build := TreeBuild{Compile: 60, Jobs: 8, ProductJobs: 20}
	if peak, _, ran := run(t, 100, build.productAdmission(), steady(0, 0.9, true, time.Millisecond), 30*time.Millisecond); ran != 100 || peak != 20 {
		t.Fatalf("a gauge reading idle ran %d at once (%d in all), past or short of the ceiling of 20", peak, ran)
	}
	for _, bound := range []struct {
		build TreeBuild
		jobs  int
	}{{TreeBuild{Compile: 60}, 60}, {TreeBuild{Compile: 60, ProductJobs: 20}, 20}, {TreeBuild{}, max(1, runtime.NumCPU()-4)}} {
		if got := bound.build.productJobs(); got != bound.jobs {
			t.Errorf("compile %d, product jobs %d: a ceiling of %d, want %d", bound.build.Compile, bound.build.ProductJobs, got, bound.jobs)
		}
	}
}

// One product test always runs: a machine over its target, or short of memory, runs them one at a time and every one
// of them, and a gauge that can't read, or none, falls back to the fixed jobs() at a time. Mutants: paced waiting for
// room with none running, which never starts one on a hot machine; paced's unread reading taking pace.ceiling (30 at
// once).
func TestOneProductTestAlwaysRunsAndABlindGaugeKeepsTheOldCeiling(t *testing.T) {
	build := TreeBuild{Compile: 60, Jobs: 8}
	for _, machine := range []struct {
		name  string
		gauge func(*atomic.Int64) Gauge
		peak  int64
	}{{"over its target", steady(0.95, 0.9, true, time.Millisecond), 1}, {"short of memory", steady(0.1, 0.1, true, time.Millisecond), 1},
		{"unread", steady(0, 0, false, 0), 8}, {"with no gauge", nil, 8}} {
		if peak, _, ran := run(t, 30, build.productAdmission(), machine.gauge, 20*time.Millisecond); ran != 30 || peak != machine.peak {
			t.Errorf("a machine %s ran %d at once (%d of 30 in all), want %d", machine.name, peak, ran, machine.peak)
		}
	}
}

// A reading at or over the target lets none start, not even to replace a job that ended, so once the machine reads
// hot one runs at a time however much room the calm reading before it left. Mutant: paced's reading over the target
// keeping the last reading's room (two at a time on a hot machine).
func TestAHotReadingStopsReplacingJobsThatEnd(t *testing.T) {
	var readings atomic.Int64
	gauge := func() (float64, float64, bool) {
		time.Sleep(time.Millisecond)
		if readings.Add(1) == 1 {
			return 0, 0.9, true
		}
		return 1, 0.9, true
	}
	var now, late atomic.Int64
	paced(8, admission{ceiling: 2, blind: 2, starts: func(float64, float64) int { return 100 }}, gauge, 0.8, nil, func(index int) {
		value := now.Add(1)
		if index >= 2 && value > late.Load() {
			late.Store(value)
		}
		time.Sleep(50 * time.Millisecond)
		now.Add(-1)
	}, nil)
	if late.Load() != 1 {
		t.Fatalf("after the machine went hot, %d ran at once on %d readings", late.Load(), readings.Load())
	}
}

// A job that ends is replaced at once on the last reading's room, so a slow reading (ProcGauge's half second) doesn't
// hold the build back to a start per reading. Mutant: paced not waking when a job ends (each wave of starts waits for
// the next reading).
func TestAJobThatEndsIsReplacedWithoutAReading(t *testing.T) {
	pace := admission{ceiling: 3, blind: 3, starts: func(float64, float64) int { return 100 }}
	var readings atomic.Int64
	started := time.Now()
	paced(60, pace, func() (float64, float64, bool) {
		readings.Add(1)
		time.Sleep(100 * time.Millisecond)
		return 0.1, 0.9, true
	}, 0.8, nil, func(int) { time.Sleep(10 * time.Millisecond) }, nil)
	if readings.Load() > 6 {
		t.Fatalf("60 jobs of 10 ms, 3 at a time, took %d readings of 100 ms and %v", readings.Load(), time.Since(started))
	}
}

// paced reads the disk before each start as admitted does: under the floor, running jobs finish and every job left
// is refused with the reason, none of them run. Mutant: paced starting without the disk's reading.
func TestPacedRefusesForTheDiskAsAdmittedDoes(t *testing.T) {
	admissionPoll = time.Millisecond
	t.Cleanup(func() { admissionPoll = time.Second })
	short := errors.New("Go's build cache has 12.0 GB free, under its 200 GB floor")
	var ran, refused atomic.Int64
	paced(5, TreeBuild{Compile: 60, Jobs: 8}.productAdmission(), steady(0.1, 0.9, true, time.Millisecond)(nil), 0.8, func() error { return short },
		func(int) { ran.Add(1) }, func(index int, err error) {
			if errors.Is(err, short) {
				refused.Add(1)
			}
		})
	if ran.Load() != 0 || refused.Load() != 5 {
		t.Fatalf("a disk under the floor: %d ran, %d refused", ran.Load(), refused.Load())
	}
}

// Products itself is paced: with --jobs 1, its product tests' go processes still overlap on a calm machine. Mutant:
// Products admitted by the old fixed jobs() (one at a time).
func TestProductsRunsPastJobsOnACalmMachine(t *testing.T) {
	marks := t.TempDir()
	files := map[string]string{"go.mod": "module example.com/paced\n\ngo 1.22\n", "p/p.go": "package p\n"}
	test := "package p\n\nimport (\n\t\"fmt\"\n\t\"os\"\n\t\"path/filepath\"\n\t\"testing\"\n\t\"time\"\n)\n\n"
	for number := range 4 {
		test += fmt.Sprintf("func TestProduct_%d(t *testing.T) {\n\tstarted := time.Now().UnixNano()\n\ttime.Sleep(2 * time.Second)\n\tos.WriteFile(filepath.Join(os.Getenv(\"LOOM_PACED_MARKS\"), \"%d\"), []byte(fmt.Sprint(started, \" \", time.Now().UnixNano())), 0o644)\n}\n\n", number, number)
	}
	files["p/p_test.go"] = test
	tree := gitTree(t, files)
	build := TreeBuild{Tree: tree, Cache: t.TempDir(), Environment: append(planner.GateEnvironmentList(), "LOOM_PACED_MARKS="+marks),
		Jobs: 1, Compile: 8, Gauge: reading(0.1, 0.9, true)}
	tests := []planner.ProductTest{}
	for number := range 4 {
		tests = append(tests, planner.ProductTest{Package: "example.com/paced/p", Directory: "p", Test: fmt.Sprintf("TestProduct_%d", number)})
	}
	if _, failed := build.Products(tests, t.TempDir()); len(failed) != 0 {
		t.Fatalf("product tests failed: %v", failed)
	}
	type span struct{ started, ended int64 }
	spans := []span{}
	for number := range 4 {
		content, err := os.ReadFile(filepath.Join(marks, strconv.Itoa(number)))
		if err != nil {
			t.Fatal(err)
		}
		fields := strings.Fields(string(content))
		started, _ := strconv.ParseInt(fields[0], 10, 64)
		ended, _ := strconv.ParseInt(fields[1], 10, 64)
		spans = append(spans, span{started, ended})
	}
	overlapping := slices.ContainsFunc(spans, func(one span) bool {
		return slices.ContainsFunc(spans, func(other span) bool { return one != other && one.started < other.ended && other.started < one.ended })
	})
	if build.jobs() != 1 || !overlapping {
		t.Fatalf("with jobs() %d, product tests never overlapped: %v", build.jobs(), spans)
	}
}
