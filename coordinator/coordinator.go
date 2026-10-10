// Package coordinator is Loom's only verdict authority. It runs on our own boxes: it takes a job file,
// writes the plan, places units onto machine slots longest first, starts the runner on each, collects their
// events and decides the run with protocol.Decide. A unit missing or late makes the run void, never green.
// docs/protocol.md is the contract it keeps.
package coordinator

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/system-inc/loom/poster"
	"github.com/system-inc/loom/protocol"
)

// A Machine runs one unit at a time per slot: it starts the runner with the unit and writes the runner's
// event lines to events until the runner ends. Run returns when the runner has ended or runContext is
// cancelled; its error says why a runner couldn't be started or was lost, and is only ever reported, since
// what the unit proved is in its events.
type Machine interface {
	Name() string
	// RunnerVersion and Platform ("linux/amd64") are part of a unit's cache key on this machine.
	RunnerVersion() string
	Platform() string
	// Cores is the machine's whole CPU count, for the board.
	Cores() int
	Run(runContext context.Context, unit protocol.Unit, events io.Writer) error
}

// Config is one coordinator's world: where the wire is, the secret it signs with, and the slots it may use.
type Config struct {
	// Wire is the Worker's origin, such as https://loom-wire.kirk-ouimet.workers.dev.
	Wire string
	// Secret signs the run's tokens (protocol.ReadTokenSecret reads ~/.loom/token-secret).
	Secret []byte
	// Slots are where units run, one unit per slot at a time. A machine may appear more than once.
	Slots []Machine
	// Uncached reads no cache and writes none: every unit runs. What lands main runs uncached.
	Uncached bool
	// Durations holds each unit's last wall time, to place the longest first. Nil places in plan order.
	Durations *Durations
	// Log receives one line per thing that happens, for a person. Nil discards it.
	Log io.Writer
	// Run, when set, is the run's id in place of one made from the job's name and the time: a pipeline future's
	// FutureRun, so Judge reads its units under the id it expects. Empty makes one.
	Run string
	// RecordPlatform, when set ("linux/amd64"), is the platform whose verdict the run records: a unit that isn't portable
	// goes only to a machine of it, and one no machine of it can take is never placed, so the run is void. Empty places
	// every unit on any platform, as before.
	RecordPlatform string
	// LateGrace is how far past its timeout an attempt may run before it counts as dropped. Zero means 60 s.
	LateGrace time.Duration
	// PoolQueueWait is how long a pool unit may wait in the pool's queue before it starts. A pool unit's timeout and
	// grace run from its first event, not from when it was queued: on a shared pool a lower priority's unit waits
	// for workers, and a 90 s unit queued behind the star would otherwise be dropped before it ever ran (the budget
	// proof on Oct 9, 04:10Z: ten packed units dropped that way). Zero means two hours.
	PoolQueueWait time.Duration
	// Client makes every HTTP call. Nil means one with sane timeouts.
	Client *http.Client
	// SlotLimit says how many of a machine's slots Loom may use right now; nil means all of them. It is asked
	// every few seconds: a machine over its limit has its newest unit stopped and queued again (preempted, not
	// failed), and a limit of 0 holds units back until it rises. The gate takes slots back this way.
	SlotLimit func(machine string) int
}

// A Result is a run's outcome: its id, the verdict, and where a person can watch it.
type Result struct {
	Run     string
	Verdict protocol.Verdict
	Page    string // the live page, with a viewer token in it
	// Machines names every machine a unit ran on, for the rollout ledger.
	Machines []string
	// Events is the run's record, every event the verdict was decided from.
	Events []protocol.Event
}

// unitState is where one planned unit stands.
type unitState struct {
	planned   protocol.PlannedUnit
	status    string // "" while waiting or running, then the finished status, or "dropped" or "skipped"
	running   bool
	attempts  int
	preempt   context.CancelFunc // stops the running attempt to give its slot back
	preempted bool
	startedAt time.Time
	lastSlot  int // the slot of the last attempt, so a re-placement goes elsewhere when it can
	// brokenAgain counts the placements after a broken attempt, and brokeOn names the boxes that broke it, which never
	// get it again.
	brokenAgain int
	brokeOn     map[string]bool
}

