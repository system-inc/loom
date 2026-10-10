package coordinator

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A PoolMachine places units on a pool, a queue on the wire, for machines Loom can't ssh into (Codex cloud
// instances). Each of those runs `loom-runner serve`, takes units off the queue and posts their events to
// the run's events endpoint itself; the coordinator reads them back from the run's event log. One
// PoolMachine is one pool: put the same pointer in Config.Slots once for each unit the pool may run at a
// time, and its slots share one follower per run, so a run's log is read once however many slots wait on it.
//
// The coordinator's late clock starts when it places a unit, so a unit's time in the queue counts against
// its timeout and grace: give a pool no more slots than it has workers serving.
//
// A unit can also leave the queue and never start: the wire answers an ask whose serve turn ended as it waited
// (Oct 8, p2's tests-08 sat 80 minutes that way). Run asks the pool which of the run's units are still queued; one
// that has left it with no event from its runner for NeverStarted is queued again. It posted nothing, so the next
// worker's events conflict with none.
//
// A worker can also go mid-unit (its turn ended). A runner that beats says so in its started event, and one
// silent for three of its heartbeats is given up; the coordinator places the unit again.
//
// The record renumbers each attempt's events and relays them to the wire, which already holds a pool unit's
// events from its runner; the relay's copies are identical, so the wire drops them as replays. A re-placed
// unit's runner is told to number from the record's offset (Unit.SequenceStart), so its stream continues the
// earlier attempts' on the wire instead of conflicting with them.
type PoolMachine struct {
	// Pool is the pool's name on the wire, such as codex.
	Pool string
	// Strict says the pool's workers serve with --strict, running only test jobs: only a unit carrying one is placed
	// there, so a strict worker never meets argv and refuses it (#098rcha).
	Strict bool
	// Priority ranks this machine's units on the pool, 0 to 1000: the pool hands out the highest first, and first in,
	// first out within one priority.
	Priority int
	// Label names the machine in the record and on the board; empty means "pool:<pool>".
	Label string
	// Wire is the Worker's origin, the coordinator's Config.Wire.
	Wire string
	// Secret signs the coordinator token each Run queues its unit and reads its run's events with.
	Secret []byte
	// Version and GoPlatform are what the pool's workers are expected to run, for the cache key. The
	// coordinator can't see which worker takes a unit; its started event says what actually ran it.
	Version    string
	GoPlatform string
	// CoreCount is the pool's CPUs for the board, 0 when unknown.
	CoreCount int
	// Client makes the calls to the wire. Nil means a client without an overall timeout, since reading the
	// event log waits up to 20 s for each answer; each request carries its own deadline instead.
	Client *http.Client
	// NeverStarted is how long a unit may be gone from the queue with no event from its runner before it is
	// queued again; 0 means two minutes. QueueCheck is how often a unit that hasn't started looks; 0 means 30 s.
	NeverStarted time.Duration
	QueueCheck   time.Duration
	// AgeEvery lifts a unit that waits: each AgeEvery it hasn't started, it is offered again AgeStep higher than
	// Priority, up to AgeCeiling (0 means MaximumPoolPriority), so a lower tier never starves behind a stream of
	// higher ones (Oct 9: a 6-unit tier-30 probe placed 0 of 6 in 30 minutes behind tier-40 landings, #0k03wz1).
	// 0 leaves every unit at Priority.
	AgeEvery   time.Duration
	AgeStep    int
	AgeCeiling int
	// Has names the toolchains every worker of the pool has, for units that require them (protocol.Toolchains):
	// a Codex instance set up with setup.sh --wasi-sdk has wasiSdk.
	Has []string
	// MemoryMegabytes is each worker's memory, 0 when unknown: a unit declaring more (its Resources) is never placed
	// here (Oct 10: f7812fff's typeaware products took two 16 GB Codex instances down, so they go to a box).
	MemoryMegabytes int
	// Log, when set, hears each unit queued again.
	Log io.Writer

	mutex     sync.Mutex
	followers map[string]*runFollower            // by run
	queued    map[string]queuedSnapshot          // by run, the last look at its units still queued
	waiting   map[string]map[string]*waitingUnit // by run and unit, the units queued that haven't started
	// placing is held around every post to the pool and every re-offer, so a re-offer's look, cancel and post see no
	// post of this machine's in between.
	placing sync.Mutex
}

