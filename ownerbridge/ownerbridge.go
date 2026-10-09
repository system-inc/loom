// Package ownerbridge carries what a change's owner acts on, and nothing else, from loom-pipeline to the owner's
// inbox (docs/contracts.md, section 5): change.landed, change.red and change.parked, read from the owners' feed
// (GET /changes/events?after=<seq>, a coordinator token) and posted with `ahra os send <owner>` on Kirk's Mac until
// the inbox has an HTTP door. Each event is sent once: the bridge keeps the last sequence it sent in a state file,
// written after each send, so a crash between a send and its write repeats at most that one event.
package ownerbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Event is one line of the owners' feed: an event of the log (contracts section 4).
type Event struct {
	Seq     int64           `json:"seq"`
	At      string          `json:"at"`
	Type    string          `json:"type"`
	Subject Subject         `json:"subject"`
	Data    json.RawMessage `json:"data"`
}

// Subject names what an event is about.
type Subject struct {
	Change  string `json:"change"`
	Future  string `json:"future"`
	UnitKey string `json:"unitKey"`
	Run     string `json:"run"`
}

// Record is the part of a change's record (contracts section 1) a message needs.
type Record struct {
	Change string `json:"change"`
	Sha    string `json:"sha"`
	Base   string `json:"base"`
	Owner  string `json:"owner"`
}

// Bridge reads the feed and sends what it holds.
type Bridge struct {
	Pipeline  string                                // loom-pipeline's origin
	Token     func() string                         // a coordinator token, fresh enough to use
	Client    *http.Client                          //
	Send      func(owner string, text string) error // posts one message to one owner's inbox
	StatePath string                                // the last sequence sent
	records   map[string]Record
}

// Once sends every owner event after the last one sent, in order, and stops at the first that fails, so the next
// pass starts from it. It returns how many it sent.
func (bridge *Bridge) Once(runContext context.Context) (int, error) {
	after, err := bridge.lastSent()
	if err != nil {
		return 0, err
	}
	body, err := bridge.get(runContext, "/changes/events?after="+strconv.FormatInt(after, 10))
	if err != nil {
		return 0, err
	}
	sent := 0
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event Event
		if err := json.Unmarshal(line, &event); err != nil {
			return sent, fmt.Errorf("the owners' feed: %v", err)
		}
		if event.Seq <= after {
			continue
		}
		record, err := bridge.record(runContext, event.Subject.Change)
		if err != nil {
			return sent, err
		}
		if err := bridge.Send(record.Owner, Message(event, record)); err != nil {
			return sent, fmt.Errorf("sending event %d to %s: %v", event.Seq, record.Owner, err)
		}
		if err := bridge.markSent(event.Seq); err != nil {
			return sent, err
		}
		after = event.Seq
		sent++
	}
	return sent, scanner.Err()
}

// Message is what the owner reads: one line saying what happened to their change and what to do, then anything
// in the event's data the line doesn't spell out, as its JSON, so nothing the log says is lost.
func Message(event Event, record Record) string {
	var data map[string]json.RawMessage
	json.Unmarshal(event.Data, &data)
	text := func(key string) string {
		var value string
		if json.Unmarshal(data[key], &value) == nil && value != "" {
			delete(data, key)
			return value
		}
		return ""
	}
	short := func(sha string) string {
		if len(sha) > 12 {
			return sha[:12]
		}
		return sha
	}
	var line string
	switch event.Type {
	case "change.landed":
		main, from := text("main"), text("from")
		line = fmt.Sprintf("Loom: your change %s (%s) landed on main", record.Change, short(record.Sha))
		if main != "" {
			line += " as " + short(main)
		}
		if from != "" {
			line += ", on top of " + short(from)
		}
		line += "."
	case "change.red":
		line = fmt.Sprintf("Loom: your change %s (%s) is red.", record.Change, short(record.Sha))
		if failing := failingTests(data["verdict"]); failing != "" {
			delete(data, "verdict")
			line += " Failing: " + failing + "."
		}
		line += fmt.Sprintf(" The diff: git diff %s..%s.", short(record.Base), short(record.Sha))
		if event.Subject.UnitKey != "" {
			line += " Reproduce it: loom repro " + event.Subject.UnitKey + "."
		}
	case "change.parked":
		by := text("by")
		line = fmt.Sprintf("Loom: your change %s (%s) is parked", record.Change, short(record.Sha))
		if by != "" {
			line += ": " + by + " was kicked below it, and yours restacks when that one moves"
		}
		line += "."
	default:
		line = fmt.Sprintf("Loom: %s for your change %s (%s).", event.Type, record.Change, short(record.Sha))
	}
	if len(data) > 0 {
		rest, _ := json.Marshal(data)
		line += "\n" + string(rest)
	}
	return line
}

// failingTests names the failing tests of a verdict record (contracts section 3), package and test.
func failingTests(raw json.RawMessage) string {
	var verdict struct {
		Tests []struct {
			Package string `json:"package"`
			Test    string `json:"test"`
			Outcome string `json:"outcome"`
		} `json:"tests"`
	}
	if json.Unmarshal(raw, &verdict) != nil {
		return ""
	}
	var failing []string
	for _, test := range verdict.Tests {
		if test.Outcome == "fail" {
			failing = append(failing, test.Package+" "+test.Test)
		}
	}
	return strings.Join(failing, ", ")
}

func (bridge *Bridge) record(runContext context.Context, change string) (Record, error) {
	if record, held := bridge.records[change]; held {
		return record, nil
	}
	body, err := bridge.get(runContext, "/changes/"+change)
	if err != nil {
		return Record{}, err
	}
	var answer struct {
		Record Record `json:"record"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || answer.Record.Owner == "" {
		return Record{}, fmt.Errorf("change %s's record names no owner", change)
	}
	if bridge.records == nil {
		bridge.records = map[string]Record{}
	}
	bridge.records[change] = answer.Record
	return answer.Record, nil
}

func (bridge *Bridge) get(runContext context.Context, path string) ([]byte, error) {
	request, err := http.NewRequestWithContext(runContext, http.MethodGet, strings.TrimSuffix(bridge.Pipeline, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+bridge.Token())
	response, err := bridge.Client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s %s", path, response.Status, bytes.TrimSpace(body))
	}
	return body, nil
}

func (bridge *Bridge) lastSent() (int64, error) {
	text, err := os.ReadFile(bridge.StatePath)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(text)), 10, 64)
}

// markSent writes the sequence beside the state file and renames it over, so the file is never half written.
func (bridge *Bridge) markSent(seq int64) error {
	temporary := filepath.Join(filepath.Dir(bridge.StatePath), "."+filepath.Base(bridge.StatePath)+".writing")
	if err := os.WriteFile(temporary, []byte(strconv.FormatInt(seq, 10)+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, bridge.StatePath)
}