// Run runs a job to its verdict. An error means the run couldn't be set up (no plan was posted, or an input
// isn't in the store); once the plan is posted every outcome is a verdict.
func Run(runContext context.Context, config Config, job protocol.Job) (Result, error) {
	if config.Log == nil {
		config.Log = io.Discard
	}
	if config.LateGrace == 0 {
		config.LateGrace = 60 * time.Second
	}
	if config.PoolQueueWait == 0 {
		config.PoolQueueWait = 2 * time.Hour
	}
	if config.Client == nil {
		config.Client = &http.Client{Timeout: 5 * time.Minute}
	}
	if len(config.Slots) == 0 {
		return Result{}, fmt.Errorf("the coordinator has no slots")
	}
	plan, err := protocol.Expand(job)
	if err != nil {
		return Result{}, err
	}
	run := config.Run
	if run == "" {
		run = RunId(job.Name, time.Now())
	} else if !protocol.RunIdPattern.MatchString(run) {
		return Result{}, fmt.Errorf("run id %q isn't one the wire takes", run)
	}
	expires := time.Now().Add(48 * time.Hour).Unix()
	mint := func(scope string) (string, error) {
		return protocol.MintToken(config.Secret, protocol.TokenClaims{Run: run, Scope: scope, Expires: expires})
	}
	coordinatorToken, err := mint(protocol.ScopeCoordinator)
	if err != nil {
		return Result{}, err
	}
	runnerToken, _ := mint(protocol.ScopeRunner)
	viewerToken, _ := mint(protocol.ScopeViewer)
	wire := &wireClient{url: config.Wire, client: config.Client}
	result := Result{Run: run, Page: strings.TrimSuffix(config.Wire, "/") + "/runs/" + run + "?token=" + viewerToken}

	// Every input must be in the store before anything runs: a unit that can't fetch one proves nothing.
	body := protocol.PlanOf(plan)
	for _, input := range body.Inputs {
		present, err := wire.hasBlob(runContext, run, coordinatorToken, input)
		if err != nil {
			return result, fmt.Errorf("checking input %s: %w", input, err)
		}
		if !present {
			return result, fmt.Errorf("input %s isn't in the store; upload it first", input)
		}
	}
	if err := wire.postPlan(runContext, run, coordinatorToken, body); err != nil {
		return result, fmt.Errorf("posting the plan: %w", err)
	}
	fmt.Fprintf(config.Log, "run %s: %d units on %d slots%s\n  %s\n", run, len(plan), len(config.Slots), map[bool]string{true: ", uncached", false: ""}[config.Uncached], result.Page)

	tellBoard(runContext, wire, coordinatorToken, config)

	relay := poster.New(strings.TrimSuffix(config.Wire, "/")+"/runs/"+run+"/events", coordinatorToken, config.Client, 250*time.Millisecond)
	relay.Report = func(message string) { fmt.Fprintf(config.Log, "wire: %s\n", message) }
	go relay.Loop()
	coordinator := &coordinator{
		config: config, job: job, run: run, wire: wire, coordinatorToken: coordinatorToken, runnerToken: runnerToken,
		record: newRecord(run, relay.Enqueue), machines: map[string]bool{},
	}
	coordinator.schedule(runContext, plan)

	if err := relay.Drain(time.Now().Add(2 * time.Minute)); err != nil {
		fmt.Fprintf(config.Log, "wire: not every event reached the wire (the verdict stands on the coordinator's record): %v\n", err)
	}
	result.Events = coordinator.record.snapshot()
	result.Verdict = protocol.Decide(run, body.Units, result.Events)
	for name := range coordinator.machines {
		result.Machines = append(result.Machines, name)
	}
	sort.Strings(result.Machines)
	if err := wire.postVerdict(context.WithoutCancel(runContext), run, coordinatorToken, result.Verdict); err != nil {
		fmt.Fprintf(config.Log, "wire: posting the verdict: %v\n", err)
	}
	fmt.Fprintf(config.Log, "run %s: %s\n", run, describe(result.Verdict))
	return result, nil
}

