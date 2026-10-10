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
	"time"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

// A PlannedFuture is one future Queue holds planned and undecided.
type PlannedFuture struct {
	Future  string            `json:"future"`
	Base    string            `json:"base"`
	Attempt int               `json:"attempt"` // the run attempt to read; 0 is read as 1
	Parity  bool              `json:"parity"`  // a parity run's future: tested on exactly its tree, never landed
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
	Log    func(run, sha256 string) ([]byte, error)                             // coordinator.ReadRunBlob, bound: each attempt's test log
	Main   MainRecords
	Queue  Queue
	Loop   Loop // its Now is used; its collaborators are set per future
	// Stale is the backstop (Loom, Oct 10 01:10Z): a run with a unit still open and no event for this long is posted
	// void as silent, so no future waits on a hand when a coordinator dies. Zero turns it off. The coordinator's own
	// finished events for the units it gives up on are the real fix; this only moves a stuck run toward void.
	Stale time.Duration
	// emptySince is when this process first saw each run with no events at all, the age of a run that never started;
	// a restart starts it over, which errs toward waiting.
	emptySince map[string]time.Time
}

// NewPuller is a Puller with its backstop's memory made.
func NewPuller(puller Puller) Puller {
	puller.emptySince = map[string]time.Time{}
	return puller
}

// StaleAfter is the backstop's line: over the 1800 s unit ceiling, with room for a slot to free.
const StaleAfter = 45 * time.Minute

// stale says why a run that hasn't finished every unit is stuck, or "" while it may still move: its newest event,
// never its first, is Stale old.
func (puller Puller) stale(run string, events []protocol.Event, open int) string {
	if puller.Stale <= 0 {
		return ""
	}
	now := puller.Loop.Now()
	var newest time.Time
	for _, event := range events {
		if at, err := time.Parse(time.RFC3339Nano, event.Time); err == nil && at.After(newest) {
			newest = at
		}
	}
	what := "no event"
	if newest.IsZero() {
		if puller.emptySince == nil {
			return ""
		}
		if _, seen := puller.emptySince[run]; !seen {
			puller.emptySince[run] = now
		}
		newest, what = puller.emptySince[run], "no event since the judge first read it"
	}
	if age := now.Sub(newest); age >= puller.Stale {
		return fmt.Sprintf("silent: %s for %d min with %d units open (the judge's %d-minute backstop)", what, int(age.Minutes()), open, int(puller.Stale.Minutes()))
	}
	return ""
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
		order, earlier, err := puller.earlier(future, attempt)
		if err != nil {
			return judged, err
		}
		if open := openUnits(future, events, order, earlier); open > 0 {
			if why := puller.stale(run, events, open); why != "" {
				loop, job := puller.jobOf(future, run, events)
				if _, err := loop.VoidFuture(job, InfraSilent, why); err != nil {
					return judged, fmt.Errorf("future %s: %w", future.Future, err)
				}
				judged++
			}
			continue
		}
		loop, job := puller.jobOf(future, run, events)
		loop.Runs, job.Earlier = EventRuns{Read: runsOf(run, events, earlier), Log: puller.Log}, order
		if _, err := loop.JudgeFuture(job); err != nil {
			return judged, fmt.Errorf("future %s: %w", future.Future, err)
		}
		judged++
	}
	return judged, nil
}

// VoidOne posts one listed future's run as void, naming cause (Loop.VoidFuture): attempt must be the attempt Queue
// lists, so a void is posted only for the run Queue is waiting on, and the next attempt is Queue's to list.
func (puller Puller) VoidOne(tree string, attempt int, cause string) (FuturePost, error) {
	futures, err := puller.Source.Planned()
	if err != nil {
		return FuturePost{}, err
	}
	for _, future := range futures {
		if future.Future != tree {
			continue
		}
		if listed := max(future.Attempt, 1); listed != attempt {
			return FuturePost{}, fmt.Errorf("future %s: Queue lists attempt %d, not %d", tree, listed, attempt)
		}
		run := puller.RunOf(future.Future, attempt)
		events, err := puller.Read(run)
		if err != nil {
			return FuturePost{}, fmt.Errorf("future %s: reading run %s: %w", tree, run, err)
		}
		loop, job := puller.jobOf(future, run, events)
		return loop.VoidFuture(job, InfraKill, cause)
	}
	return FuturePost{}, fmt.Errorf("future %s isn't listed planned and undecided", tree)
}

