package coordinator

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/system-inc/loom/protocol"
)

// RerunPriority is the tier a unit rerun alone is placed at: above every landing (40) and canary (50), since a red
// waits on it, and under the wire's MaximumPoolPriority, so there is headroom above it.
const RerunPriority = 900

// eventsPageSize is the most events the wire answers in one read of a run's log (wire/source/RunObject.ts).
const eventsPageSize = 1000

// FutureRun is the run id of the attempt-th run that tests a future, its tree's sha (attempts count from 1). A unit's
// id in that run is its unitKey, which is already a valid unit id, so nothing maps one to the other.
func FutureRun(tree string, attempt int) string {
	return fmt.Sprintf("future-%s-%d", tree, attempt)
}

// ReadRunEvents is a run's whole event log as the wire holds it, the record the coordinator wrote: each unit's
// started, output, exit, uploaded and finished events, in order. It reads page by page and stops at a short page,
// so a finished run's last read never waits on the wire for an event that won't come.
func ReadRunEvents(readContext context.Context, wire string, secret []byte, run string) ([]protocol.Event, error) {
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: run, Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		return nil, err
	}
	reader := &PoolMachine{Wire: wire}
	var events []protocol.Event
	after := int64(0)
	for {
		entries, err := reader.readEvents(readContext, run, token, after)
		for _, entry := range entries {
			events = append(events, entry.event)
			after = max(after, entry.position)
		}
		if err != nil {
			return events, err
		}
		if len(entries) < eventsPageSize {
			return events, nil
		}
	}
}

// ReadRunBlob is one blob a run's unit uploaded (an uploaded event's sha256), read with the run's coordinator token:
// the judge reads each unit's test log, loom-out/test.jsonl.gz, this way, since the runner doesn't stream it.
func ReadRunBlob(readContext context.Context, wire string, secret []byte, run string, sha256 string) ([]byte, error) {
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: run, Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		return nil, err
	}
	reader := &wireClient{url: wire, client: http.DefaultClient}
	return reader.call(readContext, http.MethodGet, "/runs/"+run+"/blobs/"+sha256, token, nil)
}

// RerunAlone runs one unit on its own, uncached, at RerunPriority on every pool the config places on: Judge's
// rerun of a failed unit on the candidate's tree and on main's base before it names a cause. The caller hands the
// unit already aimed at its tree, its id its unitKey. The config's machines are left as they were.
func RerunAlone(runContext context.Context, config Config, unit protocol.JobUnit) (Result, error) {
	name := unit.Id
	if len(name) > 12 {
		name = name[:12]
	}
	config.Uncached = true
	lifted := map[*PoolMachine]*PoolMachine{}
	slots := make([]Machine, len(config.Slots))
	for index, machine := range config.Slots {
		slots[index] = machine
		pool, isPool := machine.(*PoolMachine)
		if !isPool {
			continue
		}
		if lifted[pool] == nil {
			lifted[pool] = &PoolMachine{Pool: pool.Pool, Strict: pool.Strict, Priority: RerunPriority, Label: pool.Label, Wire: pool.Wire,
				Secret: pool.Secret, Version: pool.Version, GoPlatform: pool.GoPlatform, CoreCount: pool.CoreCount, Client: pool.Client,
				NeverStarted: pool.NeverStarted, QueueCheck: pool.QueueCheck, Has: pool.Has, Log: pool.Log}
		}
		slots[index] = lifted[pool]
	}
	config.Slots = slots
	return Run(runContext, config, protocol.Job{Name: "rerun-" + name, Units: []protocol.JobUnit{unit}})
}