// tellBoard tells the board each machine's cores and how many of its slots this coordinator
// holds, so an idle slot shows as idle. The board is a view, so a failure is only logged.
func tellBoard(runContext context.Context, wire *wireClient, token string, config Config) {
	slots := map[string]int{}
	var machines []BoardMachine
	for _, machine := range config.Slots {
		if slots[machine.Name()] == 0 {
			machines = append(machines, BoardMachine{Name: machine.Name(), Cores: machine.Cores()})
		}
		slots[machine.Name()]++
	}
	for index := range machines {
		machines[index].Slots = slots[machines[index].Name]
	}
	if err := wire.postBoardMachines(runContext, token, machines); err != nil {
		fmt.Fprintf(config.Log, "board: posting the machines: %v\n", err)
	}
}

func describe(verdict protocol.Verdict) string {
	text := verdict.Status
	if len(verdict.Failed) > 0 {
		text += ", failed: " + strings.Join(verdict.Failed, ", ")
	}
	if len(verdict.Cached) > 0 {
		text += fmt.Sprintf(", %d from the cache", len(verdict.Cached))
	}
	for _, problem := range verdict.Problems {
		text += "\n  " + problem
	}
	return text
}

var runIdUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// RunId names a run after its job and start time, with four random bytes so two starts in one second differ.
func RunId(name string, start time.Time) string {
	base := strings.Trim(runIdUnsafe.ReplaceAllString(name, "-"), "-._")
	if base == "" {
		base = "run"
	}
	if len(base) > 80 {
		base = base[:80]
	}
	suffix := make([]byte, 4)
	rand.Read(suffix)
	return base + "-" + start.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(suffix)
}

type coordinator struct {
	config           Config
	job              protocol.Job
	run              string
	wire             *wireClient
	coordinatorToken string
	runnerToken      string
	record           *record

	mutex    sync.Mutex
	units    map[string]*unitState // by planned id
	order    []string              // planned ids in plan order
	free     []bool                // per slot
	wake     chan struct{}         // signalled when an attempt ends
	active   sync.WaitGroup
	machines map[string]bool // every machine an attempt ran on
}

// schedule places every unit and returns when none is running or can start.
func (coordinator *coordinator) schedule(runContext context.Context, plan []protocol.PlannedUnit) {
	coordinator.units = map[string]*unitState{}
	for _, planned := range plan {
		coordinator.units[planned.Id] = &unitState{planned: planned, lastSlot: -1, brokeOn: map[string]bool{}}
		coordinator.order = append(coordinator.order, planned.Id)
	}
	coordinator.free = make([]bool, len(coordinator.config.Slots))
	for index := range coordinator.free {
		coordinator.free[index] = true
	}
	coordinator.wake = make(chan struct{}, len(coordinator.config.Slots)+1)
	for {
		coordinator.mutex.Lock()
		coordinator.skipBlocked()
		started := coordinator.placeReady(runContext)
		running := 0
		for _, state := range coordinator.units {
			if state.running {
				running++
			}
		}
		waiting := 0
		for _, state := range coordinator.units {
			if state.status == "" && !state.running {
				waiting++
			}
		}
		coordinator.mutex.Unlock()
		// Done when nothing runs and nothing can start; with a slot limit, units may wait for slots to come back.
		if running == 0 && !started && (waiting == 0 || coordinator.config.SlotLimit == nil) {
			break
		}
		if runContext.Err() != nil && running == 0 {
			break
		}
		coordinator.mutex.Lock()
		coordinator.preemptOverLimit()
		coordinator.mutex.Unlock()
		select {
		case <-coordinator.wake:
		case <-time.After(3 * time.Second):
		case <-runContext.Done():
			// Running attempts see the same context and end; wait for them.
			coordinator.active.Wait()
		}
	}
	coordinator.active.Wait()
}