// MaximumPoolPriority is the highest priority the wire takes.
const MaximumPoolPriority = 1000

// A waitingUnit is a unit queued on the pool that hasn't started: when it was first queued, and at what priority it
// sits there now. Its fields are guarded by the machine's mutex.
type waitingUnit struct {
	unit     protocol.Unit
	since    time.Time
	priority int
}

// A poolBatch is the body of a pool's units call: the units and the priority they share, left off at 0.
type poolBatch struct {
	Units    []protocol.Unit `json:"units"`
	Priority int             `json:"priority,omitempty"`
}

// silentBeats is how many of its runner's heartbeats a started unit may go silent before its worker counts as gone.
const silentBeats = 3

// maximumRequeues bounds how often one attempt queues its unit again before it gives the unit up as dropped.
const maximumRequeues = 5

// A queuedSnapshot is one look at a run's units still in the pool's queue, shared by every slot waiting on the run.
type queuedSnapshot struct {
	at    time.Time
	units map[string]bool
}

func (machine *PoolMachine) Name() string {
	if machine.Label != "" {
		return machine.Label
	}
	return "pool:" + machine.Pool
}

func (machine *PoolMachine) RunnerVersion() string { return machine.Version }

// TakesOnlyTestJobs says the machine's workers run nothing but test jobs.
func (machine *PoolMachine) TakesOnlyTestJobs() bool { return machine.Strict }

func (machine *PoolMachine) Platform() string { return machine.GoPlatform }

func (machine *PoolMachine) Toolchains() []string { return machine.Has }

// MemoryCapacity is each worker's memory in megabytes, 0 when unknown.
func (machine *PoolMachine) MemoryCapacity() int { return machine.MemoryMegabytes }

func (machine *PoolMachine) Cores() int { return machine.CoreCount }

// Run queues the unit on the pool and writes its events, as the wire logged them, to events until its
// finished event. Cancelling runContext just returns: the unit may still sit in the queue or run on a
// worker. Cancelling the run on the pool would drop every queued unit of the run, not only this one.
func (machine *PoolMachine) Run(runContext context.Context, unit protocol.Unit, events io.Writer) error {
	origin := strings.TrimSuffix(machine.Wire, "/")
	if unit.Wire == nil || unit.Wire.Url == "" {
		// A worker can't hand events to the coordinator, so its runner posts them to the run itself.
		unit.Wire = &protocol.Endpoint{Url: origin + "/runs/" + unit.Run + "/events"}
	}
	token, err := protocol.MintToken(machine.Secret, protocol.TokenClaims{Run: unit.Run, Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(48 * time.Hour).Unix()})
	if err != nil {
		return err
	}
	// Following starts before the unit is queued, so none of its events can come before the follower looks.
	stream := machine.subscribe(unit.Run, unit.Unit, token)
	defer machine.unsubscribe(unit.Run, unit.Unit, stream)
	wire := &wireClient{url: machine.Wire, client: machine.client()}
	waiting := machine.wait(unit)
	defer machine.unwait(unit)
	if err := machine.post(runContext, wire, token, []*waitingUnit{waiting}, machine.Priority); err != nil {
		return fmt.Errorf("queuing on pool %s: %w", machine.Pool, err)
	}
	check := time.NewTicker(machine.queueCheck())
	defer check.Stop()
	heard := false
	var leftQueue time.Time // when a look first found the unit gone from the queue, zero while it waits there
	requeues := 0
	var lastHeard time.Time
	heartbeat := time.Duration(0) // the runner's, from its started event; 0 for a runner that doesn't beat
	for {
		machine.mutex.Lock()
		lines, failure := stream.lines, stream.failure
		stream.lines = nil
		machine.mutex.Unlock()
		if len(lines) > 0 {
			heard, lastHeard = true, time.Now()
		}
		for _, line := range lines {
			if line.heartbeat > 0 {
				heartbeat = time.Duration(line.heartbeat * float64(time.Second))
			}
			if _, err := events.Write(line.text); err != nil {
				return err
			}
			if line.finished {
				return nil
			}
		}
		if failure != nil {
			return failure
		}
		select {
		case <-stream.signal:
		case <-check.C:
			// A runner that beats says something at least every heartbeat; three beats of silence is a worker
			// gone mid-unit (its turn ended). The unit is given up here and placed again, its stream continued.
			if heard && heartbeat > 0 && time.Since(lastHeard) >= silentBeats*heartbeat {
				return fmt.Errorf("silent for %.0f s, %d of its runner's %.0f s heartbeats: its worker is gone", time.Since(lastHeard).Seconds(), silentBeats, heartbeat.Seconds())
			}
			if heard {
				continue
			}
			queued, err := machine.queuedUnits(runContext, wire, token, unit.Run)
			switch {
			case err != nil:
				// No answer says nothing about the unit; the next look asks again.
			case queued[unit.Unit]:
				leftQueue = time.Time{}
				if machine.aged(waiting) > machine.priorityOf(waiting) {
					machine.reoffer(runContext, wire, token, unit.Run)
				}
			case leftQueue.IsZero():
				leftQueue = time.Now()
			case time.Since(leftQueue) >= machine.neverStarted():
				if requeues == maximumRequeues {
					return fmt.Errorf("taken off pool %s's queue %d times and never started", machine.Pool, requeues+1)
				}
				// Queued again at the tier its wait has earned, so a unit a re-offer had to leave out loses no rank.
				if err := machine.post(runContext, wire, token, []*waitingUnit{waiting}, machine.aged(waiting)); err != nil {
					return fmt.Errorf("queuing again on pool %s: %w", machine.Pool, err)
				}
				requeues++
				if machine.Log != nil {
					fmt.Fprintf(machine.Log, "%s: queued again on pool %s, taken %.0f s ago by a worker that never started it\n", unit.Unit, machine.Pool, time.Since(leftQueue).Seconds())
				}
				leftQueue = time.Time{}
			}
		case <-runContext.Done():
			return fmt.Errorf("given up on pool %s; the unit may still be queued or running there", machine.Pool)
		}
	}
}

