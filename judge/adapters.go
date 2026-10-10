package judge

// Adapters from Fabric's seams (Fabric to Judge, Oct 9, 23:49Z) to the loop's interfaces. Fabric lands
// coordinator.ReadRunEvents(ctx, wire, secret, run) and coordinator.RerunAlone(ctx, config, protocol.JobUnit) under
// those names; until then the loop's command hands these adapters stand-ins with the same shapes. A unit's id is its
// unitKey, and a future's run is coordinator.FutureRun(tree, attempt), so no mapping is kept here.

import (
	"fmt"

	"github.com/system-inc/loom/protocol"
)

// EventRuns reads a run's events and finds one unit's in them.
type EventRuns struct {
	Read func(run string) ([]protocol.Event, error) // coordinator.ReadRunEvents, bound to the wire and its token
	// Log reads an uploaded blob of a run (coordinator.ReadRunBlob, bound): each attempt's tests come from its uploaded
	// test log, which the runner doesn't stream. Nil reads tests from the output events alone.
	Log func(run, sha256 string) ([]byte, error)
}

// Finished reads the unit's last attempt in the run.
func (runs EventRuns) Finished(run, unitKey string) (Finished, bool, error) {
	events, err := runs.Read(run)
	if err != nil {
		return Finished{}, false, err
	}
	finished, found := FinishedFromEvents(eventsOf(events, unitKey))
	finished, err = readTestLog(finished, runs.Log)
	return finished, found, err
}

// readTestLog reads an attempt's tests from its uploaded test log when it has one. A log that can't be fetched or read
// is an error, so the pass fails loud and is tried again; tests are never posted empty because a read failed.
func readTestLog(finished Finished, log func(run, sha256 string) ([]byte, error)) (Finished, error) {
	if finished.TestLog == nil || log == nil {
		return finished, nil
	}
	content, err := log(finished.TestLog.Run, finished.TestLog.Sha256)
	if err != nil {
		return Finished{}, fmt.Errorf("reading test log %s of run %s: %w", finished.TestLog.Sha256, finished.TestLog.Run, err)
	}
	return WithTestLog(finished, content)
}

// EventFabric reruns one unit alone and reads its stream.
type EventFabric struct {
	// Rerun places the unit alone on the tree and returns its events: coordinator.RerunAlone on Planner's JobUnit for
	// (unitKey, tree), uncached, at RerunPriority.
	Rerun func(unitKey, tree string) ([]protocol.Event, error)
	Log   func(run, sha256 string) ([]byte, error) // as EventRuns.Log
}

// RerunAlone reruns the unit and reads its attempt. The rerun is a one-unit job, so its whole stream is the unit's;
// its unit id is Planner's JobUnitFor id for that tree, which differs from the unitKey on main's base, so the stream
// isn't filtered by key. A rerun that never reported is silent infra.
func (fabric EventFabric) RerunAlone(unitKey, tree string) (Finished, error) {
	events, err := fabric.Rerun(unitKey, tree)
	if err != nil {
		return Finished{}, err
	}
	finished, found := FinishedFromEvents(events)
	if !found {
		return Finished{Attempt: Attempt{Status: Broken}, Infra: InfraSilent}, nil
	}
	return readTestLog(finished, fabric.Log)
}

func eventsOf(events []protocol.Event, unitKey string) []protocol.Event {
	mine := []protocol.Event{}
	for _, event := range events {
		if event.Unit == unitKey {
			mine = append(mine, event)
		}
	}
	return mine
}
