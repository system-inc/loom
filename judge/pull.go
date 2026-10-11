package judge

// The judge's pull loop (#82tz9ty), beside Planner's: it lists the futures Queue holds planned and undecided, reads
// each one's run, and judges a future once every unit it runs has finished. The listing is the shape proposed to
// Queue at 23:53Z, Oct 9 (GET /futures?state=planned); until Queue lands it, the command's source is whatever answers
// that route.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

// A PlannedFuture is one future Queue holds planned and undecided.
type PlannedFuture struct {
	Future  string `json:"future"`
	Base    string `json:"base"`
	Attempt int    `json:"attempt"` // the run attempt to read; 0 is read as 1
	// FirstAttempt is the future's own first attempt when an earlier future of the same tree (a witness of the same sha
	// again) ran the ones before it: those are never carried from. 0 is read as 1.
	FirstAttempt int               `json:"firstAttempt"`
	Parity       bool              `json:"parity"` // a parity run's future: tested on exactly its tree, never landed
	Empty        bool              `json:"empty"`  // a docs-only future planned with no units, the docs lane's to decide
	Change       PlannedChange     `json:"change"`
	Units        []PlannedUnitWire `json:"units"`
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
	// Resources is the unit's declared need (Planner's unit-needs), placement only, outside the key: a rerun alone is
	// placed by it as the first placement was, or it lands on a tier that can't hold it.
	Resources protocol.Resources `json:"resources"`
	// Tree is the tree key of Workshop's build of the future's tree (planner.PlannedResult.Tree), on a test, product or
	// phase unit: what the placer names on its job, what a rerun on the future runs, and what `loom build-trees` builds.
	// Empty: the plan carried none.
	Tree string `json:"tree,omitempty"`
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

// NoMainRecords says main has no record, so a failure main shares is never excused as main's red: it errs toward red,
// never toward green. Tests use it where main's record isn't the question; the live judge reads LogMainRecords.
type NoMainRecords struct{}

// Latest says main has no record.
func (NoMainRecords) Latest(base string, unit PlanUnit) ([]TestOutcome, bool, error) {
	return nil, false, nil
}

// Puller judges every ready future one pass at a time.
type Puller struct {
	Source FutureSource
	RunOf  func(tree string, attempt int) string      // coordinator.FutureRun
	Read   func(run string) ([]protocol.Event, error) // coordinator.ReadRunEvents, bound
	// Rerun is planner.JobUnitFor with the unit's resources, then coordinator.RerunAlone. tree is the key of the build
	// the rerun runs (#v03v751): the planned unit's when sha is the future's, empty when it's the base's, which the
	// caller keys itself (planner.ReadCommitIdentity), a rerun on a base whose tree isn't built being void, never red.
	Rerun func(keyParts json.RawMessage, resources protocol.Resources, sha, tree string) ([]protocol.Event, error)
	Log   func(run, sha256 string) ([]byte, error) // coordinator.ReadRunBlob, bound: each attempt's test log
	// NeedNow is a unit's declared need as of now (NeedOf over a fresh unit-needs.json); nil skips the need-changed
	// rule.
	NeedNow func(keyParts json.RawMessage, listed protocol.Resources) (protocol.Resources, error)
	Main    MainRecords
	Queue   Queue
	Loop    Loop // its Now is used; its collaborators are set per future
	// Stale is the backstop (Loom, Oct 10 01:10Z): a run with a unit still open and no event for this long is posted
	// void as silent, so no future waits on a hand when a coordinator dies. Zero turns it off. The coordinator's own
	// finished events for the units it gives up on are the real fix; this only moves a stuck run toward void.
	Stale time.Duration
	// Rows, when set, keeps each decided run's unit rows (rows.go), each placed when Placed says; Report hears a row
	// that couldn't be kept. Nil keeps none.
	Rows   Rows
	Placed PlacedOf
	Report func(string)
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
// posted. A future still running is left for the next pass. One future's error never stops the others: they are judged,
// and the errors come back joined.
func (puller Puller) PullOnce() (int, error) {
	futures, err := puller.Source.Planned()
	if err != nil {
		return 0, err
	}
	judged, failures := 0, []error{}
	for _, future := range futures {
		posted, err := puller.pullOne(future)
		if err != nil {
			failures = append(failures, fmt.Errorf("future %s: %w", future.Future, err))
		}
		if posted {
			judged++
		}
	}
	return judged, errors.Join(failures...)
}

// pullOne judges one listed future if it's ready, and says whether it posted.
func (puller Puller) pullOne(future PlannedFuture) (bool, error) {
	if future.Empty {
		// A docs-only future, planned empty (Queue 98e66e3): the docs lane decides it by rule ruled-gate-docs-v0
		// (#w2mmfjd), never judge-v1, which Queue refuses. Until that lane lands, it's left for the old path.
		return false, nil
	}
	attempt := future.Attempt
	if attempt < 1 {
		attempt = 1
	}
	run := puller.RunOf(future.Future, attempt)
	events, err := puller.Read(run)
	if err != nil {
		return false, fmt.Errorf("reading run %s: %w", run, err)
	}
	order, earlier, err := puller.earlier(future, attempt)
	if err != nil {
		return false, err
	}
	if open := openUnits(future, events, order, earlier, puller.Loop.Warm); open > 0 {
		why := puller.stale(run, events, open)
		if why == "" {
			return false, nil
		}
		loop, job := puller.jobOf(future, run, events)
		if _, err := loop.VoidFuture(job, InfraSilent, why); err != nil {
			return false, err
		}
		puller.keepRows(future, attempt, run, events)
		return true, nil
	}
	loop, job := puller.jobOf(future, run, events)
	loop.Runs, job.Earlier = EventRuns{Read: runsOf(run, events, earlier), Log: puller.Log}, order
	if _, err = loop.JudgeFuture(job); errors.Is(err, ErrWaiting) {
		// A rerun waits on its base's tree: nothing was posted or rerun, and a later pass judges it.
		return false, nil
	} else if err != nil {
		return false, err
	}
	puller.keepRows(future, attempt, run, events)
	return true, nil
}

// VoidOne posts one listed future's run as void with the infra kind and cause given (Loop.VoidFuture): attempt must be
// the attempt Queue lists, so a void is posted only for the run Queue is waiting on, and the next attempt is Queue's to
// list.
func (puller Puller) VoidOne(tree string, attempt int, infra, cause string) (FuturePost, error) {
	futures, err := puller.Source.Planned()
	if err != nil {
		return FuturePost{}, err
	}
	for _, future := range futures {
		if future.Future == tree {
			return puller.VoidListed(future, attempt, infra, cause)
		}
	}
	return FuturePost{}, fmt.Errorf("future %s isn't listed planned and undecided", tree)
}

// VoidListed is VoidOne for a future already read from the listing: the placer voids an attempt it can't place
// (InfraNeverPlaced) from the listing it placed from, without listing again.
func (puller Puller) VoidListed(future PlannedFuture, attempt int, infra, cause string) (FuturePost, error) {
	if listed := max(future.Attempt, 1); listed != attempt {
		return FuturePost{}, fmt.Errorf("future %s: Queue lists attempt %d, not %d", future.Future, listed, attempt)
	}
	run := puller.RunOf(future.Future, attempt)
	events, err := puller.Read(run)
	if err != nil {
		return FuturePost{}, fmt.Errorf("future %s: reading run %s: %w", future.Future, run, err)
	}
	loop, job := puller.jobOf(future, run, events)
	post, err := loop.VoidFuture(job, infra, cause)
	if err == nil {
		puller.keepRows(future, attempt, run, events)
	}
	return post, err
}

// jobOf is the loop and job that judge one listed future from its run's events.
func (puller Puller) jobOf(future PlannedFuture, run string, events []protocol.Event) (Loop, Job) {
	parts := map[string]json.RawMessage{}
	resources := map[string]protocol.Resources{}
	trees := map[string]string{}
	plan := []PlanUnit{}
	for _, unit := range future.Units {
		parts[unit.UnitKey], resources[unit.UnitKey], trees[unit.UnitKey] = unit.KeyParts, unit.Resources, unit.Tree
		planUnit := planUnitOf(unit)
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
	loop.Fabric = EventFabric{Rerun: func(unitKey, sha string) ([]protocol.Event, error) {
		// The future's rerun runs its plan's build; the base's, keyed by the caller, its own.
		tree := ""
		if sha == future.Future {
			tree = trees[unitKey]
		}
		return puller.Rerun(parts[unitKey], resources[unitKey], sha, tree)
	}, Log: puller.Log}
	if puller.NeedNow != nil {
		loop.Need = func(unitKey string) (protocol.Resources, protocol.Resources, error) {
			need, err := puller.NeedNow(parts[unitKey], resources[unitKey])
			return resources[unitKey], need, err
		}
	}
	loop.Main, loop.Queue = puller.Main, puller.Queue
	record := ChangeRecord{Change: future.Change.Change, Sha: future.Change.Sha, Base: future.Change.Base, Owner: future.Change.Owner}
	return loop, Job{Record: record, Change: record.Change, Future: future.Future, Base: future.Base, Run: run, Plan: plan}
}

// planUnitOf reads a listed unit's kind, runner and named tests from its key parts.
func planUnitOf(unit PlannedUnitWire) PlanUnit {
	planUnit := PlanUnit{UnitKey: unit.UnitKey, Name: unit.Name}
	var parts struct {
		Kind  string `json:"kind"`
		Tools struct {
			Runner string `json:"runner"`
		} `json:"tools"`
		Select struct {
			Run string `json:"run"`
		} `json:"select"`
	}
	if json.Unmarshal(unit.KeyParts, &parts) == nil {
		planUnit.Named, _ = planner.RunNames(parts.Select.Run)
		planUnit.Kind, planUnit.Runner = parts.Kind, parts.Tools.Runner
	}
	return planUnit
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
//
// A pass warm says ran on a warm shared cache is never carried (Release, Oct 10 02:49Z): carried, Fabric's placer
// would leave it out, and the judge would read it warm and void the future, every attempt. Left out of the list, it's
// placed again, cold. An error asking warm counts as warm, so a unit is never carried on a guess.
func carried(future PlannedFuture, order []string, earlier map[string][]protocol.Event, warm func(string, PlanUnit, Attempt) (bool, error)) []CarriedUnit {
	units := []CarriedUnit{}
	for _, unit := range future.Units {
		if unit.Decision == "reuse" {
			continue
		}
		for _, run := range order {
			if prior, found := FinishedFromEvents(eventsOf(earlier[run], unit.UnitKey)); found && prior.Attempt.Status == Passed {
				if warm != nil {
					if isWarm, err := warm(run, planUnitOf(unit), prior.Attempt); err != nil || isWarm {
						continue
					}
				}
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
		return puller.CarriedFrom(future, attempt)
	}
	return nil, fmt.Errorf("future %s isn't listed planned and undecided", tree)
}

// CarriedFrom is Carried for a future already read from the listing, as the placer reads it.
func (puller Puller) CarriedFrom(future PlannedFuture, attempt int) ([]CarriedUnit, error) {
	order, earlier, err := puller.earlier(future, attempt)
	if err != nil {
		return nil, err
	}
	return carried(future, order, earlier, puller.Loop.Warm), nil
}

// earlier reads a future's attempts before attempt, back to its first, newest first: only these are ever carried from.
func (puller Puller) earlier(future PlannedFuture, attempt int) ([]string, map[string][]protocol.Event, error) {
	earlier := map[string][]protocol.Event{}
	order := []string{}
	for prior := attempt - 1; prior >= max(future.FirstAttempt, 1); prior-- {
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
func openUnits(future PlannedFuture, events []protocol.Event, order []string, earlier map[string][]protocol.Event, warm func(string, PlanUnit, Attempt) (bool, error)) int {
	finished := map[string]bool{}
	for _, event := range events {
		if event.Type == "finished" {
			finished[event.Unit] = true
		}
	}
	for _, unit := range carried(future, order, earlier, warm) {
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

// adamicModule is the import path prefix a unit key's package carries; unit-needs.json names packages by directory.
const adamicModule = "github.com/system-inc/adamic/"

// NeedOf is a unit's declared need for a rerun, read the way the placer reads it (Planner, Oct 10 02:02Z): the
// listing's resources when the plan carried them, else the gate tools' unit-needs.json by the unit's directory and run
// pattern (planner.UnitNeeds.Need). A need no pool holds is the rerun's placement's to refuse, as void.
func NeedOf(listed protocol.Resources, keyParts json.RawMessage, needs planner.UnitNeeds) (protocol.Resources, error) {
	if listed.MemoryMegabytes > 0 || listed.Cpus > 0 {
		return listed, nil
	}
	var parts struct {
		Package string `json:"package"`
		Select  struct {
			Run string `json:"run"`
		} `json:"select"`
	}
	if err := json.Unmarshal(keyParts, &parts); err != nil {
		return protocol.Resources{}, fmt.Errorf("a unit's keyParts: %w", err)
	}
	need := needs.Need(strings.TrimPrefix(parts.Package, adamicModule), parts.Select.Run)
	if need == nil {
		return protocol.Resources{}, nil
	}
	return *need, nil
}

// A PoolEntry is one pool of workshop's ~/.loom/pools.json (Fabric's, Oct 10 02:02Z): its name on the wire, its tier,
// the runner its workers serve, and each worker's memory and cpus. The placer and the judge's reruns read the same file.
type PoolEntry struct {
	Name            string `json:"name"`
	Tier            string `json:"tier"`
	Runner          string `json:"runner"`
	MemoryMegabytes int    `json:"memoryMegabytes"`
	Cpus            int    `json:"cpus"`
	// Kinds are the unit kinds the pool takes when it is limited to some (box-phase takes only phase, Fabric's Oct 10
	// 02:12Z): empty takes tests and products, as the planner reads it.
	Kinds []string `json:"kinds,omitempty"`
	// Cold marks a pool whose workers give each test unit an empty Go cache at its start (Release, Oct 10 02:48Z);
	// Machines are the machine names its workers report in started events, and ColdSince (RFC 3339, read from its
	// cold loop's first log line) is when that became true.
	Cold      bool     `json:"cold,omitempty"`
	Machines  []string `json:"machines,omitempty"`
	ColdSince string   `json:"coldSince,omitempty"`
	// MachinePrefixes name the workers of a pool whose instances come and go (Codex's, codex-<hostname>, #54pcx41): a
	// worker whose machine name starts with one is the pool's, as one its Machines list names.
	MachinePrefixes []string `json:"machinePrefixes,omitempty"`
	// Has is the toolchains every worker of the pool has (protocol.Toolchains): a unit whose key requires one is placed and
	// rerun only on a pool that has it. It lives here, beside the machines it is true of, and nowhere else: the placer and
	// the judge read it from this one file, so turning a pool on or off is one edit (#pzrz9r8; the units carried it as
	// --pool-has, and a hand-made drop-in had to change both).
	Has []string `json:"has,omitempty"`
}

// Names says whether a worker reporting machine in its started events is one of the pool's: Machines names it, or it
// starts with one of MachinePrefixes.
func (pool PoolEntry) Names(machine string) bool {
	return slices.Contains(pool.Machines, machine) || slices.ContainsFunc(pool.MachinePrefixes, func(prefix string) bool { return strings.HasPrefix(machine, prefix) })
}

// NamesAny says whether the pool names any machine at all, by name or by prefix.
func (pool PoolEntry) NamesAny() bool {
	return len(pool.Machines) > 0 || len(pool.MachinePrefixes) > 0
}

// Shares says whether two pools may name one worker: a machine one names the other names too, or two prefixes where one
// starts with the other (a worker named after the longer is named by both).
func (pool PoolEntry) Shares(other PoolEntry) bool {
	for _, machine := range pool.Machines {
		if other.Names(machine) {
			return true
		}
	}
	for _, machine := range other.Machines {
		if pool.Names(machine) {
			return true
		}
	}
	for _, prefix := range pool.MachinePrefixes {
		for _, otherPrefix := range other.MachinePrefixes {
			if strings.HasPrefix(prefix, otherPrefix) || strings.HasPrefix(otherPrefix, prefix) {
				return true
			}
		}
	}
	return false
}

// WarmUnit says why a unit's attempt counts as warm, empty when it counts: only a test unit keyed on one of
// warmRunners is judged by where it ran (WarmAttempt); every other unit counts as today.
func WarmUnit(pools []PoolEntry, warmRunners map[string]bool, unit PlanUnit, attempt Attempt) string {
	if (unit.Kind != "test" && unit.Kind != "build") || !warmRunners[unit.Runner] {
		return ""
	}
	return WarmAttempt(pools, attempt)
}

// ColdPools is the pools a rerun of a unit keyed on a warm runner may use: only those marked cold.
func ColdPools(pools []PoolEntry) []PoolEntry {
	return slices.DeleteFunc(slices.Clone(pools), func(pool PoolEntry) bool { return !pool.Cold })
}

// WarmAttempt says why an attempt counts as run on a warm cache, empty when it ran cold (Release, Oct 10 02:54Z):
// cold only if a pool marked cold names the attempt's machine, no pool not marked cold names it, and the attempt
// started at or after the latest coldSince among the pools naming it, since two pools can report one machine name
// (cloud-box-2 and cloud-box-3 both say Cloud). Anything unread or unnamed is warm: fail closed.
func WarmAttempt(pools []PoolEntry, attempt Attempt) string {
	started, err := time.Parse(time.RFC3339, attempt.StartedAt)
	if err != nil {
		return fmt.Sprintf("started time %q unreadable", attempt.StartedAt)
	}
	named := false
	var since time.Time
	for _, pool := range pools {
		if !pool.Names(attempt.Machine) {
			continue
		}
		if !pool.Cold {
			return fmt.Sprintf("machine %s is a worker of %s, not marked cold", attempt.Machine, pool.Name)
		}
		poolSince, err := time.Parse(time.RFC3339, pool.ColdSince)
		if err != nil {
			return fmt.Sprintf("pool %s is marked cold with no readable coldSince", pool.Name)
		}
		if poolSince.After(since) {
			since = poolSince
		}
		named = true
	}
	switch {
	case !named:
		return fmt.Sprintf("machine %q is named by no cold pool", attempt.Machine)
	case started.Before(since):
		return fmt.Sprintf("started %s, before machine %s ran cold at %s", attempt.StartedAt, attempt.Machine, since.Format(time.RFC3339))
	}
	return ""
}

// Takes is whether the pool takes a unit of this kind: a pool limited to some kinds takes only those, and an
// unlimited one takes every kind but phase, which goes only to a pool that names it.
func (pool PoolEntry) Takes(kind string) bool {
	if len(pool.Kinds) == 0 {
		return kind != "phase"
	}
	for _, taken := range pool.Kinds {
		if taken == kind {
			return true
		}
	}
	return false
}

// NeedGrew says how a declared need exceeds what a unit was placed with, empty when it doesn't. A listing without
// resources placed the unit with none declared, so any declared need now is more.
func NeedGrew(placed, need protocol.Resources) string {
	if need.Cpus <= placed.Cpus && need.MemoryMegabytes <= placed.MemoryMegabytes {
		return ""
	}
	return fmt.Sprintf("placed with %d cpus and %d MB, declared now %d cpus and %d MB", placed.Cpus, placed.MemoryMegabytes, need.Cpus, need.MemoryMegabytes)
}

// LoadPools reads pools.json, refusing a pool without a name, a runner, or positive memory and cpus.
func LoadPools(content []byte) ([]PoolEntry, error) {
	var table struct {
		Pools []PoolEntry `json:"pools"`
	}
	if err := json.Unmarshal(content, &table); err != nil {
		return nil, fmt.Errorf("pools.json: %w", err)
	}
	for _, pool := range table.Pools {
		if pool.Name == "" || pool.Runner == "" || pool.MemoryMegabytes <= 0 || pool.Cpus <= 0 {
			return nil, fmt.Errorf("pools.json: pool %+v needs a name, a runner and positive memoryMegabytes and cpus", pool)
		}
		if slices.Contains(pool.MachinePrefixes, "") {
			// An empty prefix would name every worker the pool's.
			return nil, fmt.Errorf("pools.json: pool %s has an empty machine prefix", pool.Name)
		}
		for _, toolchain := range pool.Has {
			if !slices.Contains(protocol.Toolchains, toolchain) {
				return nil, fmt.Errorf("pools.json: pool %s has %q, which isn't a toolchain (%s)", pool.Name, toolchain, strings.Join(protocol.Toolchains, ", "))
			}
		}
	}
	return table.Pools, nil
}

// FitPools is every pool a rerun of a unit may go to: it takes the unit's kind, it serves the runner the unit's key
// names (a unit runs only on its key's runner), and each worker holds the unit's declared need. None means the rerun can't be placed: void,
// with that cause, never silent.
func FitPools(pools []PoolEntry, kind, runner string, need protocol.Resources) []PoolEntry {
	fit := []PoolEntry{}
	for _, pool := range pools {
		if pool.Takes(kind) && (runner == "" || pool.Runner == runner) && pool.MemoryMegabytes >= need.MemoryMegabytes && pool.Cpus >= need.Cpus {
			fit = append(fit, pool)
		}
	}
	return fit
}
