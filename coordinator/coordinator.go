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
	// LateGrace is how far past its timeout an attempt may run before it counts as dropped. Zero means 60 s.
	LateGrace time.Duration
	// Client makes every HTTP call. Nil means one with sane timeouts.
	Client *http.Client
}

// A Result is a run's outcome: its id, the verdict, and where a person can watch it.
type Result struct {
	Run     string
	Verdict protocol.Verdict
	Page    string // the live page, with a viewer token in it
	// Machines names every machine a unit ran on, for the rollout ledger.
	Machines []string
}

// unitState is where one planned unit stands.
type unitState struct {
	planned  protocol.PlannedUnit
	status   string // "" while waiting or running, then the finished status, or "dropped" or "skipped"
	running  bool
	attempts int
	lastSlot int // the slot of the last attempt, so a re-placement goes elsewhere when it can
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
	run := RunId(job.Name, time.Now())
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
	result.Verdict = protocol.Decide(run, body.Units, coordinator.record.snapshot())
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
		coordinator.units[planned.Id] = &unitState{planned: planned, lastSlot: -1}
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
		coordinator.mutex.Unlock()
		if running == 0 && !started {
			break
		}
		if runContext.Err() != nil && running == 0 {
			break
		}
		select {
		case <-coordinator.wake:
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
	// Longest first by the last recorded wall time; a unit never timed goes first, since it may be the longest.
	sort.SliceStable(ready, func(left, right int) bool {
		return coordinator.expected(ready[left]) > coordinator.expected(ready[right])
	})
	started := false
	for _, state := range ready {
		slot := coordinator.pickSlot(state.lastSlot)
		if slot < 0 {
			break
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
	if coordinator.config.Durations == nil {
		return 0
	}
	if seconds, ok := coordinator.config.Durations.Get(coordinator.job.Name, state.planned.Id); ok {
		return seconds
	}
	return float64(1 << 30)
}

// pickSlot returns a free slot, preferring one on another machine than avoid's, or -1 when none is free.
func (coordinator *coordinator) pickSlot(avoid int) int {
	fallback := -1
	for index, free := range coordinator.free {
		if !free {
			continue
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
	if status == "dropped" && state.attempts < 2 && runContext.Err() == nil {
		// Placed again once, elsewhere when there is an elsewhere; the drop is already in the record.
		status = ""
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
		Run: coordinator.run, Unit: planned.Id, Argv: planned.Unit.Argv, Environment: planned.Unit.Environment,
		Directory: planned.Unit.Directory, Inputs: planned.Unit.Inputs, Outputs: planned.Unit.Outputs,
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
	attemptContext, cancel := context.WithTimeout(runContext, limit)
	defer cancel()
	attempt := coordinator.record.begin(id)
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
			if event.Run == coordinator.run && event.Unit == id && event.Type == "finished" {
				finishedStatus = event.Status
			}
			attempt.take(event)
		}
		io.Copy(io.Discard, reader)
	}()
	started := time.Now()
	runError := machine.Run(attemptContext, unit, writer)
	writer.Close()
	<-parsed
	wall := time.Since(started)

	if attempt.finished && attemptContext.Err() == nil {
		if coordinator.config.Durations != nil {
			coordinator.config.Durations.Set(coordinator.job.Name, id, wall.Seconds())
		}
		fmt.Fprintf(coordinator.config.Log, "%s: %s on %s in %.1f s\n", id, finishedStatus, machine.Name(), wall.Seconds())
		if finishedStatus == protocol.StatusPassed && state.planned.Unit.Cache && !coordinator.config.Uncached {
			coordinator.remember(runContext, id, key, machine, wall)
		}
		return finishedStatus
	}
	why := "its runner ended without finishing"
	if attemptContext.Err() != nil && runContext.Err() == nil {
		why = fmt.Sprintf("it ran past its timeout and %v of grace", coordinator.config.LateGrace)
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
