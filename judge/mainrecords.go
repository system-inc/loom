package judge

// Main's recorded verdicts, read from Queue's log (Loom, Oct 11 02:02Z): a candidate whose tree is main's commit itself,
// a verify of main (base = sha), decides each of its units on that commit, so its records are main's recorded verdict
// for each unit at that base. A branch's unit is matched to main's by its plan's name, never its key: the key names the
// unit's closure, gate tools and inputs, which differ on main whenever the branch touched them. With these, a failure
// main shares can be excused as main's red (Decide's mainRed), which NoMainRecords never allowed.

import (
	"encoding/json"
	"fmt"
	"sync"
)

// EventLog reads Queue's log after a seq: HTTPLog.EventsAfter.
type EventLog interface {
	EventsAfter(after int64) ([]LogEvent, error)
}

// LogMainRecords reads main's recorded verdicts from Queue's log, keeping what it has read: each call reads only the
// events after the last it saw. Safe for one judge's passes; make it with NewLogMainRecords.
type LogMainRecords struct {
	Log EventLog
	// state is behind a pointer, so the copies Loop and Puller make of a LogMainRecords share one read of the log.
	state *mainRecordsState
}

type mainRecordsState struct {
	mutex sync.Mutex
	after int64
	// names is each planned unit's name, by its future and key; newest is each future's newest decided record, not void,
	// of each unit name. Latest asks it at a branch's base, main's commit, which only a candidate testing exactly that
	// commit's tree, a verify, decides.
	names  map[string]string
	newest map[string]mainRecord
}

type mainRecord struct {
	seq    int64
	status string
	tests  []TestOutcome
}

// NewLogMainRecords is main's records read from log.
func NewLogMainRecords(log EventLog) LogMainRecords {
	return LogMainRecords{Log: log, state: &mainRecordsState{names: map[string]string{}, newest: map[string]mainRecord{}}}
}

// Latest is main's newest decided verdict, passed or failed, for the unit's name at base: its failing tests, the ones its
// record carries inline. A void record says nothing of main and is passed over; no verdict at all is found false.
func (records LogMainRecords) Latest(base string, unit PlanUnit) ([]TestOutcome, bool, error) {
	if records.state == nil {
		return nil, false, fmt.Errorf("main's records were made without NewLogMainRecords")
	}
	state := records.state
	state.mutex.Lock()
	defer state.mutex.Unlock()
	events, err := records.Log.EventsAfter(state.after)
	if err != nil {
		return nil, false, fmt.Errorf("reading Queue's log for main's records: %w", err)
	}
	for _, event := range events {
		state.after = max(state.after, event.Seq)
		if err := state.read(event); err != nil {
			return nil, false, err
		}
	}
	if unit.Name == "" {
		return nil, false, nil
	}
	record, found := state.newest[base+" "+unit.Name]
	if !found {
		return nil, false, nil
	}
	return record.tests, true, nil
}

// read folds one event in: a unit's plan gives its name, and a decided record of a verify's unit is main's.
func (state *mainRecordsState) read(event LogEvent) error {
	if event.Subject.UnitKey == "" {
		return nil
	}
	switch event.Type {
	case "unit.planned":
		var data struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return fmt.Errorf("event %d: %w", event.Seq, err)
		}
		state.names[event.Subject.Future+" "+event.Subject.UnitKey] = data.Name
	case "verdict.decided":
		var data struct {
			Verdict struct {
				Future string `json:"future"`
				Status string `json:"status"`
				Tests  struct {
					Inline []TestOutcome `json:"inline"`
				} `json:"tests"`
			} `json:"verdict"`
		}
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return fmt.Errorf("event %d: %w", event.Seq, err)
		}
		name := state.names[event.Subject.Future+" "+event.Subject.UnitKey]
		if name == "" || data.Verdict.Status == Void {
			return nil
		}
		key := event.Subject.Future + " " + name
		if event.Seq > state.newest[key].seq {
			state.newest[key] = mainRecord{seq: event.Seq, status: data.Verdict.Status, tests: failing(data.Verdict.Tests.Inline)}
		}
	}
	return nil
}