// jobOf is the loop and job that judge one listed future from its run's events.
func (puller Puller) jobOf(future PlannedFuture, run string, events []protocol.Event) (Loop, Job) {
	parts := map[string]json.RawMessage{}
	plan := []PlanUnit{}
	for _, unit := range future.Units {
		parts[unit.UnitKey] = unit.KeyParts
		planUnit := PlanUnit{UnitKey: unit.UnitKey}
		var parts struct {
			Select struct {
				Run string `json:"run"`
			} `json:"select"`
		}
		if json.Unmarshal(unit.KeyParts, &parts) == nil {
			planUnit.Named, _ = planner.RunNames(parts.Select.Run)
		}
		if unit.Decision == "reuse" {
			planUnit.Reused = "reused"
			if unit.Reused != nil && *unit.Reused != "" {
				planUnit.Reused = *unit.Reused
			}
		}
		plan = append(plan, planUnit)
	}
	loop := puller.Loop
	loop.Runs = EventRuns{Read: func(string) ([]protocol.Event, error) { return events, nil }, Log: puller.Log}
	loop.Fabric = EventFabric{Rerun: func(unitKey, tree string) ([]protocol.Event, error) {
		return puller.Rerun(parts[unitKey], tree)
	}, Log: puller.Log}
	loop.Main, loop.Queue = puller.Main, puller.Queue
	record := ChangeRecord{Change: future.Change.Change, Sha: future.Change.Sha, Base: future.Change.Base, Owner: future.Change.Owner}
	return loop, Job{Record: record, Change: record.Change, Future: future.Future, Base: future.Base, Run: run, Plan: plan}
}

// A CarriedUnit is a unit the judge carries into a future's attempt, and the earlier run of it that passed it.
type CarriedUnit struct {
	UnitKey string
	Run     string
}

// carried is every unit of the future the judge carries from its earlier attempts (order, newest first): the newest
// earlier attempt whose runner status is passed, which means a finished passed event and no signal or deadline on its
// exit. It is the one definition: openUnits counts these done, the loop judges them from that run, and
// `loom judge carried` prints them for Fabric's placer, which places every other planned unit.
func carried(future PlannedFuture, order []string, earlier map[string][]protocol.Event) []CarriedUnit {
	units := []CarriedUnit{}
	for _, unit := range future.Units {
		if unit.Decision == "reuse" {
			continue
		}
		for _, run := range order {
			if prior, found := FinishedFromEvents(eventsOf(earlier[run], unit.UnitKey)); found && prior.Attempt.Status == Passed {
				units = append(units, CarriedUnit{UnitKey: unit.UnitKey, Run: run})
				break
			}
		}
	}
	return units
}

// Carried lists the units the judge will carry into attempt of the listed future tree, and their source runs.
func (puller Puller) Carried(tree string, attempt int) ([]CarriedUnit, error) {
	futures, err := puller.Source.Planned()
	if err != nil {
		return nil, err
	}
	for _, future := range futures {
		if future.Future != tree {
			continue
		}
		order, earlier, err := puller.earlier(future, attempt)
		if err != nil {
			return nil, err
		}
		return carried(future, order, earlier), nil
	}
	return nil, fmt.Errorf("future %s isn't listed planned and undecided", tree)
}

// earlier reads a future's attempts before attempt, newest first: only these are ever carried from.
func (puller Puller) earlier(future PlannedFuture, attempt int) ([]string, map[string][]protocol.Event, error) {
	earlier := map[string][]protocol.Event{}
	order := []string{}
	for prior := attempt - 1; prior >= 1; prior-- {
		run := puller.RunOf(future.Future, prior)
		events, err := puller.Read(run)
		if err != nil {
			return nil, nil, fmt.Errorf("future %s: reading run %s: %w", future.Future, run, err)
		}
		earlier[run], order = events, append(order, run)
	}
	return order, earlier, nil
}

// openUnits is how many units the future runs that have no finished event in its run and aren't carried.
func openUnits(future PlannedFuture, events []protocol.Event, order []string, earlier map[string][]protocol.Event) int {
	finished := map[string]bool{}
	for _, event := range events {
		if event.Type == "finished" {
			finished[event.Unit] = true
		}
	}
	for _, unit := range carried(future, order, earlier) {
		finished[unit.UnitKey] = true
	}
	open := 0
	for _, unit := range future.Units {
		if unit.Decision != "reuse" && !finished[unit.UnitKey] {
			open++
		}
	}
	return open
}

// runsOf reads the current run's events or an earlier attempt's, by run id; any other run has none.
func runsOf(run string, events []protocol.Event, earlier map[string][]protocol.Event) func(string) ([]protocol.Event, error) {
	return func(asked string) ([]protocol.Event, error) {
		if asked == run {
			return events, nil
		}
		return earlier[asked], nil
	}
}