// wait records the unit as queued and not started, from now.
func (machine *PoolMachine) wait(unit protocol.Unit) *waitingUnit {
	machine.mutex.Lock()
	defer machine.mutex.Unlock()
	if machine.waiting == nil {
		machine.waiting = map[string]map[string]*waitingUnit{}
	}
	if machine.waiting[unit.Run] == nil {
		machine.waiting[unit.Run] = map[string]*waitingUnit{}
	}
	waiting := &waitingUnit{unit: unit, since: time.Now(), priority: machine.Priority}
	machine.waiting[unit.Run][unit.Unit] = waiting
	return waiting
}

func (machine *PoolMachine) unwait(unit protocol.Unit) {
	machine.mutex.Lock()
	defer machine.mutex.Unlock()
	delete(machine.waiting[unit.Run], unit.Unit)
	if len(machine.waiting[unit.Run]) == 0 {
		delete(machine.waiting, unit.Run)
	}
}

func (machine *PoolMachine) priorityOf(waiting *waitingUnit) int {
	machine.mutex.Lock()
	defer machine.mutex.Unlock()
	return waiting.priority
}

// aged is the priority a waiting unit has earned: Priority, plus AgeStep for each AgeEvery since it was first queued,
// up to the ceiling. It never falls below Priority.
func (machine *PoolMachine) aged(waiting *waitingUnit) int {
	if machine.AgeEvery <= 0 || machine.AgeStep <= 0 {
		return machine.Priority
	}
	ceiling := machine.AgeCeiling
	if ceiling <= 0 || ceiling > MaximumPoolPriority {
		ceiling = MaximumPoolPriority
	}
	earned := machine.Priority + machine.AgeStep*int(time.Since(waiting.since)/machine.AgeEvery)
	return max(machine.Priority, min(earned, ceiling))
}

// post queues the units on the pool as one batch at the priority, under placing, and records it as theirs.
func (machine *PoolMachine) post(callContext context.Context, wire *wireClient, token string, units []*waitingUnit, priority int) error {
	machine.placing.Lock()
	defer machine.placing.Unlock()
	return machine.postLocked(callContext, wire, token, units, priority)
}

