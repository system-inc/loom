package runner

import (
	"bytes"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// Nothing follows a unit's finished event (protocol.Decide reads an event after finished as a problem, and the run is
// void): a beat racing finished from another goroutine, or a late line, is dropped, and a beat checks the silence and
// says its line under one lock. Mutants: no finished guard (a beat lands after finished); the beat's check outside the
// lock.
func TestNothingFollowsFinished(t *testing.T) {
	for round := range 200 {
		var stream lockedBuffer
		emitter := &emitter{run: "r", unit: "u", writer: &stream, now: time.Now}
		emitter.emit(protocol.Event{Type: "started"})
		var beaters sync.WaitGroup
		for range 4 {
			beaters.Add(1)
			go func() {
				defer beaters.Done()
				for range 50 {
					emitter.beat(0, "loom-runner: still running")
				}
			}()
		}
		emitter.emit(protocol.Event{Type: "finished", Status: protocol.StatusPassed})
		beaters.Wait()
		emitter.emit(protocol.Event{Type: "output", Stream: "runner", Text: "late"})
		lines := bytes.Split(bytes.TrimSpace(stream.Bytes()), []byte("\n"))
		events := decodeEvents(t, stream.Bytes())
		if last := events[len(events)-1]; last.Type != "finished" || len(lines) != len(events) {
			t.Fatalf("round %d: the stream ends %+v", round, last)
		}
		for index, event := range events {
			if event.Sequence != index {
				t.Fatalf("round %d: event %d has sequence %d", round, index, event.Sequence)
			}
		}
	}
	// A beat says nothing until the unit has been silent its heartbeat.
	var stream lockedBuffer
	emitter := &emitter{run: "r", unit: "u", writer: &stream, now: time.Now}
	emitter.emit(protocol.Event{Type: "started"})
	emitter.beat(time.Hour, "loom-runner: still running")
	if events := decodeEvents(t, stream.Bytes()); len(events) != 1 {
		t.Fatalf("a beat inside its heartbeat said something: %+v", events)
	}
}
