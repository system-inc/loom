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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
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
// unit and why. An attempt with nothing to run (every unit reused or carried) is recorded with neither. The attempt's
// newest record stands for it.
type Record struct {
	Future   string       `json:"future"`
	Attempt  int          `json:"attempt"`
	Run      string       `json:"run"`
	At       string       `json:"at"`
	PlanHash string       `json:"planHash,omitempty"` // PlanHash of the plan the attempt was placed from
	Placed   []PlacedUnit `json:"placed,omitempty"`
	Reused   int          `json:"reused,omitempty"`
	Carried  int          `json:"carried,omitempty"`
	Unplaced []string     `json:"unplaced,omitempty"`
	Void     string       `json:"void,omitempty"`
	// Hold is the void's stable reason class, what a later void of the future is held by: never a log's tail or a run
	// id, which differ every attempt.
	Hold string `json:"hold,omitempty"`
	// StartFailed is why its run didn't start: the attempt counts as not placed, and is placed again until voided.
	StartFailed string `json:"startFailed,omitempty"`
	// Exit is how its loom run ended, when this placer saw it end; EarlyExit is the void's cause when it ended before
	// the run's first event, so nothing ran and the judge would wait out its backstop.
	Exit      string `json:"exit,omitempty"`
	EarlyExit string `json:"earlyExit,omitempty"`
}

// placed says the attempt is the placer's no more: started, voided, or with nothing to run.
func (record Record) placed() bool {
	return record.StartFailed == "" || record.Void != ""
}

// An Exit is a placed run's loom run ending, as the process that started it saw it.
type Exit struct {
	Future  string
	Attempt int
	Run     string
	Status  string // "exit 3", "signal: killed"
	Tail    string // its log's last lines, tokens taken out
}

// A Ledger holds what the placer did, by future and attempt, across restarts.
type Ledger interface {
	Find(future string, attempt int) (Record, bool)
	// LastVoid is the newest record of the future that posted a void, any attempt.
	LastVoid(future string) (Record, bool)
	Append(record Record) error
	// Compact keeps only the attempts keep says to.
	Compact(keep func(Record) bool) error
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
	// pool the judge's WarmAttempt would read as cold, or the judge voids it warmCache.
	WarmRunners map[string]bool
	// Start starts the placement's coordinator run and returns once it's started, never waiting on its verdict.
	Start func(Placement) error
	// Exits are the started runs' loom runs ending (startRun's), read each pass; RunStarted says whether a run has any
	// event, so a run that ended before its first one is voided now, not at the judge's backstop.
	Exits      <-chan Exit
	RunStarted func(run string) (bool, error)
	// Void posts the listed attempt void as Loom's, neverPlaced, with the cause (judge.Puller.VoidListed).
	Void func(future judge.PlannedFuture, attempt int, cause string) error
	// Ledger is what was placed, by future and attempt (FileLedger), so a restart never places twice.
	Ledger Ledger
	// PoolSlots is the most units of one run queued on one pool at once; zero means every unit that may go there.
	PoolSlots int
	// UnfitEvery is how soon a future voided for a cause is voided again for the same one: an attempt that fails the
	// same way waits until then, so a future no pool serves is voided every UnfitEvery, under the judge's 45-minute
	// backstop, never every pass. A future whose reads keep failing is voided after it too. Zero voids at once.
	UnfitEvery time.Duration
	// Keep is how long the ledger keeps an attempt of a future Queue no longer lists; zero keeps every one.
	Keep time.Duration
	Now  func() time.Time
	Log  io.Writer
	// noted are the attempts already logged as waiting (empty or held), so each is said once.
	noted map[string]bool
	// failing are the futures whose reads are failing, backed off between tries.
	failing map[string]*readFailure
	// exited are runs seen ending and not yet recorded, kept while a read of their run fails.
	exited      []Exit
	compactedAt time.Time
}

// readFailure is a future whose reads keep failing: since when, and when it's tried next.
type readFailure struct {
	first, next time.Time
	delay       time.Duration
}

