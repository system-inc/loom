// Package placer places every planned future's attempt on Loom's pools (`loom place`), in place of the shell script on
// Workshop deleted Oct 10: Queue lists a future planned and undecided with the attempt it waits on, and nothing else
// ever queues that attempt's units, so without a placer every future is voided silent at the judge's backstop and
// listed again, forever.
//
// For each listed attempt it hasn't placed, it turns every unit the attempt runs (not reused, not carried from an
// earlier attempt by the judge's own rule) into a job (planner.FutureJobUnit, every kind), picks the pools that take
// the unit (its kind, its key's runner, its declared need, every toolchain its key names) and starts one coordinator
// run, coordinator.FutureRun(tree, attempt), the run the judge reads. The ledger records each attempt before anything
// starts, so a restart never places one twice. An attempt with a unit it can't place starts nothing and is posted void
// as Loom's (neverPlaced), naming each unit and why: a future is never left silent, and never runs without a unit.
package placer

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/system-inc/loom/coordinator"
	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

// A Pool is one pool of the pool table (judge.PoolEntry, workshop's ~/.loom/pools.json) with the toolchains every
// worker of it has, which the table doesn't hold: the judge's --pool-has, given to the placer the same way.
type Pool struct {
	judge.PoolEntry
	Has []string
}

// A RunPool is a pool a placement's run uses, with how many of its units may be queued there at once.
type RunPool struct {
	Pool
	Slots int
}

// A PlacedUnit is one unit a placement queues and the pools it may go to.
type PlacedUnit struct {
	UnitKey string   `json:"unitKey"`
	Name    string   `json:"name"`
	Kind    string   `json:"kind"`
	Pools   []string `json:"pools"`
}

// A Placement is one future attempt's coordinator run, as the placer starts it.
type Placement struct {
	Future  string
	Attempt int
	Run     string
	Job     protocol.Job
	Pools   []RunPool
	Units   []PlacedUnit
}

// A Record is one ledger line: what the placer did with one attempt of one future, written before it starts anything.
// Placed names every unit it queued; Void is the cause it posted when it couldn't place every unit, and Unplaced each
// unit and why. An attempt with nothing to run (every unit reused or carried) is recorded with neither.
type Record struct {
	Future   string       `json:"future"`
	Attempt  int          `json:"attempt"`
	Run      string       `json:"run"`
	At       string       `json:"at"`
	Placed   []PlacedUnit `json:"placed,omitempty"`
	Reused   int          `json:"reused,omitempty"`
	Carried  int          `json:"carried,omitempty"`
	Unplaced []string     `json:"unplaced,omitempty"`
	Void     string       `json:"void,omitempty"`
}

// A Ledger holds what the placer did, by future and attempt, across restarts.
type Ledger interface {
	Find(future string, attempt int) (Record, bool)
	// LastVoid is the newest record of the future that posted a void, any attempt.
	LastVoid(future string) (Record, bool)
	Append(record Record) error
}

// Placer places every listed attempt once.
type Placer struct {
	Source judge.FutureSource // Queue's planned listing (judge.HTTPFutures)
	// Carried is the units the judge carries into the attempt from earlier ones (judge.Puller.CarriedFrom, the one
	// definition `loom judge carried` prints): they are judged from the earlier run and never placed again.
	Carried func(future judge.PlannedFuture, attempt int) ([]judge.CarriedUnit, error)
	// ChangePaths is a change's paths from its record (GET /changes/<change>), asked only for a future with a unit
	// keyed on ADAMIC_GATE_CHANGED.
	ChangePaths func(change string) ([]string, error)
	// Pools is the pool table with each pool's toolchains, read every pass so a resized pool is seen without a restart.
	Pools func() ([]Pool, error)
	// Needs is the gate tools' unit-needs.json (the judge's loadNeeds), asked at most once a pass and only for a unit
	// whose plan carried no resources, read the way the judge's reruns read it (judge.NeedOf).
	Needs func() (planner.UnitNeeds, error)
	// WarmRunners are the runner sha256s whose workers keep a shared Go cache: a test unit keyed on one goes only to a
	// pool marked cold, as the judge's reruns do, or the judge voids it warmCache.
	WarmRunners map[string]bool
	// Start starts the placement's coordinator run and returns once it's started, never waiting on its verdict.
	Start func(Placement) error
	// Void posts the listed attempt void as Loom's, neverPlaced, with the cause (judge.Puller.VoidListed).
	Void func(future judge.PlannedFuture, attempt int, cause string) error
	// Ledger is what was placed, by future and attempt (FileLedger), so a restart never places twice.
	Ledger Ledger
	// PoolSlots is the most units of one run queued on one pool at once; zero means every unit that may go there.
	PoolSlots int
	// UnfitEvery is how soon a future voided for a cause is voided again for the same one: an attempt that fails the
	// same way waits until then, so a future no pool serves is voided every UnfitEvery, under the judge's 45-minute
	// backstop, never every pass. Zero voids every attempt at once.
	UnfitEvery time.Duration
	Now        func() time.Time
	Log        io.Writer
	// noted are the attempts already logged as waiting (empty or held), so each is said once.
	noted map[string]bool
}

