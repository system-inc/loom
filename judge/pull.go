package judge

// The judge's pull loop (#82tz9ty), beside Planner's: it lists the futures Queue holds planned and undecided, reads
// each one's run, and judges a future once every unit it runs has finished. The listing is the shape proposed to
// Queue at 23:53Z, Oct 9 (GET /futures?state=planned); until Queue lands it, the command's source is whatever answers
// that route.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/system-inc/loom/protocol"
)

// A PlannedFuture is one future Queue holds planned and undecided.
type PlannedFuture struct {
	Future  string            `json:"future"`
	Base    string            `json:"base"`
	Attempt int               `json:"attempt"` // the run attempt to read; 0 is read as 1
	Change  PlannedChange     `json:"change"`
	Units   []PlannedUnitWire `json:"units"`
}

// PlannedChange is the future's newest change's record, what a kick needs.
type PlannedChange struct {
	Change string `json:"change"`
	Sha    string `json:"sha"`
	Base   string `json:"base"`
	Owner  string `json:"owner"`
}

// PlannedUnitWire is a unit as Queue's unit.planned event holds it.
type PlannedUnitWire struct {
	UnitKey  string          `json:"unitKey"`
	Name     string          `json:"name"`
	KeyParts json.RawMessage `json:"keyParts"`
	Decision string          `json:"decision"` // run or reuse
	Reused   *string         `json:"reused"`
}

// FutureSource lists the futures waiting for a verdict.
type FutureSource interface {
	Planned() ([]PlannedFuture, error)
}

// HTTPFutures reads Queue's planned listing with the coordinator token.
type HTTPFutures struct {
	Base  string
	Token string
	HTTP  *http.Client
}

// Planned lists the futures Queue holds planned and undecided.
func (futures HTTPFutures) Planned() ([]PlannedFuture, error) {
	request, err := http.NewRequest("GET", strings.TrimSuffix(futures.Base, "/")+"/futures?state=planned", nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+futures.Token)
	client := futures.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		detail, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return nil, fmt.Errorf("GET /futures?state=planned: %s: %s", response.Status, strings.TrimSpace(string(detail)))
	}
	var listing struct {
		Futures []PlannedFuture `json:"futures"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		return nil, fmt.Errorf("GET /futures?state=planned: %w", err)
	}
	return listing.Futures, nil
}

// NoMainRecords is the named stand-in for main's recorded verdicts until Queue's verdict index answers by unit at a
// base: it says main has no record, so a failure main shares is never excused as main's red. That errs toward red,
// never toward green. Remove it when the index lookup lands.
type NoMainRecords struct{}

// Latest says main has no record.
func (NoMainRecords) Latest(base, unitKey string) ([]TestOutcome, bool, error) {
	return nil, false, nil
}

// Puller judges every ready future one pass at a time.
type Puller struct {
	Source FutureSource
	RunOf  func(tree string, attempt int) string                                // coordinator.FutureRun
	Read   func(run string) ([]protocol.Event, error)                           // coordinator.ReadRunEvents, bound
	Rerun  func(keyParts json.RawMessage, sha string) ([]protocol.Event, error) // planner.JobUnitFor, then coordinator.RerunAlone
	Main   MainRecords
	Queue  Queue
	Loop   Loop // its Now is used; its collaborators are set per future
}

// PullOnce judges every listed future whose run has a finished event for each unit it runs, and returns how many it
// posted. A future still running is left for the next pass.
func (puller Puller) PullOnce() (int, error) {
	futures, err := puller.Source.Planned()
	if err != nil {
		return 0, err
	}
	judged := 0
	for _, future := range futures {
		attempt := future.Attempt
		if attempt < 1 {
			attempt = 1
		}
		run := puller.RunOf(future.Future, attempt)
		events, err := puller.Read(run)
		if err != nil {
			return judged, fmt.Errorf("future %s: reading run %s: %w", future.Future, run, err)
		}
		if !finishedAll(future, events) {
			continue
		}
		parts := map[string]json.RawMessage{}
		plan := []PlanUnit{}
		for _, unit := range future.Units {
			parts[unit.UnitKey] = unit.KeyParts
			planUnit := PlanUnit{UnitKey: unit.UnitKey}
			if unit.Decision == "reuse" {
				planUnit.Reused = "reused"
				if unit.Reused != nil && *unit.Reused != "" {
					planUnit.Reused = *unit.Reused
				}
			}
			plan = append(plan, planUnit)
		}
		loop := puller.Loop
		loop.Runs = EventRuns{Read: func(string) ([]protocol.Event, error) { return events, nil }}
		loop.Fabric = EventFabric{Rerun: func(unitKey, tree string) ([]protocol.Event, error) {
			return puller.Rerun(parts[unitKey], tree)
		}}
		loop.Main, loop.Queue = puller.Main, puller.Queue
		record := ChangeRecord{Change: future.Change.Change, Sha: future.Change.Sha, Base: future.Change.Base, Owner: future.Change.Owner}
		if _, err := loop.JudgeFuture(Job{Record: record, Change: record.Change, Future: future.Future, Base: future.Base, Run: run, Plan: plan}); err != nil {
			return judged, fmt.Errorf("future %s: %w", future.Future, err)
		}
		judged++
	}
	return judged, nil
}

// finishedAll says whether every unit the future runs has a finished event in its run.
func finishedAll(future PlannedFuture, events []protocol.Event) bool {
	finished := map[string]bool{}
	for _, event := range events {
		if event.Type == "finished" {
			finished[event.Unit] = true
		}
	}
	for _, unit := range future.Units {
		if unit.Decision != "reuse" && !finished[unit.UnitKey] {
			return false
		}
	}
	return true
}
