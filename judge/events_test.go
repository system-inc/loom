package judge

import (
	"testing"

	"github.com/system-inc/loom/protocol"
)

func code(value int) *int { return &value }

func testLine(action, test string) string {
	return `{"Action":"` + action + `","Package":"` + testPackage + `","Test":"` + test + `"}`
}

func started() protocol.Event {
	return protocol.Event{Type: "started", Machine: "server", RunnerVersion: "git-abc", Time: "2026-10-09T23:50:00Z"}
}

func TestEventsReadAsTheirStructureSays(t *testing.T) {
	cases := []struct {
		name   string
		events []protocol.Event
		status string
		infra  string
		found  bool
	}{
		{"a pass", []protocol.Event{started(), {Type: "exit", Code: code(0)}, {Type: "finished", Status: "passed"}}, Passed, "", true},
		{"a failure", []protocol.Event{started(), {Type: "exit", Code: code(1)}, {Type: "finished", Status: "failed"}}, Failed, "", true},
		{"a signal is a kill", []protocol.Event{started(), {Type: "exit", Code: code(-1), Signal: "killed"}, {Type: "finished", Status: "failed"}}, Broken, InfraKill, true},
		{"the runner's deadline is a kill", []protocol.Event{started(), {Type: "exit", Code: code(-1), TimedOut: true}, {Type: "finished", Status: "failed"}}, Broken, InfraKill, true},
		{"a test that prints it exceeded its deadline is a failure, not a kill", []protocol.Event{started(),
			{Type: "output", Text: "TestPortMatchesGoCohereClassOrder_001 exceeded its 90s deadline\nsignal: killed"},
			{Type: "exit", Code: code(2)}, {Type: "finished", Status: "failed"}}, Failed, "", true},
		{"a finish the runner marked broken is refused", []protocol.Event{started(), {Type: "exit", Code: code(75)}, {Type: "finished", Status: "broken"}}, Broken, InfraRefused, true},
		{"started and never finished is silent", []protocol.Event{started(), {Type: "output", Text: "working"}}, Broken, InfraSilent, true},
		{"never placed", []protocol.Event{{Type: "error", Phase: protocol.PhasePlace, Message: "no fit machine"}}, Broken, InfraNeverPlaced, true},
		{"no events at all never reported", []protocol.Event{}, "", "", false},
		{"the last attempt counts", []protocol.Event{started(), {Type: "exit", Code: code(-1), Signal: "killed"}, {Type: "finished", Status: "broken"},
			{Type: "started", Machine: "workshop", Time: "2026-10-09T23:55:00Z"}, {Type: "exit", Code: code(0)}, {Type: "finished", Status: "passed"}}, Passed, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			finished, found := FinishedFromEvents(c.events)
			if found != c.found || finished.Attempt.Status != c.status || finished.Infra != c.infra {
				t.Fatalf("got %v %q %q, want %v %q %q", found, finished.Attempt.Status, finished.Infra, c.found, c.status, c.infra)
			}
			if c.name == "the last attempt counts" && (finished.Attempt.Machine != "workshop" || finished.Attempt.StartedAt != "2026-10-09T23:55:00Z") {
				t.Fatalf("attempt %+v, want the last one, on workshop", finished.Attempt)
			}
		})
	}
}

func TestTestsComeFromTest2jsonLinesAndAnUnfinishedTestStaysUnfinished(t *testing.T) {
	events := []protocol.Event{started(),
		{Type: "output", Text: testLine("run", "TestA") + "\n" + testLine("pass", "TestA") + "\nplain text\n" + testLine("run", "TestB")},
		{Type: "output", Text: testLine("run", "TestC") + "\n" + testLine("fail", "TestC")},
		{Type: "uploaded", Sha256: "b2"}, {Type: "uploaded", Sha256: "a1"},
		{Type: "exit", Code: code(1), WallSeconds: 4.5}, {Type: "finished", Status: "failed", Time: "2026-10-09T23:51:00Z"}}
	finished, _ := FinishedFromEvents(events)
	got := map[string]string{}
	for _, outcome := range finished.Tests {
		got[outcome.Test] = outcome.Outcome
	}
	if len(got) != 3 || got["TestA"] != "pass" || got["TestB"] != "run" || got["TestC"] != "fail" {
		t.Fatalf("tests %v", got)
	}
	if len(finished.Outputs) != 2 || finished.Outputs[0] != "a1" {
		t.Fatalf("outputs %v, want both, sorted", finished.Outputs)
	}
	attempt := finished.Attempt
	if attempt.Machine != "server" || attempt.Runner != "git-abc" || attempt.Exit != 1 || attempt.WallSeconds != 4.5 || attempt.FinishedAt == "" {
		t.Fatalf("attempt %+v", attempt)
	}
	// Decide reads the unfinished TestB as red, through the failure path.
	decision, err := Decide(evidenceOf(finished))
	if err != nil || decision.Next != "rerunAlone" {
		t.Fatalf("decision %+v %v", decision, err)
	}
}
