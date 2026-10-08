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
// The record renumbers each attempt's events and relays them to the wire, which already holds a pool unit's
// events from its runner; the relay's copies are identical, so the wire drops them as replays. A re-placed
// unit is different: a runner always numbers from 0, the wire already holds the unit's earlier sequences,
// so it refuses the new runner's batch as a conflict and the runner gives the wire up. The coordinator
// never hears that attempt, drops it again, and the run is void. Void is honest, never green; a re-placed
// pool unit finishing needs the runner to number from the record's offset, which v1 doesn't do.
type PoolMachine struct {
	// Pool is the pool's name on the wire, such as codex.
	Pool string
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

	mutex     sync.Mutex
	followers map[string]*runFollower // by run
}

func (machine *PoolMachine) Name() string {
	if machine.Label != "" {
		return machine.Label
	}
	return "pool:" + machine.Pool
}

func (machine *PoolMachine) RunnerVersion() string { return machine.Version }

func (machine *PoolMachine) Platform() string { return machine.GoPlatform }

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
	body, err := json.Marshal(map[string][]protocol.Unit{"units": {unit}})
	if err != nil {
		return err
	}
	wire := &wireClient{url: machine.Wire, client: machine.client()}
	if _, err := wire.call(runContext, http.MethodPost, "/pools/"+machine.Pool+"/units", token, body); err != nil {
		return fmt.Errorf("queuing on pool %s: %w", machine.Pool, err)
	}
	for {
		machine.mutex.Lock()
		lines, failure := stream.lines, stream.failure
		stream.lines = nil
		machine.mutex.Unlock()
		for _, line := range lines {
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
		case <-runContext.Done():
			return fmt.Errorf("given up on pool %s; the unit may still be queued or running there", machine.Pool)
		}
	}
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
	text     []byte // the event as a JSON line
	finished bool
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
			stream.lines = append(stream.lines, poolLine{text: entry.line, finished: entry.event.Type == "finished"})
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
