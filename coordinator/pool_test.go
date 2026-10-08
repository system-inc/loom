package coordinator

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner"
)

// servePool runs workers in this process as `loom-runner serve` runs on a Codex instance: each asks the
// fake wire's pool for units with a pool token and runs them, posting their events to the run itself. They
// serve until the test ends.
func servePool(t *testing.T, wire *fakeWire, pool string, workers int) {
	token, err := protocol.MintToken(testSecret, protocol.TokenClaims{Run: pool, Scope: protocol.ScopePool, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	serveContext, cancel := context.WithCancel(context.Background())
	var group sync.WaitGroup
	for index := range workers {
		options := runner.ServeOptions{Pool: wire.server.URL + "/pools/" + pool, Token: token, Worker: fmt.Sprintf("worker-%d", index),
			Deadline: time.Now().Add(time.Hour), Unit: runner.Options{WorkspaceParent: t.TempDir()}}
		group.Add(1)
		go func() {
			defer group.Done()
			runner.Serve(serveContext, options)
		}()
	}
	t.Cleanup(func() {
		cancel()
		group.Wait()
	})
}

// poolSlots is one pool given to the coordinator as count slots: the same machine each time, so its slots
// share their reading of each run's log.
func poolSlots(wire *fakeWire, count int) []Machine {
	pool := &PoolMachine{Pool: "codex", Wire: wire.server.URL, Secret: testSecret, Version: runner.Version,
		GoPlatform: runtime.GOOS + "/" + runtime.GOARCH, CoreCount: 8}
	slots := make([]Machine, count)
	for index := range slots {
		slots[index] = pool
	}
	return slots
}

// poolUnit is a shell unit with a short timeout, so a unit the coordinator never hears from goes late soon.
func poolUnit(id string, script string) protocol.JobUnit {
	unit := shell(id, script)
	unit.TimeoutSeconds = 5
	return unit
}

func unitEventsOf(events []protocol.Event, unit string) []protocol.Event {
	var result []protocol.Event
	for _, event := range events {
		if event.Unit == unit {
			result = append(result, event)
		}
	}
	return result
}

func TestThreeUnitsOnPoolSlotsAreGreenAndTheWireHoldsTheRecord(t *testing.T) {
	wire := newFakeWire(t)
	servePool(t, wire, "codex", 2)
	result := run(t, config(wire, poolSlots(wire, 3)...), poolUnit("a", "echo a"), poolUnit("b", "echo b"), poolUnit("c", "echo c"))
	if result.Verdict.Status != "green" {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	// The runners posted each unit's stream; the coordinator read it back and relayed it, and the wire kept
	// one copy. Each unit's stream on the wire is the coordinator's record of it, event for event.
	held := wire.events(result.Run)
	if len(held) != len(result.Events) {
		t.Fatalf("the wire holds %d events, the record %d", len(held), len(result.Events))
	}
	hostname, _ := os.Hostname()
	for _, id := range []string{"a", "b", "c"} {
		onWire, inRecord := unitEventsOf(held, id), unitEventsOf(result.Events, id)
		if !reflect.DeepEqual(onWire, inRecord) {
			t.Fatalf("%s: the wire has %+v, the record %+v", id, onWire, inRecord)
		}
		// The worker says where it ran; the pool is only where the coordinator placed it.
		said := ""
		for _, event := range inRecord {
			if event.Type == "output" {
				said += event.Text
			}
		}
		if inRecord[0].Type != "started" || inRecord[0].Machine != hostname || said != id {
			t.Fatalf("%s: %+v", id, inRecord)
		}
	}
	if again := protocol.Decide(result.Run, []string{"a", "b", "c"}, held); again.Status != "green" {
		t.Fatalf("the wire's events decide %+v", again)
	}
	// The three slots waited on the run together and read its log through one follower.
	wire.mutex.Lock()
	mostRead, machines := wire.mostRead, wire.machines
	wire.mutex.Unlock()
	if mostRead != 1 {
		t.Fatalf("the run's log was read %d at once; the pool's slots share one follower", mostRead)
	}
	if len(machines) != 1 || machines[0] != (BoardMachine{Name: "pool:codex", Cores: 8, Slots: 3}) {
		t.Fatalf("machines %+v", machines)
	}
	if !reflect.DeepEqual(result.Machines, []string{"pool:codex"}) {
		t.Fatalf("ran on %v", result.Machines)
	}
}

func TestAPlantedFailureOnAPoolUnitTurnsTheRunRed(t *testing.T) {
	wire := newFakeWire(t)
	servePool(t, wire, "codex", 2)
	result := run(t, config(wire, poolSlots(wire, 2)...), poolUnit("a", "echo a"), poolUnit("planted", "echo boom >&2; exit 3"), poolUnit("c", "echo c"))
	if result.Verdict.Status != "red" || !reflect.DeepEqual(result.Verdict.Failed, []string{"planted"}) {
		t.Fatalf("verdict %+v", result.Verdict)
	}
	if !strings.Contains(fmt.Sprint(unitEventsOf(result.Events, "planted")), "boom") {
		t.Fatal("the planted unit's output isn't in the record")
	}
}

func TestAPoolUnitNobodyTakesLeavesAStoppedRunVoid(t *testing.T) {
	wire := newFakeWire(t)
	stopping, stop := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, stop)
	started := time.Now()
	result, err := Run(stopping, config(wire, poolSlots(wire, 1)...), protocol.Job{Name: "j", Units: []protocol.JobUnit{poolUnit("waits", "true")}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Verdict.Status != "void" || time.Since(started) > 5*time.Second {
		t.Fatalf("verdict %+v after %v", result.Verdict, time.Since(started))
	}
	// The unit is left queued: cancelling the pool for the run would drop its other units too.
	wire.mutex.Lock()
	defer wire.mutex.Unlock()
	if queue := wire.pools["codex"]; len(queue) != 1 || queue[0].Wire == nil || queue[0].Wire.Url != wire.server.URL+"/runs/"+result.Run+"/events" {
		t.Fatalf("queued %+v", queue)
	}
}