func (machine *PoolMachine) postLocked(callContext context.Context, wire *wireClient, token string, units []*waitingUnit, priority int) error {
	batch := poolBatch{Priority: priority}
	for _, waiting := range units {
		batch.Units = append(batch.Units, waiting.unit)
	}
	body, err := json.Marshal(batch)
	if err != nil {
		return err
	}
	if _, err := wire.call(callContext, http.MethodPost, "/pools/"+machine.Pool+"/units", token, body); err != nil {
		return err
	}
	machine.mutex.Lock()
	for _, waiting := range units {
		waiting.priority = priority
	}
	machine.mutex.Unlock()
	return nil
}

// reoffer moves the run's queued units up to the tiers their waits have earned. The wire has no way to change a queued
// unit's priority and drops queued units only a whole run at a time, so it looks at the run's queue, drops it, and
// queues the same units again. A worker can take a unit between the look and the drop; then the drop counts fewer
// than the look listed, and nothing is queued again, since queuing a unit already taken would run it twice. Each
// dropped unit is found gone from the queue and queued again on its own, at its earned tier, after NeverStarted.
func (machine *PoolMachine) reoffer(callContext context.Context, wire *wireClient, token string, run string) {
	machine.placing.Lock()
	defer machine.placing.Unlock()
	listed, err := machine.queuedNow(callContext, wire, token, run)
	if err != nil || len(listed) == 0 {
		return
	}
	units := make([]*waitingUnit, 0, len(listed))
	due := false
	machine.mutex.Lock()
	for _, id := range listed {
		waiting := machine.waiting[run][id]
		if waiting == nil {
			// A unit in the queue this machine isn't waiting on: dropping it would lose it, so nothing moves.
			machine.mutex.Unlock()
			return
		}
		units = append(units, waiting)
	}
	machine.mutex.Unlock()
	for _, waiting := range units {
		if machine.aged(waiting) > machine.priorityOf(waiting) {
			due = true
		}
	}
	if !due {
		return
	}
	body, err := json.Marshal(map[string]string{"run": run})
	if err != nil {
		return
	}
	answer, err := wire.call(callContext, http.MethodPost, "/pools/"+machine.Pool+"/cancel", token, body)
	if err != nil {
		return
	}
	var cancelled struct {
		Dropped int `json:"dropped"`
	}
	if err := protocol.Decode(bytes.NewReader(answer), &cancelled); err != nil {
		return
	}
	machine.mutex.Lock()
	delete(machine.queued, run)
	machine.mutex.Unlock()
	if cancelled.Dropped != len(listed) {
		if machine.Log != nil {
			fmt.Fprintf(machine.Log, "pool %s: %d of run %s's units were queued and %d dropped, so a worker took one in between; each is queued again on its own\n", machine.Pool, len(listed), run, cancelled.Dropped)
		}
		return
	}
	byPriority := map[int][]*waitingUnit{}
	for _, waiting := range units {
		earned := machine.aged(waiting)
		byPriority[earned] = append(byPriority[earned], waiting)
	}
	for priority, group := range byPriority {
		if err := machine.postLocked(callContext, wire, token, group, priority); err != nil {
			// Not queued again here; each is found gone and queued again on its own.
			continue
		}
		if machine.Log != nil {
			fmt.Fprintf(machine.Log, "pool %s: %d of run %s's units offered again at priority %d after waiting %.0f s\n", machine.Pool, len(group), run, priority, time.Since(group[0].since).Seconds())
		}
	}
}

// queuedNow is the run's units in the pool's queue, oldest first, from a fresh look.
func (machine *PoolMachine) queuedNow(callContext context.Context, wire *wireClient, token string, run string) ([]string, error) {
	body, err := json.Marshal(map[string]string{"run": run})
	if err != nil {
		return nil, err
	}
	answer, err := wire.call(callContext, http.MethodPost, "/pools/"+machine.Pool+"/queued", token, body)
	if err != nil {
		return nil, err
	}
	var listed struct {
		Units []string `json:"units"`
	}
	if err := protocol.Decode(bytes.NewReader(answer), &listed); err != nil {
		return nil, fmt.Errorf("pool %s's queued units: %w", machine.Pool, err)
	}
	return listed.Units, nil
}

