package judge

// Adapters from Fabric's seams (Fabric to Judge, Oct 9, 23:49Z) to the loop's interfaces. Fabric lands
// coordinator.ReadRunEvents(ctx, wire, secret, run) and coordinator.RerunAlone(ctx, config, protocol.JobUnit) under
// those names; until then the loop's command hands these adapters stand-ins with the same shapes. A unit's id is its
// unitKey, and a future's run is coordinator.FutureRun(tree, attempt), so no mapping is kept here.

import "github.com/system-inc/loom/protocol"

// EventRuns reads a run's events and finds one unit's in them.
type EventRuns struct {
	Read func(run string) ([]protocol.Event, error) // coordinator.ReadRunEvents, bound to the wire and its token
}

// Finished reads the unit's last attempt in the run.
func (runs EventRuns) Finished(run, unitKey string) (Finished, bool, error) {
	events, err := runs.Read(run)
	if err != nil {
		return Finished{}, false, err
	}
	finished, found := FinishedFromEvents(eventsOf(events, unitKey))
	return finished, found, nil
}

// EventFabric reruns one unit alone and reads its stream.
type EventFabric struct {
	// Rerun places the unit alone on the tree and returns its events: coordinator.RerunAlone on Planner's JobUnit for
	// (unitKey, tree), uncached, at RerunPriority.
	Rerun func(unitKey, tree string) ([]protocol.Event, error)
}

// RerunAlone reruns the unit and reads its attempt. A rerun that never reported is silent infra.
func (fabric EventFabric) RerunAlone(unitKey, tree string) (Finished, error) {
	events, err := fabric.Rerun(unitKey, tree)
	if err != nil {
		return Finished{}, err
	}
	finished, found := FinishedFromEvents(eventsOf(events, unitKey))
	if !found {
		return Finished{Attempt: Attempt{Status: Broken}, Infra: InfraSilent}, nil
	}
	return finished, nil
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
