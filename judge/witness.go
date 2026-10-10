package judge

// The uncached witness (#82d430f): until read tracing lands, the lock on key reuse is an uncached whole run of main
// every hour, compared with what the planner would have reused at the same tree. A unit the planner reused, whose
// verdict at that key passed, but which reds when run uncached, is a key that misses an input: a P0 against the keys,
// paged to Loom. The comparison here is pure; the hourly run itself goes through Queue's witness future.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// A WitnessReport is what the uncached run says about the planner's reuse at one tree.
type WitnessReport struct {
	KeyFaults   []string // reused by the planner, red uncached: a P0 against the keys
	Unwitnessed []string // reused by the planner, with no uncached verdict to check it against: the witness is incomplete
	Reds        []string // run (not reused) by the planner and red uncached: an ordinary red, judged as any other
	Voids       []string // the witness's own voids: rerun them before the witness counts
}

// Clean says the witness found no key fault and checked every reused unit.
func (report WitnessReport) Clean() bool {
	return len(report.KeyFaults) == 0 && len(report.Unwitnessed) == 0 && len(report.Voids) == 0
}

// CompareWitness reads the uncached run's verdicts against the planner's plan for the same tree. Keys match exactly:
// uncached mode ignores reuse, it doesn't change a key (planner), so the reused unit and its uncached run share one.
func CompareWitness(plan []PlannedUnitWire, witness []Verdict) WitnessReport {
	report := WitnessReport{KeyFaults: []string{}, Unwitnessed: []string{}, Reds: []string{}, Voids: []string{}}
	byKey := map[string]Verdict{}
	for _, verdict := range witness {
		byKey[verdict.UnitKey] = verdict
	}
	for _, unit := range plan {
		verdict, found := byKey[unit.UnitKey]
		reused := unit.Decision == "reuse"
		switch {
		case !found && reused:
			report.Unwitnessed = append(report.Unwitnessed, unit.UnitKey)
		case !found:
		case verdict.Status == Void:
			report.Voids = append(report.Voids, unit.UnitKey)
		case verdict.Status == Failed && reused:
			report.KeyFaults = append(report.KeyFaults, unit.UnitKey)
		case verdict.Status == Failed:
			report.Reds = append(report.Reds, unit.UnitKey)
		}
	}
	return report
}

// ReadWitnessPlan reads a plan written as PlannedUnitWire JSON: a list of units, or an object holding them as "units".
func ReadWitnessPlan(content []byte) ([]PlannedUnitWire, error) {
	var units []PlannedUnitWire
	if err := json.Unmarshal(content, &units); err == nil {
		return units, nil
	}
	var wrapped struct {
		Units []PlannedUnitWire `json:"units"`
	}
	if err := json.Unmarshal(content, &wrapped); err != nil || wrapped.Units == nil {
		return nil, fmt.Errorf("the plan is a list of units or an object with units: %v", err)
	}
	return wrapped.Units, nil
}

// ReadWitnessVerdicts reads the witness's unit verdicts, one JSON line each: a verdict record, or a Queue log event
// holding one as data.verdict (other log events are skipped). Every record must be for the same future, so a
// comparison never mixes trees.
func ReadWitnessVerdicts(input io.Reader) ([]Verdict, error) {
	verdicts := []Verdict{}
	future := ""
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 1<<20), 4<<20)
	for number := 1; scanner.Scan(); number++ {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var line struct {
			UnitKey string `json:"unitKey"`
			Data    struct {
				Verdict json.RawMessage `json:"verdict"`
			} `json:"data"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("line %d: %w", number, err)
		}
		raw := json.RawMessage(scanner.Bytes())
		if line.UnitKey == "" {
			if line.Data.Verdict == nil {
				continue
			}
			raw = line.Data.Verdict
		}
		var record struct {
			UnitKey string  `json:"unitKey"`
			Future  string  `json:"future"`
			Status  string  `json:"status"`
			Cause   *string `json:"cause"`
		}
		if err := json.Unmarshal(raw, &record); err != nil || record.UnitKey == "" {
			return nil, fmt.Errorf("line %d isn't a verdict record: %v", number, err)
		}
		if future == "" {
			future = record.Future
		} else if record.Future != future {
			return nil, fmt.Errorf("line %d is a verdict at future %s, and line 1's is at %s", number, record.Future, future)
		}
		verdict := Verdict{UnitKey: record.UnitKey, Future: record.Future, Status: record.Status}
		if record.Cause != nil {
			verdict.Cause = *record.Cause
		}
		verdicts = append(verdicts, verdict)
	}
	return verdicts, scanner.Err()
}

// Lines is the report as the command prints it: one line per finding, then the summary.
func (report WitnessReport) Lines() []string {
	lines := []string{}
	for _, group := range []struct {
		name string
		keys []string
	}{{"keyFault", report.KeyFaults}, {"unwitnessed", report.Unwitnessed}, {"void", report.Voids}, {"red", report.Reds}} {
		for _, key := range group.keys {
			lines = append(lines, group.name+" "+key)
		}
	}
	verdict := "clean"
	switch {
	case len(report.KeyFaults) > 0:
		verdict = "keyFault: a reused key reds uncached, a P0 against the keys"
	case !report.Clean():
		verdict = "incomplete: rerun the voids or witness every reused unit before it counts"
	}
	return append(lines, fmt.Sprintf("witness %s (key faults %d, unwitnessed %d, voids %d, ordinary reds %d)", verdict, len(report.KeyFaults), len(report.Unwitnessed), len(report.Voids), len(report.Reds)))
}