func (machine *PoolMachine) neverStarted() time.Duration {
	if machine.NeverStarted > 0 {
		return machine.NeverStarted
	}
	return 2 * time.Minute
}

func (machine *PoolMachine) queueCheck() time.Duration {
	if machine.QueueCheck > 0 {
		return machine.QueueCheck
	}
	return 30 * time.Second
}

// queuedUnits is the run's units still in the pool's queue. A look younger than half a check is shared, so a run's
// waiting slots ask the pool about once a check between them, not once each.
func (machine *PoolMachine) queuedUnits(callContext context.Context, wire *wireClient, token string, run string) (map[string]bool, error) {
	machine.mutex.Lock()
	snapshot, ok := machine.queued[run]
	machine.mutex.Unlock()
	if ok && time.Since(snapshot.at) < machine.queueCheck()/2 {
		return snapshot.units, nil
	}
	body, err := json.Marshal(map[string]string{"run": run})
	if err != nil {
		return nil, err
	}
	answer, err := wire.call(callContext, http.MethodPost, "/pools/"+machine.Pool+"/queued", token, body)
	if err != nil {
		return nil, err
	}
	var listed struct {
		Units []string `json:"units"`
	}
	if err := protocol.Decode(bytes.NewReader(answer), &listed); err != nil {
		return nil, fmt.Errorf("pool %s's queued units: %w", machine.Pool, err)
	}
	units := map[string]bool{}
	for _, id := range listed.Units {
		units[id] = true
	}
	machine.mutex.Lock()
	if machine.queued == nil {
		machine.queued = map[string]queuedSnapshot{}
	}
	machine.queued[run] = queuedSnapshot{at: time.Now(), units: units}
	machine.mutex.Unlock()
	return units, nil
}

func (machine *PoolMachine) client() *http.Client {
	if machine.Client != nil {
		return machine.Client
	}
	return &http.Client{}
}

// A runFollower reads one run's event log from the wire for every slot waiting on a unit of that run. Its
// fields are guarded by the machine's mutex.
type runFollower struct {
	run      string
	token    string
	position int64                  // the last position read; the log's positions start at 1
	streams  map[string]*poolStream // by unit, the units a slot is waiting on
	stop     context.CancelFunc     // ends the reading loop; nil while none runs
}

// A poolStream is one waiting unit's events, read but not yet written to its slot.
type poolStream struct {
	lines   []poolLine
	failure error         // the wire refused the follower in a way asking again can't change
	signal  chan struct{} // told when lines or failure change
}

type poolLine struct {
	text      []byte // the event as a JSON line
	finished  bool
	heartbeat float64 // a started event's heartbeatSeconds, 0 for any other line
}

// subscribe makes a stream for the unit and starts the run's reading loop if none runs.
func (machine *PoolMachine) subscribe(run string, unit string, token string) *poolStream {
	machine.mutex.Lock()
	defer machine.mutex.Unlock()
	if machine.followers == nil {
		machine.followers = map[string]*runFollower{}
	}
	follower := machine.followers[run]
	if follower == nil {
		follower = &runFollower{run: run, streams: map[string]*poolStream{}}
		machine.followers[run] = follower
	}
	follower.token = token
	stream := &poolStream{signal: make(chan struct{}, 1)}
	follower.streams[unit] = stream
	if follower.stop == nil {
		loopContext, cancel := context.WithCancel(context.Background())
		follower.stop = cancel
		go machine.follow(loopContext, follower)
	}
	return stream
}

// unsubscribe drops the unit's stream and stops the reading loop when no slot waits on the run. The follower
// keeps its position, so a later unit of the run reads on from there.
func (machine *PoolMachine) unsubscribe(run string, unit string, stream *poolStream) {
	machine.mutex.Lock()
	defer machine.mutex.Unlock()
	follower := machine.followers[run]
	if follower.streams[unit] == stream {
		delete(follower.streams, unit)
	}
	if len(follower.streams) == 0 && follower.stop != nil {
		follower.stop()
		follower.stop = nil
	}
}