// A readError is a future's read that failed (its carried units, its change's paths, the unit needs): the future is
// backed off and, past UnfitEvery, voided.
type readError struct{ err error }

func (read readError) Error() string { return read.err.Error() }
func (read readError) Unwrap() error { return read.err }

// errStopPass ends a pass: a run that wouldn't start likely won't for the next future either, so one start is tried a
// pass while the cause holds.
var errStopPass = errors.New("the pass stops at a run that didn't start")

// errEndPass ends a pass quietly after an early exit's void: nothing failed, but the next void waits a pass.
var errEndPass = errors.New("the pass ends after a void for a run that ended early")

var treePattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PlanHash is a plan's identity in the ledger: the sha256 of its units' keys and decisions, sorted. An attempt whose
// listed plan no longer hashes to its record's was planned again after it was placed.
func PlanHash(units []judge.PlannedUnitWire) string {
	lines := []string{}
	for _, unit := range units {
		lines = append(lines, unit.UnitKey+" "+unit.Decision)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}

// PlaceOnce places every listed attempt it hasn't recorded and returns how many runs it started. One future's error
// never stops the others, save a run that didn't start; the errors come back joined, and a future that erred is tried
// again next pass, or later when its reads keep failing.
func (placer *Placer) PlaceOnce() (int, error) {
	// Drained before any read, so ended runs never back up behind a Queue or pool table that won't answer.
	placer.drainExits()
	futures, err := placer.Source.Planned()
	if err != nil {
		return 0, err
	}
	pools, err := placer.Pools()
	if err != nil {
		return 0, fmt.Errorf("the pool table: %w", err)
	}
	pass := &pass{placer: placer, pools: pools}
	failures := placer.recordExits()
	started, listed := 0, map[string]bool{}
	for _, future := range futures {
		listed[future.Future] = true
		if failure := placer.failing[future.Future]; failure != nil && placer.Now().Before(failure.next) {
			continue
		}
		placed, err := pass.placeOne(future)
		if err == errEndPass {
			break
		}
		var read readError
		switch {
		case errors.As(err, &read):
			err = pass.readFailed(future, read)
		case err == nil:
			delete(placer.failing, future.Future)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("future %s: %v", future.Future, err))
		}
		if placed {
			started++
		}
		if errors.Is(err, errStopPass) {
			break
		}
	}
	if err := placer.compact(listed); err != nil {
		failures = append(failures, fmt.Sprintf("compacting the ledger: %v", err))
	}
	if len(failures) > 0 {
		return started, fmt.Errorf("%s", strings.Join(failures, "; "))
	}
	return started, nil
}

// drainExits takes every run ending waiting on Exits into exited, for recordExits.
func (placer *Placer) drainExits() {
	for {
		select {
		case exit := <-placer.Exits:
			placer.exited = append(placer.exited, exit)
		default:
			return
		}
	}
}

// recordExits records each run seen ending since the last pass, and marks one that ended before its run's first
// event, whose attempt is then voided by placeOne.
func (placer *Placer) recordExits() []string {
	failures, waiting := []string{}, []Exit{}
	for _, exit := range placer.exited {
		record, found := placer.Ledger.Find(exit.Future, exit.Attempt)
		if !found || record.Run != exit.Run || record.Exit != "" {
			continue
		}
		started, err := placer.RunStarted(exit.Run)
		if err != nil {
			failures, waiting = append(failures, fmt.Sprintf("run %s ended (%s), and reading it: %v", exit.Run, exit.Status, err)), append(waiting, exit)
			continue
		}
		record.Exit, record.At = exit.Status, placer.Now().UTC().Format(time.RFC3339)
		if !started {
			record.EarlyExit = fmt.Sprintf("its loom run ended (%s) before the run's first event: %s", exit.Status, exit.Tail)
		}
		if err := placer.Ledger.Append(record); err != nil {
			failures, waiting = append(failures, err.Error()), append(waiting, exit)
		}
	}
	placer.exited = waiting
	return failures
}

