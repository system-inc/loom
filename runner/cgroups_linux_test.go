package runner

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// These run only where systemd delegated the test's cgroup, as loom-serve.service's Delegate=yes does:
//
//	go test -c -o runner.test ./runner
//	systemd-run --user --wait --pipe -p Delegate=yes -p DelegateSubgroup=serve ./runner.test -test.run Cgroup
//
// Mutants: a unit's commands not started in its cgroup, or memory.max unset, so the hog takes the machine's memory or
// its neighbor's: TestCgroupAUnitPastItsMemoryIsKilledAloneAndBroken. cpu.max unset: TestCgroupAUnitKeepsToItsCpus.

func delegatedForTest(t *testing.T) {
	t.Helper()
	if _, err := delegatedCgroups(); err != nil {
		t.Skipf("no delegated cgroup here (run under systemd-run --user -p Delegate=yes -p DelegateSubgroup=serve): %v", err)
	}
}

func TestCgroupAUnitPastItsMemoryIsKilledAloneAndBroken(t *testing.T) {
	delegatedForTest(t)
	pool := newTestPool(t)
	hog := pool.unit("hog", `python3 -c 'b = bytearray(1 << 30); import time; time.sleep(5)'`)
	hog.Resources = protocol.Resources{Cpus: 1, MemoryMegabytes: 256}
	pool.queue = []protocol.Unit{hog, pool.unit("neighbor", `python3 -c 'b = bytearray(400 << 20); import time; time.sleep(3)'`)}
	options := pool.slotsOptions(t, 8, 64<<10, 4, 1, 1024)
	var report lockedBuffer
	options.Report = &report
	summary, err := Serve(context.Background(), options)
	if err != nil || summary.Units != 2 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	pool.mutex.Lock()
	hogEvents, neighborEvents := pool.events["hog"], pool.events["neighbor"]
	pool.mutex.Unlock()
	if status := hogEvents[len(hogEvents)-1].Status; status != protocol.StatusBroken {
		t.Fatalf("the hog finished %s, not broken", status)
	}
	said := false
	for _, event := range hogEvents {
		said = said || event.Type == "error" && strings.Contains(event.Message, "memory share of 256 MB") && strings.Contains(event.Message, "Loom's")
	}
	if !said {
		t.Fatalf("the hog's stream doesn't say its memory share killed it: %+v", hogEvents)
	}
	if status := neighborEvents[len(neighborEvents)-1].Status; status != protocol.StatusPassed {
		t.Fatalf("the hog's neighbor finished %s: %+v", status, neighborEvents)
	}
	if mostAtOnce(pool.spans(t, "hog", "neighbor")) != 2 {
		t.Fatal("the hog and its neighbor didn't run at once")
	}
	if !strings.Contains(string(report.Bytes()), "unit neighbor passed on 1 cpus, peak") {
		t.Fatalf("serve's report: %s", string(report.Bytes()))
	}
	// Every unit's cgroup is gone with it.
	cgroups, _ := delegatedCgroups()
	if leftover, _ := filepath.Glob(filepath.Join(cgroups.parent, unitCgroupPrefix+"*")); len(leftover) != 0 {
		t.Fatalf("units' cgroups outlived them: %v", leftover)
	}
}

func TestCgroupAUnitKeepsToItsCpus(t *testing.T) {
	delegatedForTest(t)
	pool := newTestPool(t)
	// Four busy processes for three seconds on a one-cpu share use about three seconds of CPU, not twelve.
	spin := pool.unit("spin", `for i in 1 2 3 4; do timeout 3 sh -c 'while :; do :; done' & done; wait; exit 0`)
	spin.Resources = protocol.Resources{Cpus: 1, MemoryMegabytes: 512}
	pool.queue = []protocol.Unit{spin}
	options := pool.slotsOptions(t, 8, 64<<10, 4, 1, 1024)
	options.Units = 2
	started := time.Now()
	if summary, err := Serve(context.Background(), options); err != nil || summary.Passed != 1 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	for _, event := range pool.events["spin"] {
		if event.Type == "exit" {
			if used := event.UserSeconds + event.SystemSeconds; used > 4.5 || used < 1.5 {
				t.Fatalf("a one-cpu unit used %.1f s of CPU in %.1f s", used, time.Since(started).Seconds())
			}
			return
		}
	}
	t.Fatal("no exit event")
}

// A unit run alone, one at a time, gets a cgroup too, of the whole machine's share, so its timing says its peak memory
// (#g1jvdbq). Mutant: alone units without a cgroup, whose timing has no peak.
func TestCgroupAUnitRunAloneHasACgroupAndItsTimingAPeak(t *testing.T) {
	delegatedForTest(t)
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("alone", `python3 -c 'b = bytearray(300 << 20); import time; time.sleep(1)'`)}
	options := pool.slotsOptions(t, 8, 64<<10, 1, 1, 1024)
	if summary, err := Serve(context.Background(), options); err != nil || summary.Passed != 1 {
		t.Fatalf("summary %+v, %v", summary, err)
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	for _, event := range pool.events["alone"] {
		if event.Type == "timing" {
			if event.Timing.PeakMegabytes < 300 || event.Timing.ShareCpus != 8 || event.Timing.ShareMemoryMegabytes != 64<<10*9/10 || event.Timing.UnitsInHand != 1 {
				t.Fatalf("the alone unit's timing: %+v", event.Timing)
			}
			return
		}
	}
	t.Fatal("no timing event")
}
