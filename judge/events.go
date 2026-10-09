package judge

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/system-inc/loom/protocol"
)

// FinishedFromEvents reads one unit's last attempt from its run's events (protocol.Event, as the coordinator records
// them) into what Decide needs. Infra comes from structure only (Loom's binding rule): a kill is the runner's exit
// event saying it saw a signal or hit the deadline; a placement error is neverPlaced; a finish the runner marked broken
// for any other reason is refused (the machine couldn't run it). Nothing a test printed decides infra: a test that
// panics saying "exceeded its deadline" exits with a code and no signal, and reads failed. Tests come from the
// test2json lines in the output events, and a test that never reached pass, fail or skip keeps its last action, which
// Decide reads as red. found is false when the unit has no started event: it never reported.
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
	attempt := Attempt{Machine: events[start].Machine, Runner: events[start].RunnerVersion, StartedAt: events[start].Time}
	tests := map[string]TestOutcome{}
	order := []string{}
	outputs := []string{}
	killed, finishedStatus := false, ""
	for _, event := range events[start:] {
		switch event.Type {
		case "output":
			for _, line := range strings.Split(event.Text, "\n") {
				var action struct {
					Action  string
					Package string
					Test    string
				}
				if json.Unmarshal([]byte(line), &action) != nil || action.Test == "" {
					continue
				}
				name := action.Package + " " + action.Test
				if _, seen := tests[name]; !seen {
					order = append(order, name)
				}
				switch action.Action {
				case "pass", "fail", "skip":
					tests[name] = TestOutcome{Package: action.Package, Test: action.Test, Outcome: action.Action}
				case "run", "pause", "cont":
					if current, seen := tests[name]; !seen || current.Outcome == "" || current.Outcome == "run" {
						tests[name] = TestOutcome{Package: action.Package, Test: action.Test, Outcome: "run"}
					}
				}
			}
		case "exit":
			if event.Code != nil {
				attempt.Exit = *event.Code
			}
			attempt.WallSeconds = event.WallSeconds
			killed = event.Signal != "" || event.TimedOut
		case "uploaded":
			outputs = append(outputs, event.Sha256)
		case "finished":
			finishedStatus = event.Status
			attempt.FinishedAt = event.Time
		}
	}
	for _, name := range order {
		finished.Tests = append(finished.Tests, tests[name])
	}
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
