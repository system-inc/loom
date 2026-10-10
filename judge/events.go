package judge

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/system-inc/loom/protocol"
)

// FinishedFromEvents reads one unit's last attempt from its run's events (protocol.Event, as the coordinator records
// them) into what Decide needs. Infra comes from structure only (Loom's binding rule): a kill is the runner's exit
// event saying it saw a signal or hit the deadline; a placement error is neverPlaced; a finish the runner marked broken
// for any other reason is refused (the machine couldn't run it). Nothing a test printed decides infra: a test that
// panics saying "exceeded its deadline" exits with a code and no signal, and reads failed. Tests come from any test2json
// lines in the output events, and a test that never reached pass, fail or skip keeps its last action, which Decide reads
// as red; but the runner uploads its go test -json as TestLogPath rather than streaming it, so TestLog names that blob
// and WithTestLog reads the tests from it. found is false when the unit has no started event: it never reported.
func FinishedFromEvents(events []protocol.Event) (finished Finished, found bool) {
	start := -1
	for index, event := range events {
		if event.Type == "started" {
			start = index
		}
	}
	placeError := false
	for _, event := range events {
		if event.Type == "error" && event.Phase == protocol.PhasePlace {
			placeError = true
		}
	}
	if start < 0 {
		if placeError {
			return Finished{Attempt: Attempt{Status: Broken}, Infra: InfraNeverPlaced}, true
		}
		return Finished{}, false
	}
	attempt := Attempt{Machine: events[start].Machine, Runner: events[start].RunnerVersion, RunnerSha256: events[start].RunnerSha256, StartedAt: events[start].Time}
	finished.RanWith = protocol.Resources{Cpus: events[start].Cpus, MemoryMegabytes: events[start].MemoryMegabytes}
	if attempt.RunnerSha256 == "" {
		attempt.RunnerSha256 = RunnerUnreported
	}
	reader := newTestReader()
	outputs := []string{}
	killed, finishedStatus := false, ""
	for _, event := range events[start:] {
		switch event.Type {
		case "output":
			reader.lines(strings.Split(event.Text, "\n"))
		case "exit":
			if event.Code != nil {
				attempt.Exit = *event.Code
			}
			attempt.WallSeconds = event.WallSeconds
			killed = event.Signal != "" || event.TimedOut
		case "uploaded":
			outputs = append(outputs, event.Sha256)
			if event.Path == TestLogPath {
				finished.TestLog = &TestLogRef{Run: event.Run, Sha256: event.Sha256}
			}
		case "finished":
			finishedStatus = event.Status
			attempt.FinishedAt = event.Time
		}
	}
	finished.Tests, finished.Events = reader.read()
	sort.Strings(outputs)
	finished.Outputs = outputs
	switch {
	case killed:
		attempt.Status, finished.Infra = Broken, InfraKill
	case finishedStatus == protocol.StatusPassed:
		attempt.Status = Passed
	case finishedStatus == protocol.StatusFailed:
		attempt.Status = Failed
	case finishedStatus == protocol.StatusBroken:
		attempt.Status, finished.Infra = Broken, InfraRefused
	default:
		// Started and never finished: the runner went silent.
		attempt.Status, finished.Infra = Broken, InfraSilent
	}
	finished.Attempt = attempt
	return finished, true
}

// TestLogPath is the uploaded output that holds a unit's go test -json, gzipped (loom-runner's loom-out).
const TestLogPath = "loom-out/test.jsonl.gz"

// A TestLogRef names a unit's uploaded test log: the run it was uploaded to and its sha256.
type TestLogRef struct {
	Run    string
	Sha256 string
}

// WithTestLog reads an attempt's tests and test events from its uploaded test log, gzipped go test -json, replacing
// whatever the output events held, and notes whether the log reads and whether it says the package has no test files.
// A log that doesn't decompress or parse is an error: a unit's tests are never read as empty because its log was bad.
func WithTestLog(finished Finished, log []byte) (Finished, error) {
	unzipped, err := gzip.NewReader(bytes.NewReader(log))
	if err != nil {
		return Finished{}, fmt.Errorf("test log %s: %w", finished.TestLog.Sha256, err)
	}
	plain, err := io.ReadAll(unzipped)
	if err != nil {
		return Finished{}, fmt.Errorf("test log %s: %w", finished.TestLog.Sha256, err)
	}
	reader := newTestReader()
	lines := strings.Split(string(plain), "\n")
	for number, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var probe map[string]any
		if json.Unmarshal([]byte(line), &probe) != nil {
			return Finished{}, fmt.Errorf("test log %s: line %d isn't go test -json", finished.TestLog.Sha256, number+1)
		}
		if action, _ := probe["Action"].(string); action == "output" && probe["Test"] == nil {
			if output, _ := probe["Output"].(string); strings.Contains(output, "[no test files]") {
				finished.NoTestFiles = true
			}
		}
	}
	reader.lines(lines)
	finished.Tests, finished.Events = reader.read()
	finished.TestLogRead = true
	return finished, nil
}

// testReader folds test2json lines into each test's outcome, in first-seen order, and keeps every line that names a
// test for the census.
type testReader struct {
	tests  map[string]TestOutcome
	order  []string
	events []TestEvent
}

func newTestReader() *testReader { return &testReader{tests: map[string]TestOutcome{}} }

func (reader *testReader) lines(lines []string) {
	for _, line := range lines {
		var action struct {
			Action  string
			Package string
			Test    string
			Output  string
		}
		if json.Unmarshal([]byte(line), &action) != nil || action.Test == "" {
			continue
		}
		reader.events = append(reader.events, TestEvent{Action: action.Action, Package: action.Package, Test: action.Test, Output: action.Output})
		name := action.Package + " " + action.Test
		if _, seen := reader.tests[name]; !seen {
			reader.order = append(reader.order, name)
		}
		switch action.Action {
		case "pass", "fail", "skip":
			reader.tests[name] = TestOutcome{Package: action.Package, Test: action.Test, Outcome: action.Action}
		case "run", "pause", "cont":
			if current, seen := reader.tests[name]; !seen || current.Outcome == "" || current.Outcome == "run" {
				reader.tests[name] = TestOutcome{Package: action.Package, Test: action.Test, Outcome: "run"}
			}
		}
	}
}

func (reader *testReader) read() ([]TestOutcome, []TestEvent) {
	var tests []TestOutcome
	for _, name := range reader.order {
		tests = append(tests, reader.tests[name])
	}
	return tests, reader.events
}