// ready says whether every unit a unit needs has passed; blocked says whether one never will. The caller
// holds the mutex.
func (coordinator *coordinator) needsOf(state *unitState) (ready bool, blocked string) {
	ready = true
	for _, other := range coordinator.order {
		need := coordinator.units[other]
		if !contains(state.planned.Needs, need.planned.Base) {
			continue
		}
		switch need.status {
		case protocol.StatusPassed:
		case "":
			ready = false
		default:
			return false, fmt.Sprintf("%s, which ended %s", other, need.status)
		}
	}
	return ready, ""
}

func contains(list []string, item string) bool {
	for _, value := range list {
		if value == item {
			return true
		}
	}
	return false
}

// skipBlocked notes every waiting unit whose needs can no longer pass: it will never run, so it never
// finishes, and the run is void for it. The caller holds the mutex.
func (coordinator *coordinator) skipBlocked() {
	for _, id := range coordinator.order {
		state := coordinator.units[id]
		if state.status != "" || state.running {
			continue
		}
		if _, blocked := coordinator.needsOf(state); blocked != "" {
			state.status = "skipped"
			coordinator.record.note(id, placeError("not placed: it needs %s", blocked))
			fmt.Fprintf(coordinator.config.Log, "%s: not placed, it needs %s\n", id, blocked)
		}
	}
}

// placeReady starts every ready unit it has a slot for, longest first, and says whether it started any.
// The caller holds the mutex.
func (coordinator *coordinator) placeReady(runContext context.Context) bool {
	if runContext.Err() != nil {
		return false
	}
	var ready []*unitState
	for _, id := range coordinator.order {
		state := coordinator.units[id]
		if state.status != "" || state.running {
			continue
		}
		if ok, _ := coordinator.needsOf(state); ok {
			ready = append(ready, state)
		}
	}
	// A unit another unit needs goes first, whatever its length: everything waiting on it starts only once it passes
	// (Oct 9, main 002fdade: 59 product units of 6 s, which 635 test units need, ranked behind 545 longer units). Then
	// longest first by the planner's estimate or the last recorded wall; a unit never timed goes first, since it may
	// be the longest.
	needed := map[string]bool{}
	for _, id := range coordinator.order {
		for _, need := range coordinator.units[id].planned.Needs {
			needed[need] = true
		}
	}
	sort.SliceStable(ready, func(left, right int) bool {
		if needed[ready[left].planned.Base] != needed[ready[right].planned.Base] {
			return needed[ready[left].planned.Base]
		}
		return coordinator.expected(ready[left]) > coordinator.expected(ready[right])
	})
	started := false
	for _, state := range ready {
		if missing := coordinator.unfit(state); missing != "" {
			// No machine of the run could ever take it: it's never placed, and the run is void for it.
			state.status = "skipped"
			coordinator.record.note(state.planned.Id, placeError("not placed: no machine of the run has %s", missing))
			fmt.Fprintf(coordinator.config.Log, "%s: not placed, no machine of the run has %s\n", state.planned.Id, missing)
			continue
		}
		slot := coordinator.pickSlot(state.lastSlot, state.planned.Unit, state.brokeOn)
		if slot < 0 {
			// A test job may wait for a strict slot while an argv unit behind it takes a box's, or the other way round.
			continue
		}
		coordinator.free[slot] = false
		state.running = true
		state.attempts++
		state.lastSlot = slot
		started = true
		coordinator.active.Add(1)
		go coordinator.attempt(runContext, state, slot)
	}
	return started
}

func (coordinator *coordinator) expected(state *unitState) float64 {
	if state.planned.Unit.ExpectedSeconds > 0 {
		return state.planned.Unit.ExpectedSeconds
	}
	if coordinator.config.Durations == nil {
		return 0
	}
	if seconds, ok := coordinator.config.Durations.Get(coordinator.job.Name, state.planned.Id); ok {
		return seconds
	}
	return float64(1 << 30)
}

// busyOn counts the slots of a machine in use. The caller holds the mutex.
func (coordinator *coordinator) busyOn(name string) int {
	busy := 0
	for index, free := range coordinator.free {
		if !free && coordinator.config.Slots[index].Name() == name {
			busy++
		}
	}
	return busy
}

