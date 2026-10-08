package runner

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/system-inc/loom/protocol"
)

// maximumLineBytes is the longest text one output event carries; a longer line is split into several.
const maximumLineBytes = 64 << 10

// splitLines reads a stream and calls emit once per line, newline removed. A final line without a newline
// is still a line. A line longer than maximum is emitted in pieces of at most maximum bytes, each cut on a
// UTF-8 boundary when one is near, so a split never invents a replacement character. emit may keep nothing
// it is handed: the slice is reused.
func splitLines(reader io.Reader, maximum int, emit func(line []byte)) error {
	line := make([]byte, 0, maximum)
	chunk := make([]byte, 32<<10)
	for {
		count, readError := reader.Read(chunk)
		data := chunk[:count]
		for len(data) > 0 {
			if len(line) == maximum {
				if data[0] == '\n' {
					emit(line)
					line = line[:0]
					data = data[1:]
					continue
				}
				cut := runeBoundary(line)
				emit(line[:cut])
				line = line[:copy(line, line[cut:])]
				continue
			}
			room := maximum - len(line)
			if newline := bytes.IndexByte(data, '\n'); newline >= 0 && newline <= room {
				line = append(line, data[:newline]...)
				emit(line)
				line = line[:0]
				data = data[newline+1:]
				continue
			}
			take := min(room, len(data))
			line = append(line, data[:take]...)
			data = data[take:]
		}
		if readError != nil {
			if len(line) > 0 {
				emit(line)
			}
			if readError == io.EOF {
				return nil
			}
			return readError
		}
	}
}

// runeBoundary returns where to cut a full line so its last UTF-8 sequence isn't split: before a trailing
// incomplete sequence, or at the end when there is none (or when the line is nothing but one).
func runeBoundary(line []byte) int {
	for back := 1; back < utf8.UTFMax && back <= len(line); back++ {
		start := len(line) - back
		if utf8.RuneStart(line[start]) {
			if !utf8.FullRune(line[start:]) && start > 0 {
				return start
			}
			return len(line)
		}
	}
	return len(line)
}

// outputEvent makes one line into an output event, replacing invalid UTF-8 with U+FFFD and saying so.
func outputEvent(stream string, line []byte) protocol.Event {
	event := protocol.Event{Type: "output", Stream: stream, Text: string(line)}
	if !utf8.Valid(line) {
		event.Text = strings.ToValidUTF8(event.Text, "�")
		event.Replaced = true
	}
	return event
}