// follow reads the run's log until its context ends, handing each event to the stream waiting on its unit.
// It checks its context under the mutex before it touches the follower, and unsubscribe cancels it under
// the same mutex, so a loop that was stopped never delivers beside the loop that replaced it.
func (machine *PoolMachine) follow(loopContext context.Context, follower *runFollower) {
	failures := 0
	for {
		machine.mutex.Lock()
		after, token := follower.position, follower.token
		machine.mutex.Unlock()
		entries, err := machine.readEvents(loopContext, follower.run, token, after)
		machine.mutex.Lock()
		if loopContext.Err() != nil {
			machine.mutex.Unlock()
			return
		}
		for _, entry := range entries {
			follower.position = max(follower.position, entry.position)
			stream := follower.streams[entry.event.Unit]
			// The coordinator's own place notes come back through the log; the record holds them already.
			if stream == nil || entry.event.Run != follower.run || (entry.event.Type == "error" && entry.event.Phase == protocol.PhasePlace) {
				continue
			}
			line := poolLine{text: entry.line, finished: entry.event.Type == "finished"}
			if entry.event.Type == "started" {
				line.heartbeat = entry.event.HeartbeatSeconds
			}
			stream.lines = append(stream.lines, line)
			notify(stream)
		}
		var failure *readFailure
		if errors.As(err, &failure) && failure.permanent {
			for _, stream := range follower.streams {
				stream.failure = fmt.Errorf("following run %s on the wire: %w", follower.run, err)
				notify(stream)
			}
		}
		machine.mutex.Unlock()
		if err == nil {
			failures = 0
			continue
		}
		failures++
		select {
		case <-loopContext.Done():
			return
		case <-time.After(min(time.Second<<min(failures-1, 3), 5*time.Second)):
		}
	}
}

func notify(stream *poolStream) {
	select {
	case stream.signal <- struct{}{}:
	default: // a signal is already pending; the slot takes every line when it wakes
	}
}

// A poolEntry is one line of the run's event log as the wire serves it.
type poolEntry struct {
	position int64
	event    protocol.Event
	line     []byte // the event alone, compact and newline ended
}

// readFailure is a read of the log that failed; permanent when asking again can't help.
type readFailure struct {
	message   string
	permanent bool
}

func (failure *readFailure) Error() string { return failure.message }

// readEvents asks the wire for the run's events after a position, waiting up to 20 s for the first when
// there are none. It returns every entry it could read, with the error that stopped it, if any.
func (machine *PoolMachine) readEvents(readContext context.Context, run string, token string, after int64) ([]poolEntry, error) {
	requestContext, cancel := context.WithTimeout(readContext, 90*time.Second)
	defer cancel()
	url := strings.TrimSuffix(machine.Wire, "/") + "/runs/" + run + "/events?after=" + strconv.FormatInt(after, 10)
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, url, nil)
	if err != nil {
		return nil, &readFailure{message: err.Error(), permanent: true}
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := machine.client().Do(request)
	if err != nil {
		return nil, &readFailure{message: err.Error()}
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if response.StatusCode != http.StatusOK {
		answer, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		status := response.StatusCode
		return nil, &readFailure{
			message:   fmt.Sprintf("GET %s: %s %s", url, response.Status, bytes.TrimSpace(answer)),
			permanent: status/100 == 4 && status != http.StatusRequestTimeout && status != http.StatusTooManyRequests,
		}
	}
	var entries []poolEntry
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		text := bytes.TrimSpace(scanner.Bytes())
		if len(text) == 0 {
			continue
		}
		var logged struct {
			Position int64           `json:"position"`
			Event    json.RawMessage `json:"event"`
		}
		var event protocol.Event
		if err := protocol.Decode(bytes.NewReader(text), &logged); err != nil {
			return entries, &readFailure{message: fmt.Sprintf("run %s's log: %v", run, err), permanent: true}
		}
		if err := protocol.Decode(bytes.NewReader(logged.Event), &event); err != nil {
			return entries, &readFailure{message: fmt.Sprintf("run %s's log, position %d: %v", run, logged.Position, err), permanent: true}
		}
		var line bytes.Buffer
		json.Compact(&line, logged.Event)
		line.WriteByte('\n')
		entries = append(entries, poolEntry{position: logged.Position, event: event, line: line.Bytes()})
	}
	if err := scanner.Err(); err != nil {
		return entries, &readFailure{message: fmt.Sprintf("reading run %s's log: %v", run, err)}
	}
	return entries, nil
}