// preemptOverLimit stops the newest unit on each machine that holds more slots than its limit allows. The
// caller holds the mutex.
func (coordinator *coordinator) preemptOverLimit() {
	if coordinator.config.SlotLimit == nil {
		return
	}
	names := map[string]bool{}
	for _, machine := range coordinator.config.Slots {
		names[machine.Name()] = true
	}
	for name := range names {
		over := coordinator.busyOn(name) - coordinator.config.SlotLimit(name)
		for ; over > 0; over-- {
			var newest *unitState
			for _, state := range coordinator.units {
				if state.running && !state.preempted && state.preempt != nil && state.lastSlot >= 0 &&
					coordinator.config.Slots[state.lastSlot].Name() == name && (newest == nil || state.startedAt.After(newest.startedAt)) {
					newest = state
				}
			}
			if newest == nil {
				break
			}
			newest.preempted = true
			newest.preempt()
			fmt.Fprintf(coordinator.config.Log, "%s: preempted on %s, the gate wants the slot back\n", newest.planned.Id, name)
		}
	}
}

// pickSlot returns a free slot that can run the unit, preferring one on another machine than avoid's, or -1 when
// none is free. A machine at its slot limit offers none. A strict machine's slots take only test jobs; when the run
// has any, a test job goes only to them, and an argv unit never does (#098rcha). A machine without every toolchain
// the unit requires offers none.
func (coordinator *coordinator) pickSlot(avoid int, unit protocol.JobUnit, brokeOn map[string]bool) int {
	testJob := unit.Test != nil
	fallback := -1
	strictSlots := false
	for _, machine := range coordinator.config.Slots {
		strictSlots = strictSlots || takesOnlyTestJobs(machine)
	}
	for index, free := range coordinator.free {
		if !free {
			continue
		}
		if strict := takesOnlyTestJobs(coordinator.config.Slots[index]); strict != (testJob && strictSlots) {
			continue
		}
		if brokeOn[coordinator.config.Slots[index].Name()] || !coordinator.fitsUnit(coordinator.config.Slots[index], unit) {
			continue
		}
		if limit := coordinator.config.SlotLimit; limit != nil {
			name := coordinator.config.Slots[index].Name()
			if coordinator.busyOn(name) >= limit(name) {
				continue
			}
		}
		if avoid < 0 || coordinator.config.Slots[index].Name() != coordinator.config.Slots[avoid].Name() {
			return index
		}
		if fallback < 0 {
			fallback = index
		}
	}
	return fallback
}

// hasMachineFor says whether any slot of the run, free or not, could take the unit again: one of its kind on a machine
// that hasn't broken it. The caller holds the mutex.
func (coordinator *coordinator) hasMachineFor(state *unitState) bool {
	strictSlots := false
	for _, machine := range coordinator.config.Slots {
		strictSlots = strictSlots || takesOnlyTestJobs(machine)
	}
	for _, machine := range coordinator.config.Slots {
		if takesOnlyTestJobs(machine) == (state.planned.Unit.Test != nil && strictSlots) && !state.brokeOn[machine.Name()] && coordinator.fitsUnit(machine, state.planned.Unit) {
			return true
		}
	}
	return false
}

// unfit names what the unit needs that no machine of the run has (a toolchain, or the record platform for a unit
// that isn't portable), or is empty when one could take it.
func (coordinator *coordinator) unfit(state *unitState) string {
	if record := coordinator.config.RecordPlatform; record != "" && !state.planned.Unit.Portable {
		found := false
		for _, machine := range coordinator.config.Slots {
			found = found || machine.Platform() == record
		}
		if !found {
			return "the record platform " + record
		}
	}
	for _, toolchain := range state.planned.Unit.Requires {
		found := false
		for _, machine := range coordinator.config.Slots {
			if fits(machine, []string{toolchain}) {
				found = true
				break
			}
		}
		if !found {
			return toolchain
		}
	}
	return ""
}

