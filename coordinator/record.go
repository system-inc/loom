package coordinator

import (
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A record is a run's events as the coordinator holds them: every line every runner said, in arrival order,
// each unit's stream numbered as one gapless sequence across its attempts. It is what protocol.Decide reads,
// so nothing a runner said is filtered out of it; the wire gets the same lines.
type record struct {
	mutex  sync.Mutex
	run    string
	events []protocol.Event
	next   map[string]int // each unit's next sequence in the record
	now    func() time.Time
	sink   func(line []byte) // hands each line, newline ended, to the wire relay as it is recorded; nil for none
}

func newRecord(run string, sink func(line []byte)) *record {
	return &record{run: run, next: map[string]int{}, now: time.Now, sink: sink}
}

// An attempt is one runner's go at one unit. Its own stream starts at sequence 0, or at the offset when the
// runner was told to continue the unit's stream (continueFromOffset); the record shifts it to follow whatever
// the unit's earlier attempts already wrote, so a re-placed unit stays one gapless stream.
type attempt struct {
	record   *record
	unit     string
	offset   int  // the record's next sequence for the unit when the attempt began
	start    int  // the runner's first sequence: 0, or offset for a pool runner told to continue the stream
	expected int  // the next sequence the runner should send
	finished bool // the runner sent finished
	lines    int  // events taken from the runner
	gap      bool // the runner skipped or repeated a sequence
}

func (record *record) begin(unit string) *attempt {
	record.mutex.Lock()
	defer record.mutex.Unlock()
	return &attempt{record: record, unit: unit, offset: record.next[unit]}
}

// continueFromOffset has the runner number its events from the attempt's offset, for a runner that posts its
// events to the wire itself: the wire already holds the unit's earlier attempts, and a runner numbering from 0
// again would conflict with them.
func (attempt *attempt) continueFromOffset(unit *protocol.Unit) {
	unit.SequenceStart = attempt.offset
	attempt.start = attempt.offset
	attempt.expected = attempt.offset
}

// take records one event a runner sent. An event of this attempt's run and unit is renumbered into the
// unit's stream; anything else (another run's, another unit's) is recorded as sent, for Decide to judge.
func (attempt *attempt) take(event protocol.Event) {
	record := attempt.record
	record.mutex.Lock()
	defer record.mutex.Unlock()
	attempt.lines++
	if event.Run == record.run && event.Unit == attempt.unit {
		if event.Sequence != attempt.expected {
			attempt.gap = true
		}
		attempt.expected = event.Sequence + 1
		event.Sequence += attempt.offset - attempt.start
		if event.Sequence >= record.next[attempt.unit] {
			record.next[attempt.unit] = event.Sequence + 1
		}
		if event.Type == "finished" {
			attempt.finished = true
		}
	}
	record.append(event)
}

// note adds the coordinator's own event to a unit's stream, such as the error that says a box dropped it.
func (record *record) note(unit string, event protocol.Event) {
	record.mutex.Lock()
	defer record.mutex.Unlock()
	event.Run = record.run
	event.Unit = unit
	event.Sequence = record.next[unit]
	event.Time = record.now().UTC().Format(timeLayout)
	record.next[unit] = event.Sequence + 1
	record.append(event)
}

// append keeps the event and hands its line to the relay; the caller holds the mutex.
func (record *record) append(event protocol.Event) {
	record.events = append(record.events, event)
	if record.sink != nil {
		line, err := json.Marshal(event)
		if err != nil {
			// An Event holds only strings, numbers and a map of strings, so this can't happen.
			panic(err)
		}
		record.sink(append(line, '\n'))
	}
}

// snapshot is every event so far, for Decide.
func (record *record) snapshot() []protocol.Event {
	record.mutex.Lock()
	defer record.mutex.Unlock()
	return append([]protocol.Event(nil), record.events...)
}

// unitEvents is one unit's stream in order, for its cache entry's event log.
func (record *record) unitEvents(unit string) []protocol.Event {
	record.mutex.Lock()
	defer record.mutex.Unlock()
	var result []protocol.Event
	for _, event := range record.events {
		if event.Run == record.run && event.Unit == unit {
			result = append(result, event)
		}
	}
	return result
}

// timeLayout is the runner's: RFC 3339 in UTC with milliseconds.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// placeError is the coordinator's error event about where a unit ran.
func placeError(format string, arguments ...any) protocol.Event {
	return protocol.Event{Type: "error", Phase: protocol.PhasePlace, Message: fmt.Sprintf(format, arguments...)}
}
