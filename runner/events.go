package runner

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/system-inc/loom/poster"
	"github.com/system-inc/loom/protocol"
)

// timeLayout is RFC 3339 in UTC with milliseconds, the form every event's time takes.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// An emitter numbers and writes one unit's events. emit holds a single lock while it assigns the next
// sequence, stamps the time, writes the line and hands it to the wire, so lines leave in sequence order
// whichever goroutine sent them, and the wire sees exactly the bytes stdout saw.
type emitter struct {
	mutex      sync.Mutex
	run        string
	unit       string
	sequence   int
	writer     io.Writer
	wire       *poster.Poster
	now        func() time.Time
	writeError error
	lastEmit   time.Time // when the last event left, so a silent unit can be told from a lost runner
	// finished is set once the unit's finished event has left: nothing follows it (protocol.Decide reads an event
	// after finished as a problem, and the run is void), so a beat or a late line from a goroutine is dropped.
	finished bool
}

// next is the sequence the next event takes.
func (emitter *emitter) next() int {
	emitter.mutex.Lock()
	defer emitter.mutex.Unlock()
	return emitter.sequence
}

func (emitter *emitter) emit(event protocol.Event) {
	emitter.mutex.Lock()
	defer emitter.mutex.Unlock()
	emitter.emitLocked(event)
}

// beat says the unit is still running, text, when it has been silent at least heartbeat, the check and the line under
// one lock, so no event slips between them and none follows finished (Loom's review of feb1728, before release 7).
func (emitter *emitter) beat(heartbeat time.Duration, text string) {
	emitter.mutex.Lock()
	defer emitter.mutex.Unlock()
	if emitter.lastEmit.IsZero() || emitter.now().Sub(emitter.lastEmit) < heartbeat {
		return
	}
	emitter.emitLocked(protocol.Event{Type: "output", Stream: "runner", Text: text})
}

func (emitter *emitter) emitLocked(event protocol.Event) {
	if emitter.finished {
		return
	}
	emitter.finished = event.Type == "finished"
	event.Run = emitter.run
	event.Unit = emitter.unit
	event.Sequence = emitter.sequence
	event.Time = emitter.now().UTC().Format(timeLayout)
	var line bytes.Buffer
	encoder := json.NewEncoder(&line)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(event); err != nil {
		// An Event holds only strings, numbers and a map of strings, so this can't happen.
		panic(err)
	}
	emitter.sequence++
	emitter.lastEmit = emitter.now()
	if _, err := emitter.writer.Write(line.Bytes()); err != nil && emitter.writeError == nil {
		emitter.writeError = err
	}
	if emitter.wire != nil {
		emitter.wire.Enqueue(line.Bytes())
	}
}