// fitsUnit says whether the machine may take the unit: it has every toolchain the unit requires, and in a run with a
// record platform, it is of that platform or the unit is portable.
func (coordinator *coordinator) fitsUnit(machine Machine, unit protocol.JobUnit) bool {
	if record := coordinator.config.RecordPlatform; record != "" && !unit.Portable && machine.Platform() != record {
		return false
	}
	return fits(machine, unit.Requires)
}

// fits says whether the machine has every toolchain in requires. A machine that names none has none.
func fits(machine Machine, requires []string) bool {
	if len(requires) == 0 {
		return true
	}
	declared, ok := machine.(interface{ Toolchains() []string })
	if !ok {
		return false
	}
	for _, toolchain := range requires {
		if !slices.Contains(declared.Toolchains(), toolchain) {
			return false
		}
	}
	return true
}

// takesOnlyTestJobs says whether a machine runs only test jobs (a strict pool's).
func takesOnlyTestJobs(machine Machine) bool {
	strict, ok := machine.(interface{ TakesOnlyTestJobs() bool })
	return ok && strict.TakesOnlyTestJobs()
}

// attempt runs one unit once on one slot, or serves it from the cache, and settles its state.
func (coordinator *coordinator) attempt(runContext context.Context, state *unitState, slot int) {
	defer coordinator.active.Done()
	machine := coordinator.config.Slots[slot]
	id := state.planned.Id
	unit := coordinator.unitFor(state.planned)
	key := protocol.CacheKey(unit, machine.RunnerVersion(), machine.Platform())
	status := ""
	if state.planned.Unit.Cache && !coordinator.config.Uncached {
		status = coordinator.fromCache(runContext, id, key)
	}
	if status == "" {
		status = coordinator.runOn(runContext, state, machine, unit, key)
	}

	coordinator.mutex.Lock()
	defer coordinator.mutex.Unlock()
	state.running = false
	coordinator.free[slot] = true
	if status == "preempted" {
		// The gate took the slot back: the unit waits for one again, and this attempt doesn't count.
		state.attempts--
		state.preempted = false
		status = ""
	}
	state.preempt = nil
	if status == "dropped" && state.attempts < 2 && runContext.Err() == nil {
		// Placed again once, elsewhere when there is an elsewhere; the drop is already in the record.
		status = ""
	}
	// A unit its machine couldn't run (#5pfcv0t: Codex 5116fd40e771 refused 220 units for its disk, and their reds
	// cancelled the run) is placed again, at most twice, never on a box that broke it. A pool is one machine here: it
	// may hand the unit back to the same worker until the wire skips the workers that broke it.
	if status == protocol.StatusBroken && state.planned.Unit.BrokenExit != 0 && state.brokenAgain < 2 && runContext.Err() == nil {
		if _, pooled := machine.(*PoolMachine); !pooled {
			state.brokeOn[machine.Name()] = true
		}
		if coordinator.hasMachineFor(state) {
			state.brokenAgain++
			coordinator.record.note(id, placeError("%s broke the unit; placing it again", machine.Name()))
			status = ""
		}
	}
	state.status = status
	select {
	case coordinator.wake <- struct{}{}:
	default: // a wake is already pending; the scheduler looks at every unit when it runs
	}
}

// jsonLine is one event as the line the wire and the cache's event log hold.
func jsonLine(event protocol.Event) (string, error) {
	line, err := json.Marshal(event)
	return string(line) + "\n", err
}

// unitFor is what a runner receives for a planned unit. The coordinator relays events itself, so the runner
// has no wire; it reads and writes blobs through the run's own endpoint.
func (coordinator *coordinator) unitFor(planned protocol.PlannedUnit) protocol.Unit {
	unit := protocol.Unit{
		Run: coordinator.run, Unit: planned.Id, Argv: planned.Unit.Argv, Test: planned.Unit.Test, BrokenExit: planned.Unit.BrokenExit, Environment: planned.Unit.Environment,
		Directory: planned.Unit.Directory, Inputs: planned.Unit.Inputs, Outputs: planned.Unit.Outputs, Products: planned.Unit.Products, ProductStore: planned.Unit.ProductStore,
		TimeoutSeconds: planned.Unit.TimeoutSeconds, Resources: planned.Unit.Resources, Token: coordinator.runnerToken,
	}
	if len(unit.Inputs) > 0 || len(unit.Outputs) > 0 {
		unit.Store = &protocol.Endpoint{Url: strings.TrimSuffix(coordinator.config.Wire, "/") + "/runs/" + coordinator.run + "/blobs"}
	}
	return unit
}

