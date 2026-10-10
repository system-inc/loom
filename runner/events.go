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
}

// next is the sequence the next event takes.
func (emitter *emitter) next() int {
	emitter.mutex.Lock()
	defer emitter.mutex.Unlock()
	return emitter.sequence
}

// silentFor is how long since the last event left.
func (emitter *emitter) silentFor() time.Duration {
	emitter.mutex.Lock()
	defer emitter.mutex.Unlock()
	if emitter.lastEmit.IsZero() {
		return 0
	}
	return emitter.now().Sub(emitter.lastEmit)
}

func (emitter *emitter) emit(event protocol.Event) {
	emitter.mutex.Lock()
	defer emitter.mutex.Unlock()
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