// compact drops, at most hourly, the ledger's attempts of futures Queue no longer lists once they're Keep old.
func (placer *Placer) compact(listed map[string]bool) error {
	now := placer.Now()
	if placer.Keep <= 0 || now.Sub(placer.compactedAt) < time.Hour {
		return nil
	}
	placer.compactedAt = now
	return placer.Ledger.Compact(func(record Record) bool {
		at, err := time.Parse(time.RFC3339, record.At)
		return listed[record.Future] || err != nil || now.Sub(at) < placer.Keep
	})
}

// pass is one PlaceOnce: the pool table it read, and the unit needs once it has read them.
type pass struct {
	placer *Placer
	pools  []Pool
	needs  *planner.UnitNeeds
	voided bool // the last void call posted, not held
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

// readFailed backs a future off after a failed read, a minute and doubling to ten, and once its reads have failed
// for UnfitEvery voids the listed attempt as Loom's, naming the read: a future is never left waiting silent on them.
func (pass *pass) readFailed(future judge.PlannedFuture, read readError) error {
	placer, now := pass.placer, pass.placer.Now()
	if placer.failing == nil {
		placer.failing = map[string]*readFailure{}
	}
	failure := placer.failing[future.Future]
	if failure == nil {
		failure = &readFailure{first: now, delay: time.Minute}
		placer.failing[future.Future] = failure
	} else {
		failure.delay = min(2*failure.delay, 10*time.Minute)
	}
	failure.next = now.Add(failure.delay)
	if now.Sub(failure.first) < placer.UnfitEvery {
		return read
	}
	attempt := max(future.Attempt, 1)
	record := Record{Future: future.Future, Attempt: attempt, Run: coordinator.FutureRun(future.Future, attempt), At: now.UTC().Format(time.RFC3339),
		PlanHash: PlanHash(future.Units), Unplaced: []string{fmt.Sprintf("its reads failed for %d min: %v", int(now.Sub(failure.first).Minutes()), read.err)}}
	if err := pass.void(future, attempt, record, ""); err != nil {
		return fmt.Errorf("%v; voiding it: %w", read, err)
	}
	delete(placer.failing, future.Future)
	return nil
}

// placeOne places one listed future's attempt, and says whether it started a run.
func (pass *pass) placeOne(future judge.PlannedFuture) (bool, error) {
	placer := pass.placer
	if !treePattern.MatchString(future.Future) {
		placer.note(future.Future, fmt.Sprintf("future %q isn't a tree sha: nothing placed", future.Future))
		return false, nil
	}
	attempt := max(future.Attempt, 1)
	run := coordinator.FutureRun(future.Future, attempt)
	if record, found := placer.Ledger.Find(future.Future, attempt); found && record.placed() {
		switch {
		case record.Void != "":
		case record.EarlyExit != "":
			// Held by its exit status alone, and the pass stops after one: runs that all end early (a broken loom, a
			// wire refusing every plan) void one future a pass and each future once a window, never every one each pass.
			record.Unplaced = []string{record.EarlyExit}
			if err := pass.void(future, attempt, record, "early exit: "+record.Exit); err != nil {
				return false, errors.Join(errStopPass, err)
			}
			if pass.voided {
				return false, errEndPass
			}
			return false, nil
		case record.PlanHash != "" && record.PlanHash != PlanHash(future.Units):
			// Queue planned it again after it was placed: the wire holds run's first plan, so only a fresh attempt runs
			// the new one.
			record.Unplaced = []string{fmt.Sprintf("its plan changed after %s was placed with another", run)}
			return false, pass.void(future, attempt, record, "")
		}
		return false, nil
	}
	if future.Empty {
		// A docs-only future, planned empty: the docs lane decides it (judge.pullOne leaves it too), nothing runs.
		placer.note(run, fmt.Sprintf("%s: planned empty, nothing to place (the docs lane's)", run))
		return false, nil
	}
	carriedUnits, err := placer.Carried(future, attempt)
	if err != nil {
		return false, readError{fmt.Errorf("the units carried into attempt %d: %w", attempt, err)}
	}
	carried := map[string]bool{}
	for _, unit := range carriedUnits {
		carried[unit.UnitKey] = true
	}
	record := Record{Future: future.Future, Attempt: attempt, Run: run, PlanHash: PlanHash(future.Units)}
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
				return false, readError{fmt.Errorf("change %s's paths: %w", future.Change.Change, err)}
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
			return false, readError{err}
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
		return false, pass.void(future, attempt, record, "unplaced: "+strings.ReplaceAll(strings.Join(record.Unplaced, "; "), run, "<run>"))
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
		// It never started, so the attempt is recorded as not placed and tried again next pass, and voided as Loom's,
		// held by cause like any other: a cause every start meets voids each future once a window, never in a storm.
		record.StartFailed = err.Error()
		if err := placer.Ledger.Append(record); err != nil {
			return false, errors.Join(errStopPass, err)
		}
		record.Unplaced = []string{"its coordinator run didn't start: " + err.Error()}
		if err := pass.void(future, attempt, record, "start: "+strings.ReplaceAll(record.StartFailed, run, "<run>")); err != nil {
			return false, errors.Join(errStopPass, err)
		}
		return false, fmt.Errorf("%w: %v", errStopPass, err)
	}
	fmt.Fprintf(placer.Log, "placed %s: %d units on %s (%d reused, %d carried)\n", run, len(placement.Units), poolNames(placement.Pools), record.Reused, record.Carried)
	return true, nil
}