// fromCache serves a unit from the cache when its key has an entry, and returns "passed", or "" on a miss.
func (coordinator *coordinator) fromCache(runContext context.Context, id string, key string) string {
	entry, err := coordinator.wire.cacheEntry(runContext, coordinator.coordinatorToken, key)
	if err != nil {
		fmt.Fprintf(coordinator.config.Log, "%s: reading the cache: %v; running it\n", id, err)
		return ""
	}
	if entry == nil {
		return ""
	}
	coordinator.record.note(id, protocol.Event{Type: "cached", Key: key, FromRun: entry.Run, EventLog: entry.Events})
	coordinator.record.note(id, protocol.Event{Type: "finished", Status: protocol.StatusPassed})
	fmt.Fprintf(coordinator.config.Log, "%s: from the cache (run %s)\n", id, entry.Run)
	return protocol.StatusPassed
}

// runOn runs the unit on a machine and returns its finished status, or "dropped" when the runner ended
// without finishing or ran late.
func (coordinator *coordinator) runOn(runContext context.Context, state *unitState, machine Machine, unit protocol.Unit, key string) string {
	id := state.planned.Id
	if state.attempts > 1 {
		coordinator.record.note(id, placeError("placed again on %s", machine.Name()))
	}
	fmt.Fprintf(coordinator.config.Log, "%s: on %s\n", id, machine.Name())
	coordinator.mutex.Lock()
	coordinator.machines[machine.Name()] = true
	coordinator.mutex.Unlock()
	limit := time.Duration(unit.TimeoutSeconds)*time.Second + coordinator.config.LateGrace
	_, pooled := machine.(*PoolMachine)
	// A box starts the unit when it's placed; a pool unit may wait for a worker first, so its clock starts at its
	// first event and its wait in the queue has an allowance of its own.
	deadline := limit
	if pooled {
		deadline = coordinator.config.PoolQueueWait + limit
	}
	attemptContext, cancel := context.WithTimeout(runContext, deadline)
	defer cancel()
	var firstEvent time.Time
	var clock *time.Timer
	late := false
	coordinator.mutex.Lock()
	state.preempt = cancel
	state.startedAt = time.Now()
	coordinator.mutex.Unlock()
	attempt := coordinator.record.begin(id)
	if pooled {
		attempt.continueFromOffset(&unit)
	}
	reader, writer := io.Pipe()
	parsed := make(chan struct{})
	finishedStatus := ""
	go func() {
		defer close(parsed)
		scanner := bufio.NewScanner(reader)
		scanner.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for scanner.Scan() {
			var event protocol.Event
			if err := protocol.Decode(strings.NewReader(scanner.Text()), &event); err != nil {
				coordinator.record.note(id, placeError("the runner on %s sent a line that isn't an event: %v", machine.Name(), err))
				continue
			}
			// A preempted attempt's stream ends at the preemption: what its runner says after belongs to an
			// attempt that was given up, and its finished would make the unit finish twice.
			coordinator.mutex.Lock()
			given := state.preempted
			coordinator.mutex.Unlock()
			if given {
				continue
			}
			if event.Run == coordinator.run && event.Unit == id && event.Type == "finished" {
				finishedStatus = event.Status
			}
			if pooled {
				coordinator.mutex.Lock()
				if firstEvent.IsZero() {
					firstEvent = time.Now()
					clock = time.AfterFunc(limit, func() {
						coordinator.mutex.Lock()
						late = true
						coordinator.mutex.Unlock()
						cancel()
					})
				}
				coordinator.mutex.Unlock()
			}
			attempt.take(event)
		}
		io.Copy(io.Discard, reader)
	}()
	started := time.Now()
	runError := machine.Run(attemptContext, unit, writer)
	writer.Close()
	<-parsed
	coordinator.mutex.Lock()
	if clock != nil {
		clock.Stop()
	}
	// A pool unit's wall is its own, from its first event: its time in the queue is the pool's, not the unit's.
	if !firstEvent.IsZero() {
		started = firstEvent
	}
	ranLate, neverStarted := late, pooled && firstEvent.IsZero()
	coordinator.mutex.Unlock()
	wall := time.Since(started)

	coordinator.mutex.Lock()
	preempted := state.preempted
	coordinator.mutex.Unlock()
	// A unit that finished as it was being preempted finished: queuing it again would finish it twice.
	if attempt.finished && (attemptContext.Err() == nil || preempted) {
		if coordinator.config.Durations != nil {
			coordinator.config.Durations.Set(coordinator.job.Name, id, wall.Seconds())
		}
		fmt.Fprintf(coordinator.config.Log, "%s: %s on %s in %.1f s\n", id, finishedStatus, machine.Name(), wall.Seconds())
		if finishedStatus == protocol.StatusPassed && state.planned.Unit.Cache && !coordinator.config.Uncached {
			coordinator.remember(runContext, id, key, machine, wall)
		}
		return finishedStatus
	}
	if preempted && runContext.Err() == nil {
		coordinator.record.note(id, placeError("preempted on %s after %d events: the gate took the slot back; queued again", machine.Name(), attempt.lines))
		return "preempted"
	}
	why := "its runner ended without finishing"
	if attemptContext.Err() != nil && runContext.Err() == nil {
		why = fmt.Sprintf("it ran past its timeout and %v of grace", coordinator.config.LateGrace)
		if pooled && neverStarted && !ranLate {
			why = fmt.Sprintf("it waited %v in the pool's queue and never started", coordinator.config.PoolQueueWait)
		}
	}
	if runError != nil {
		why += fmt.Sprintf(" (%v)", runError)
	}
	if runContext.Err() != nil {
		why = "the coordinator was stopped"
	}
	coordinator.record.note(id, placeError("%s dropped the unit after %d events: %s", machine.Name(), attempt.lines, why))
	fmt.Fprintf(coordinator.config.Log, "%s: dropped by %s after %d events: %s\n", id, machine.Name(), attempt.lines, why)
	return "dropped"
}