// PlaceOnce places every listed attempt it hasn't recorded and returns how many runs it started. One future's error
// never stops the others; the errors come back joined, and a future that erred is tried again next pass.
func (placer *Placer) PlaceOnce() (int, error) {
	futures, err := placer.Source.Planned()
	if err != nil {
		return 0, err
	}
	pools, err := placer.Pools()
	if err != nil {
		return 0, fmt.Errorf("the pool table: %w", err)
	}
	pass := &pass{placer: placer, pools: pools}
	started, failures := 0, []string{}
	for _, future := range futures {
		placed, err := pass.placeOne(future)
		if err != nil {
			failures = append(failures, fmt.Sprintf("future %s: %v", future.Future, err))
		}
		if placed {
			started++
		}
	}
	if len(failures) > 0 {
		return started, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return started, nil
}

// pass is one PlaceOnce: the pool table it read, and the unit needs once it has read them.
type pass struct {
	placer *Placer
	pools  []Pool
	needs  *planner.UnitNeeds
}

// candidate is one unit the attempt runs, as a job, with the pools that take it.
type candidate struct {
	unit judge.PlannedUnitWire
	job  protocol.JobUnit
	fit  []string
}

func (placer *Placer) note(key, line string) {
	if placer.noted == nil {
		placer.noted = map[string]bool{}
	}
	if !placer.noted[key] {
		placer.noted[key] = true
		fmt.Fprintln(placer.Log, line)
	}
}

// placeOne places one listed future's attempt, and says whether it started a run.
func (pass *pass) placeOne(future judge.PlannedFuture) (bool, error) {
	placer := pass.placer
	attempt := max(future.Attempt, 1)
	run := coordinator.FutureRun(future.Future, attempt)
	if _, done := placer.Ledger.Find(future.Future, attempt); done {
		return false, nil
	}
	if future.Empty {
		// A docs-only future, planned empty: the docs lane decides it (judge.pullOne leaves it too), nothing runs.
		placer.note(run, fmt.Sprintf("%s: planned empty, nothing to place (the docs lane's)", run))
		return false, nil
	}
	carriedUnits, err := placer.Carried(future, attempt)
	if err != nil {
		return false, fmt.Errorf("the units carried into attempt %d: %w", attempt, err)
	}
	carried := map[string]bool{}
	for _, unit := range carriedUnits {
		carried[unit.UnitKey] = true
	}
	record := Record{Future: future.Future, Attempt: attempt, Run: run}
	candidates := []candidate{}
	var changed []string
	for _, unit := range future.Units {
		switch {
		case unit.Decision == "reuse":
			record.Reused++
			continue
		case carried[unit.UnitKey]:
			record.Carried++
			continue
		}
		var parts planner.KeyParts
		if err := json.Unmarshal(unit.KeyParts, &parts); err != nil {
			record.Unplaced = append(record.Unplaced, fmt.Sprintf("%s: its keyParts: %v", unitName(unit, ""), err))
			continue
		}
		if parts.Env["ADAMIC_GATE_CHANGED"] != "" && changed == nil {
			if changed, err = placer.ChangePaths(future.Change.Change); err != nil {
				return false, fmt.Errorf("change %s's paths: %w", future.Change.Change, err)
			}
			if changed == nil {
				changed = []string{}
			}
		}
		job, err := planner.FutureJobUnit(unit.UnitKey, parts, future.Future, future.Base, changed)
		if err != nil {
			record.Unplaced = append(record.Unplaced, fmt.Sprintf("%s: %v", unitName(unit, parts.Kind), err))
			continue
		}
		if job.Resources, err = pass.need(unit); err != nil {
			return false, err
		}
		fit, why := pass.fit(parts, job)
		if len(fit) == 0 {
			record.Unplaced = append(record.Unplaced, fmt.Sprintf("%s: %s", unitName(unit, parts.Kind), why))
			continue
		}
		candidates = append(candidates, candidate{unit: unit, job: job, fit: fit})
	}
	record.Unplaced = append(record.Unplaced, pass.pin(candidates)...)
	record.At = placer.Now().UTC().Format(time.RFC3339)
	if len(record.Unplaced) > 0 {
		return false, pass.void(future, attempt, record, true)
	}
	if len(candidates) == 0 {
		// Every unit reused or carried: the judge decides the attempt from the earlier runs, with nothing to run.
		fmt.Fprintf(placer.Log, "%s: nothing to place, %d reused and %d carried\n", run, record.Reused, record.Carried)
		return false, placer.Ledger.Append(record)
	}
	placement := Placement{Future: future.Future, Attempt: attempt, Run: run, Job: protocol.Job{Name: "future-" + future.Future[:12]}}
	slots := map[string]int{}
	for _, candidate := range candidates {
		placement.Job.Units = append(placement.Job.Units, candidate.job)
		placement.Units = append(placement.Units, PlacedUnit{UnitKey: candidate.unit.UnitKey, Name: candidate.unit.Name, Kind: candidate.job.Kind, Pools: candidate.fit})
		for _, name := range candidate.fit {
			slots[name]++
		}
	}
	if _, err := protocol.Expand(placement.Job); err != nil {
		return false, fmt.Errorf("attempt %d's job: %w", attempt, err)
	}
	for _, pool := range pass.pools {
		if count := slots[pool.Name]; count > 0 {
			if placer.PoolSlots > 0 {
				count = min(count, placer.PoolSlots)
			}
			placement.Pools = append(placement.Pools, RunPool{Pool: pool, Slots: count})
		}
	}
	record.Placed = placement.Units
	// Recorded first: a restart between the record and the start leaves the attempt unplaced, which the judge's
	// backstop voids, while a start before the record could place it twice.
	if err := placer.Ledger.Append(record); err != nil {
		return false, err
	}
	if err := placer.Start(placement); err != nil {
		// Recorded and never started: said now, as Loom's, never held, since the ledger already counts it placed.
		record.Unplaced = []string{"the coordinator run didn't start: " + err.Error()}
		return false, pass.void(future, attempt, record, false)
	}
	fmt.Fprintf(placer.Log, "placed %s: %d units on %s (%d reused, %d carried)\n", run, len(placement.Units), poolNames(placement.Pools), record.Reused, record.Carried)
	return true, nil
}

// void posts the attempt void as Loom's, naming every unit it couldn't place, and records it. When hold, the same cause
// again within UnfitEvery of the last waits: said once, voided when the window ends, so it never loops each pass.
func (pass *pass) void(future judge.PlannedFuture, attempt int, record Record, hold bool) error {
	placer := pass.placer
	cause := "not placed: " + strings.Join(record.Unplaced, "; ")
	if last, found := placer.Ledger.LastVoid(future.Future); hold && found && last.Void == cause {
		if at, err := time.Parse(time.RFC3339, last.At); err == nil && placer.Now().Sub(at) < placer.UnfitEvery {
			placer.note(record.Run, fmt.Sprintf("%s: held, %s as attempt %d was; voided again after %s", record.Run, cause, last.Attempt, at.Add(placer.UnfitEvery).Format(time.RFC3339)))
			return nil
		}
	}
	if err := placer.Void(future, attempt, cause); err != nil {
		return fmt.Errorf("posting attempt %d void (%s): %w", attempt, cause, err)
	}
	record.Placed, record.Void = nil, cause
	fmt.Fprintf(placer.Log, "void %s: %s\n", record.Run, cause)
	return placer.Ledger.Append(record)
}

// need is the unit's declared need, as the judge reads it for a rerun: the listing's, else unit-needs.json's.
func (pass *pass) need(unit judge.PlannedUnitWire) (protocol.Resources, error) {
	if unit.Resources.MemoryMegabytes > 0 || unit.Resources.Cpus > 0 {
		return unit.Resources, nil
	}
	if pass.needs == nil {
		needs, err := pass.placer.Needs()
		if err != nil {
			return protocol.Resources{}, fmt.Errorf("unit-needs.json: %w", err)
		}
		pass.needs = &needs
	}
	return judge.NeedOf(unit.Resources, unit.KeyParts, *pass.needs)
}

// fit is every pool that may run the job: it takes the unit's kind, serves the runner its key names, holds its
// declared need (judge.FitPools, the reruns' rule), is cold when the key's runner is warm, and has every toolchain the
// job requires. None comes back with why, naming what's missing.
func (pass *pass) fit(parts planner.KeyParts, job protocol.JobUnit) ([]string, string) {
	fit, unequipped := []string{}, []string{}
	for _, pool := range pass.pools {
		if len(judge.FitPools([]judge.PoolEntry{pool.PoolEntry}, parts.Kind, parts.Tools.Runner, job.Resources)) == 0 {
			continue
		}
		if parts.Kind == "test" && pass.placer.WarmRunners[parts.Tools.Runner] && !pool.Cold {
			continue
		}
		if missing := missingTools(pool, job.Requires); len(missing) > 0 {
			unequipped = append(unequipped, fmt.Sprintf("%s lacks %s", pool.Name, strings.Join(missing, ",")))
			continue
		}
		fit = append(fit, pool.Name)
	}
	if len(fit) > 0 {
		return fit, ""
	}
	why := fmt.Sprintf("no pool takes a %s unit on runner %.12s needing %s with %d MB and %d cpus", parts.Kind, parts.Tools.Runner,
		strings.Join(job.Requires, ","), job.Resources.MemoryMegabytes, job.Resources.Cpus)
	if pass.placer.WarmRunners[parts.Tools.Runner] && parts.Kind == "test" {
		why += " on a cold pool (its runner keeps a warm cache)"
	}
	if len(unequipped) > 0 {
		why += " (" + strings.Join(unequipped, "; ") + ")"
	}
	return nil, why
}

// pin narrows the run's pools so the coordinator, which places a unit on any pool of its run whose kinds, size and
// toolchains take it (it knows no runner and no cold mark), can only put each unit on a pool the placer chose for it.
// A pool that would take some unit it wasn't chosen for leaves the run, and a unit left with no pool is returned,
// with why: a run can't pin a unit to its own runner's pools while that pool is in it.
func (pass *pass) pin(candidates []candidate) []string {
	inRun := map[string]bool{}
	for _, candidate := range candidates {
		for _, name := range candidate.fit {
			inRun[name] = true
		}
	}
	dropped := map[string]string{}
	for _, pool := range pass.pools {
		if !inRun[pool.Name] {
			continue
		}
		for _, candidate := range candidates {
			if coordinatorTakes(pool, candidate.job) && !slices.Contains(candidate.fit, pool.Name) {
				inRun[pool.Name], dropped[pool.Name] = false, unitName(candidate.unit, candidate.job.Kind)
				break
			}
		}
	}
	unplaced := []string{}
	for index := range candidates {
		candidate := &candidates[index]
		kept := []string{}
		for _, name := range candidate.fit {
			if inRun[name] {
				kept = append(kept, name)
			}
		}
		if len(kept) == 0 {
			reasons := []string{}
			for _, name := range candidate.fit {
				reasons = append(reasons, fmt.Sprintf("%s would also take %s, which it doesn't serve", name, dropped[name]))
			}
			unplaced = append(unplaced, fmt.Sprintf("%s: no pool the run can pin it to (%s)", unitName(candidate.unit, candidate.job.Kind), strings.Join(reasons, "; ")))
		}
		candidate.fit = kept
	}
	return unplaced
}

// coordinatorTakes is the coordinator's own fit for a pool slot (coordinator.fitsUnit for a PoolMachine): the kinds it
// takes, its workers' memory and cpus, and its toolchains.
func coordinatorTakes(pool Pool, job protocol.JobUnit) bool {
	return pool.Takes(job.Kind) && (job.Resources.MemoryMegabytes <= 0 || pool.MemoryMegabytes >= job.Resources.MemoryMegabytes) &&
		(job.Resources.Cpus <= 0 || pool.Cpus >= job.Resources.Cpus) && len(missingTools(pool, job.Requires)) == 0
}

func missingTools(pool Pool, requires []string) []string {
	missing := []string{}
	for _, toolchain := range requires {
		if !slices.Contains(pool.Has, toolchain) {
			missing = append(missing, toolchain)
		}
	}
	return missing
}

// unitName is how a unit is named in a cause: its kind and its planned name, else its key.
func unitName(unit judge.PlannedUnitWire, kind string) string {
	name := unit.Name
	if name == "" {
		name = unit.UnitKey
	}
	if kind == "" {
		return name
	}
	return kind + " unit " + name
}

func poolNames(pools []RunPool) string {
	names := []string{}
	for _, pool := range pools {
		names = append(names, fmt.Sprintf("%s=%d", pool.Name, pool.Slots))
	}
	sort.Strings(names)
	return strings.Join(names, " ")
}