// void posts the attempt void as Loom's, naming every unit it couldn't place, and records it. A void with a hold key
// (a stable reason class: the unplaced causes or start error with the run id taken out, an early exit's status) waits
// when the future's last void had the same key within UnfitEvery: said once, voided when the window ends, so it never
// loops each pass. An empty key never waits.
func (pass *pass) void(future judge.PlannedFuture, attempt int, record Record, hold string) error {
	placer := pass.placer
	pass.voided = false
	cause := "not placed: " + strings.Join(record.Unplaced, "; ")
	last, found := placer.Ledger.LastVoid(future.Future)
	if hold != "" && found && last.Hold == hold {
		if at, err := time.Parse(time.RFC3339, last.At); err == nil && placer.Now().Sub(at) < placer.UnfitEvery {
			placer.note(record.Run+" "+cause, fmt.Sprintf("%s: held, %s as attempt %d was; voided again after %s", record.Run, cause, last.Attempt, at.Add(placer.UnfitEvery).Format(time.RFC3339)))
			return nil
		}
	}
	if err := placer.Void(future, attempt, cause); err != nil {
		return fmt.Errorf("posting attempt %d void (%s): %w", attempt, cause, err)
	}
	record.Void, record.Hold, record.At, pass.voided = cause, hold, placer.Now().UTC().Format(time.RFC3339), true
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
		if parts.Kind == "test" && pass.placer.WarmRunners[parts.Tools.Runner] && !coldPool(pool, pass.pools, pass.placer.Now()) {
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

// coldPool says whether every worker of the pool would read as cold to judge.WarmAttempt for a unit starting now: the
// pool is marked cold, names its machines, and has a readable coldSince already past, and no pool naming one of its
// machines is unmarked or cold since later than now. Only a unit's own start, unknown here, is left to the judge.
func coldPool(pool Pool, pools []Pool, now time.Time) bool {
	if !pool.Cold || len(pool.Machines) == 0 {
		return false
	}
	for _, other := range pools {
		if other.Name != pool.Name && !slices.ContainsFunc(other.Machines, func(machine string) bool { return slices.Contains(pool.Machines, machine) }) {
			continue
		}
		since, err := time.Parse(time.RFC3339, other.ColdSince)
		if !other.Cold || err != nil || since.After(now) {
			return false
		}
	}
	return true
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