// remember writes a passed unit's cache entry: its event log as a blob, then the entry. A failure is only
// logged; the cache is an optimisation and the verdict doesn't depend on it.
func (coordinator *coordinator) remember(runContext context.Context, id string, key string, machine Machine, wall time.Duration) {
	var log strings.Builder
	var outputs []protocol.CacheOutput
	for _, event := range coordinator.record.unitEvents(id) {
		line, _ := jsonLine(event)
		log.WriteString(line)
		if event.Type == "uploaded" {
			outputs = append(outputs, protocol.CacheOutput{Path: event.Path, Sha256: event.Sha256, Bytes: event.Bytes})
		}
	}
	content := []byte(log.String())
	sum := sha256.Sum256(content)
	events := hex.EncodeToString(sum[:])
	if err := coordinator.wire.putBlob(runContext, coordinator.run, coordinator.coordinatorToken, events, content); err != nil {
		fmt.Fprintf(coordinator.config.Log, "%s: caching its event log: %v\n", id, err)
		return
	}
	entry := protocol.CacheEntry{Key: key, Run: coordinator.run, Unit: id, Machine: machine.Name(), RunnerVersion: machine.RunnerVersion(),
		WallSeconds: wall.Seconds(), Outputs: outputs, Events: events}
	if err := coordinator.wire.putCacheEntry(runContext, coordinator.coordinatorToken, entry); err != nil {
		fmt.Fprintf(coordinator.config.Log, "%s: writing its cache entry: %v\n", id, err)
	}
}
