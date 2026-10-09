package judge

import (
	"strings"
	"testing"

	"github.com/system-inc/loom/protocol"
)

func TestTheAdaptersReadOnlyTheirUnitsEvents(t *testing.T) {
	unit, other := strings.Repeat("1", 64), strings.Repeat("2", 64)
	stream := []protocol.Event{
		{Unit: unit, Type: "started", Machine: "server"}, {Unit: other, Type: "started"},
		{Unit: other, Type: "exit", Code: code(-1), Signal: "killed"}, {Unit: other, Type: "finished", Status: "failed"},
		{Unit: unit, Type: "exit", Code: code(0)}, {Unit: unit, Type: "finished", Status: "passed"},
	}
	runs := EventRuns{Read: func(run string) ([]protocol.Event, error) { return stream, nil }}
	finished, found, err := runs.Finished("future-x-1", unit)
	if err != nil || !found || finished.Attempt.Status != Passed || finished.Attempt.Machine != "server" {
		t.Fatalf("unit's attempt %+v %v %v, want its own pass, not the other unit's kill", finished.Attempt, found, err)
	}
	if _, found, _ := runs.Finished("future-x-1", strings.Repeat("3", 64)); found {
		t.Fatal("a unit with no events was found")
	}
	// A rerun is a one-unit job whose unit id on main's base isn't the unitKey: its whole stream is read.
	baseRerun := []protocol.Event{{Unit: "rerun-on-base-id", Type: "started"}, {Unit: "rerun-on-base-id", Type: "exit", Code: code(-1), Signal: "killed"},
		{Unit: "rerun-on-base-id", Type: "finished", Status: "failed"}}
	fabric := EventFabric{Rerun: func(unitKey, tree string) ([]protocol.Event, error) {
		if tree == baseTree {
			return baseRerun, nil
		}
		return []protocol.Event{}, nil
	}}
	rerun, err := fabric.RerunAlone(other, baseTree)
	if err != nil || rerun.Attempt.Status != Broken || rerun.Infra != InfraKill {
		t.Fatalf("the base rerun %+v %v, want its kill read under its own unit id", rerun, err)
	}
	silent, err := fabric.RerunAlone(strings.Repeat("3", 64), futureTree)
	if err != nil || silent.Attempt.Status != Broken || silent.Infra != InfraSilent {
		t.Fatalf("a rerun that never reported reads %+v, want silent infra", silent)
	}
}
