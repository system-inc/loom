package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/protocol"
)

// ServeOptions are what a serving runner needs beyond each unit's own Options. docs/protocol.md, "The pool",
// is the contract: a machine Loom can't ssh into asks its pool for units until its deadline.
type ServeOptions struct {
	// Pool is the pool's address on the wire, <wire>/pools/<pool>.
	Pool string
	// Token is the pool token: it reaches this pool's next and nothing else.
	Token string
	// Worker names this instance to the pool, which shows it in the pool's status.
	Worker string
	// Deadline is when serving ends. A unit in hand runs to its finish; no new one is asked for once less
	// than Margin remains, so a unit taken has time to run.
	Deadline time.Time
	// Margin is how much time must remain before the deadline to ask for a new unit. Zero means a minute.
	Margin time.Duration
	// Unit is the Options every unit runs with. Each unit posts its own events to the wire it names.
	Unit Options
	// Report receives what goes wrong with the pool itself, one line each; a unit's events go to
	// Unit.Events. Nil discards it.
	Report io.Writer
	// MinimumFreeMegabytes is the room a unit needs on the instance (#zzmz489): below it on the workspace's disk or a
	// strict runner's root, serve asks the pool for nothing and looks again every UnfitPause, so a full instance stops
	// taking units it can only break (Oct 9: instances at 0.15 s a unit drained the queue from 21:00Z). Zero means
	// 1500, the floor of today's units; a prebuilt unit refuses itself under Unit.FreeFloorBytes.
	MinimumFreeMegabytes int64
	// UnfitPause is how long serve waits before looking at an unfit disk again. Zero means 30 s.
	UnfitPause time.Duration
	// Releases is where a unit's runner is fetched by its sha256 when the unit names one other than this runner
	// (runners.go). Empty means DefaultReleases.
	Releases string
	// Drain, once it delivers or closes, ends serving as the deadline does: nothing more is asked for, and the unit in
	// hand runs to its finish. A box's loom-serve unit drains on SIGHUP, so a release restarts serve and breaks nothing.
	Drain <-chan struct{}
	// LiveStatus is serve's live status for `loom top` (livestatus.ServePath under the root): the unit in hand, its
	// phase until its runner takes over Unit.LiveStatus, the totals and the recent units, carried over from the last
	// serve's file. Empty writes none.
	LiveStatus string
	// checkRunner says whether a fetched binary is a loom-runner for this platform; nil means its build information
	// (runners.go). Tests running a stand-in pass one that takes it.
	checkRunner func(path string) error
	// freeMegabytes reads a path's free room; nil means the file system's. Tests plant a full disk through it.
	freeMegabytes func(path string) (int64, error)
	// trim clears what earlier units left on a strict runner's root, and an exclusive one's HOME (prepare.sh trim-only);
	// nil means that script.
	trim func(trimContext context.Context, root string, exclusive bool, report io.Writer) error
	// Units is the most units serve runs at once (#ef2rgaq): a box's 64 threads and 125 GB hold several, and one at a
	// time left most of each idle. Zero or one runs one at a time. Only a prebuilt test job on serve's own runner runs
	// beside others (slots.go); any other unit waits for the units in hand to finish and runs alone.
	Units int
	// UnitCpus and UnitMemoryMegabytes are a unit's share when it declares none (protocol.Resources). Zero means 8 and
	// 16384, the strict tier's memory, so any unit that ran on a Codex instance fits its share.
	UnitCpus            int
	UnitMemoryMegabytes int
	// Busy is the fraction of the machine's CPU time serve lets the machine reach before it asks for another unit while
	// any is in hand. Zero means 0.8, Workshop's admission target.
	Busy float64
	// busy reads how busy the machine's CPUs were since its last call; nil means /proc/stat's (busyMeter). Tests plant a
	// load through it.
	busy func() (float64, bool)
	// machine is the machine whose threads and memory the shares in hand are held to; nil means this one. Tests plant one.
	machine *machine
	// alongside says whether a unit may run beside others; nil means slots.go's alongside. Tests let a command unit.
	alongside func(unit protocol.Unit) bool
}

// A ServeSummary is how a serving runner ended: how many units it ran and how each finished, how long it
// served, and why it stopped. Its String is the one line a Codex turn shows.
type ServeSummary struct {
	Units   int
	Passed  int
	Failed  int
	Broken  int
	Seconds float64
	Stopped string
	// Unfit says why the instance couldn't take a unit when serving ended ("1200 MB free on /tmp"), or is empty. The
	// summary line then ends "unfit: ...", which rearm.sh reads to leave the instance alone.
	Unfit string
}

func (summary ServeSummary) String() string {
	line := fmt.Sprintf("loom-runner serve: %d units, %d passed, %d failed, %d broken in %.0f s; stopped %s",
		summary.Units, summary.Passed, summary.Failed, summary.Broken, summary.Seconds, summary.Stopped)
	if summary.Unfit != "" {
		line += "; unfit: " + summary.Unfit
	}
	return line
}

// errPoolRefused is an answer from the pool that asking again can't change: a token it doesn't take, a pool
// it doesn't have, a request it can't read.
var errPoolRefused = errors.New("the pool refused")

// Serve asks the pool for units and runs them until the deadline or a drain, either of which lets the units in hand
// finish, or until serveContext is cancelled, which stops them (each finishes broken, as with any stopped runner). With
// Units over one it runs several at once (slots.go): it asks for another only while the shares in hand leave room for
// a unit of the default share and the machine is under its busy target, and a unit that can't run beside others waits
// for the units in hand to finish and runs alone. The error is the pool refusing this runner; the summary is always
// filled in.
func Serve(serveContext context.Context, options ServeOptions) (ServeSummary, error) {
	started := time.Now()
	unitOptions := options.Unit.withDefaults()
	if unitOptions.Machine == "" {
		// Its started events name the worker the pool shows, so the board ties the unit to it.
		unitOptions.Machine = options.Worker
	}
	if options.Margin == 0 {
		options.Margin = time.Minute
	}
	if options.Report == nil {
		options.Report = io.Discard
	}
	if options.MinimumFreeMegabytes == 0 {
		// Today's units' floor. A prebuilt unit keeps its own, Unit.FreeFloorBytes, and refuses itself under it.
		options.MinimumFreeMegabytes = 1500
	}
	if options.UnfitPause == 0 {
		options.UnfitPause = 30 * time.Second
	}
	if options.Releases == "" {
		options.Releases = DefaultReleases
	}
	if options.freeMegabytes == nil {
		options.freeMegabytes = freeMegabytes
	}
	if options.trim == nil {
		options.trim = trimRoot
	}
	options.Units = max(1, options.Units)
	if options.UnitCpus == 0 {
		options.UnitCpus = 8
	}
	if options.UnitMemoryMegabytes == 0 {
		options.UnitMemoryMegabytes = 16384
	}
	if options.Busy == 0 {
		options.Busy = 0.8
	}
	if options.alongside == nil {
		options.alongside = alongside
	}
	if options.busy == nil {
		meter := &busyMeter{}
		options.busy = meter.busy
	}
	// The disks a unit writes: its workspace's, and a strict runner's root, where its test job keeps the checkout.
	disks := []string{unitOptions.WorkspaceParent}
	root := unitOptions.Root
	if root == "" && unitOptions.Strict {
		root = unitOptions.testRoot()
	}
	if root != "" {
		disks = append(disks, root)
	}
	machine := describeMachine()
	if options.machine != nil {
		machine = *options.machine
	}
	// What the units in hand may hold between them: every thread, and nine tenths of the memory, since a share's memory
	// is a ceiling its cgroup kills at, never an average.
	capacity := share{cpus: machine.cpus, memoryMegabytes: machine.memoryMegabytes * 9 / 10}
	var cgroups *unitCgroups
	if options.Units > 1 {
		var err error
		if cgroups, err = delegatedCgroups(); err != nil {
			fmt.Fprintf(options.Report, "loom-runner serve: units run without a cgroup of their own: %v\n", err)
		}
	}
	// A drain ends every wait at once, but never an ask in flight, whose unit the pool has already taken off its queue,
	// and never a unit in hand.
	drainContext, drained := context.WithCancel(serveContext)
	defer drained()
	go func() {
		select {
		case <-options.Drain:
			drained()
		case <-drainContext.Done():
		}
	}()
	summary := ServeSummary{}
	failures := 0
	live := startLive(options, started)
	defer func() {
		live.Update(func(status *livestatus.Status) {
			status.Unit, status.Stopped, status.Unfit = nil, summary.Stopped, summary.Unfit
		})
		live.Close()
	}()
	runners := runnerCache{directory: filepath.Join(unitOptions.testRoot(), runnerDirectoryName), releases: options.Releases, client: unitOptions.Client,
		house: unitOptions.HouseCache, houseClient: unitOptions.houseClient, check: options.checkRunner, live: live}
	// unhad counts the units in a row whose runner couldn't be had: past two, serve waits a little before it asks again,
	// so a store that is down doesn't void a whole queue in seconds. Any unit whose runner was had starts it over.
	unhad := 0
	// askAfter is when serve may ask again after that wait.
	askAfter := time.Time{}
	// The units in hand, each running in its own goroutine, which says it finished on finished. Only this loop reads
	// or changes them.
	type inHand struct {
		unit    protocol.Unit
		share   *share
		live    *livestatus.Unit
		started time.Time
	}
	type finish struct {
		hand   *inHand
		result Result
		had    bool
	}
	hands := map[*inHand]bool{}
	// holding is the shares of the units in hand, alone whether one runs alone.
	holding := share{}
	alone := false
	finished := make(chan finish, options.Units)
	settle := func(done finish) {
		delete(hands, done.hand)
		if done.hand.share != nil {
			holding.cpus -= done.hand.share.cpus
			holding.memoryMegabytes -= done.hand.share.memoryMegabytes
		}
		alone = false
		var next *livestatus.Unit
		for other := range hands {
			if next == nil || other.live.StartedAt.Before(next.StartedAt) {
				next = other.live
			}
		}
		finishLive(live, done.hand.unit, done.result.Status, done.hand.started, next, done.hand.share == nil, unitOptions.LiveStatus)
		summary.Units++
		switch done.result.Status {
		case protocol.StatusPassed:
			summary.Passed++
		case protocol.StatusFailed:
			summary.Failed++
		default:
			summary.Broken++
		}
		if done.had {
			unhad = 0
			return
		}
		unhad++
		fmt.Fprintf(options.Report, "loom-runner serve: unit %s's runner couldn't be had (%d in a row)\n", done.hand.unit.Unit, unhad)
		if unhad > 2 {
			askAfter = time.Now().Add(serveBackoff(unhad - 2))
		}
	}
	// rest waits for delay, settling every unit that finishes meanwhile, and ends early at the first one, or at a drain,
	// never past until.
	rest := func(until time.Time, delay time.Duration) {
		delay = max(0, min(delay, time.Until(until)))
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case done := <-finished:
			settle(done)
		case <-drainContext.Done():
		case <-timer.C:
		}
	}
	// start runs a unit in its own goroutine on its share (nil: alone, on the whole machine).
	start := func(unit protocol.Unit, unitShare *share) {
		hand := &inHand{unit: unit, share: unitShare, live: liveUnit(unit, livestatus.PhaseStarting, time.Now()), started: time.Now()}
		hands[hand] = true
		ownOptions, ownRunners := unitOptions, runners
		if unitShare != nil {
			holding.cpus += unitShare.cpus
			holding.memoryMegabytes += unitShare.memoryMegabytes
			ownOptions.share = unitShare
			// Several units at once can't share one live status file, nor serve's phase for the unit in hand.
			ownOptions.LiveStatus, ownRunners.live = "", nil
		} else {
			alone = true
		}
		live.Update(func(status *livestatus.Status) { status.Unit = hand.live })
		go func() {
			result, had := ownRunners.run(serveContext, unit, ownOptions)
			if unitShare != nil && unitShare.cgroup != nil {
				fmt.Fprintf(options.Report, "loom-runner serve: unit %s %s on %d cpus, peak %d MB of its %d MB\n", unit.Unit, result.Status,
					unitShare.cpus, unitShare.cgroup.peakMegabytes(), unitShare.memoryMegabytes)
				unitShare.cgroup.remove()
			}
			finished <- finish{hand: hand, result: result, had: had}
		}()
	}
	cgroupsMade := 0
	for {
		if serveContext.Err() != nil {
			summary.Stopped = "by a signal"
			break
		}
		if drainContext.Err() != nil {
			summary.Stopped = "on a drain"
			break
		}
		if time.Until(options.Deadline) < options.Margin {
			summary.Stopped = "at the deadline"
			break
		}
		askUntil := options.Deadline.Add(-options.Margin)
		// Room for one more: no unit running alone, fewer than Units in hand, shares left for a default one, and, with any
		// in hand, the machine under its busy target.
		if alone || len(hands) >= options.Units || holding.cpus+options.UnitCpus > capacity.cpus && len(hands) > 0 ||
			holding.memoryMegabytes+options.UnitMemoryMegabytes > capacity.memoryMegabytes && len(hands) > 0 {
			rest(askUntil, time.Hour)
			continue
		}
		if len(hands) > 0 {
			busy, ok := options.busy()
			if !ok || busy >= options.Busy {
				rest(askUntil, 5*time.Second)
				continue
			}
		}
		// The blob cache gives up what it must before the disks are read, so a full cache never stands an instance down;
		// a disk under the floor with the cache empty is unfit just below.
		if err := readyRoot(unitOptions, unitOptions.testRoot()); err != nil && !errors.Is(err, errUnfit) {
			fmt.Fprintf(options.Report, "loom-runner serve: readying the blob cache: %v\n", err)
		}
		unfit := unfitDisk(options, disks)
		// What earlier units left on a strict runner's root is no one's once no unit holds it (and an exclusive one's HOME
		// caches too): the first time serve finds no room with no unit in hand, it clears that once, as every unit's
		// preparation does, and looks again. A runner that isn't strict only stands down.
		if unfit != "" && summary.Unfit == "" && unitOptions.Strict && len(hands) == 0 {
			fmt.Fprintf(options.Report, "loom-runner serve: unfit: %s; trimming earlier units' leavings on %s\n", unfit, root)
			trimContext, cancel := context.WithTimeout(serveContext, 5*time.Minute)
			trimAlone(root, func() {
				if err := options.trim(trimContext, root, unitOptions.Exclusive, options.Report); err != nil {
					fmt.Fprintf(options.Report, "loom-runner serve: trimming %s: %v\n", root, err)
				}
			})
			cancel()
			unfit = unfitDisk(options, disks)
		}
		if unfit != summary.Unfit {
			if unfit != "" {
				fmt.Fprintf(options.Report, "loom-runner serve: unfit: %s; asking for no unit until there is room\n", unfit)
			} else {
				fmt.Fprintf(options.Report, "loom-runner serve: room again; asking for units\n")
			}
			summary.Unfit = unfit
			live.Update(func(status *livestatus.Status) { status.Unfit = unfit })
		}
		if summary.Unfit != "" {
			rest(askUntil, options.UnfitPause)
			continue
		}
		if wait := time.Until(askAfter); wait > 0 {
			// The last few units' runners weren't had: a store that is down.
			rest(askUntil, wait)
			continue
		}
		asked := time.Now()
		unit, found, err := askForUnit(serveContext, options, unitOptions.Client, machine.cpus)
		var unreadable *unreadableUnit
		switch {
		case serveContext.Err() != nil:
			continue
		case errors.Is(err, errPoolRefused):
			fmt.Fprintf(options.Report, "loom-runner serve: %v\n", err)
			for len(hands) > 0 {
				settle(<-finished)
			}
			summary.Stopped = "because " + err.Error()
			summary.Seconds = time.Since(started).Seconds()
			return summary, err
		case errors.As(err, &unreadable):
			// The pool took it off its queue and it can't run here; the coordinator sees it dropped.
			summary.Units++
			summary.Broken++
			live.Update(func(status *livestatus.Status) {
				status.Totals.Units, status.Totals.Broken = status.Totals.Units+1, status.Totals.Broken+1
			})
			fmt.Fprintf(options.Report, "loom-runner serve: %v\n", err)
			continue
		case err != nil:
			failures++
			if failures == 1 {
				fmt.Fprintf(options.Report, "loom-runner serve: asking the pool failed, retrying: %v\n", err)
			}
			rest(askUntil, serveBackoff(failures))
			continue
		}
		failures = 0
		live.Update(func(status *livestatus.Status) { status.AskedAt = time.Now() })
		if !found {
			// The pool waits up to 20 s before it says none; one that answers at once mustn't be asked in a spin.
			rest(askUntil, time.Second-time.Since(asked))
			continue
		}
		// The unit is this serve's now, whatever the wait: the pool took it off its queue.
		if options.Units == 1 || !options.alongside(unit) {
			waited := time.Now()
			for len(hands) > 0 {
				settle(<-finished)
			}
			if since := time.Since(waited); since > time.Second {
				fmt.Fprintf(options.Report, "loom-runner serve: unit %s runs alone; it waited %.0f s for the units in hand\n", unit.Unit, since.Seconds())
			}
			start(unit, nil)
			continue
		}
		unitShare := &share{cpus: options.UnitCpus, memoryMegabytes: options.UnitMemoryMegabytes}
		if unit.Resources.Cpus > 0 {
			unitShare.cpus = unit.Resources.Cpus
		}
		if unit.Resources.MemoryMegabytes > 0 {
			unitShare.memoryMegabytes = unit.Resources.MemoryMegabytes
		}
		// A unit that declares more than the machine has gets all of it, alone among the shares.
		unitShare.cpus, unitShare.memoryMegabytes = min(unitShare.cpus, capacity.cpus), min(unitShare.memoryMegabytes, capacity.memoryMegabytes)
		waited := time.Now()
		for holding.cpus+unitShare.cpus > capacity.cpus || holding.memoryMegabytes+unitShare.memoryMegabytes > capacity.memoryMegabytes {
			settle(<-finished)
		}
		if since := time.Since(waited); since > time.Second {
			fmt.Fprintf(options.Report, "loom-runner serve: unit %s waited %.0f s for room for its %d cpus and %d MB\n", unit.Unit, since.Seconds(), unitShare.cpus, unitShare.memoryMegabytes)
		}
		cgroupsMade++
		group, err := cgroups.make(fmt.Sprintf("%d-%d", os.Getpid(), cgroupsMade), unitShare.cpus, unitShare.memoryMegabytes)
		if err != nil {
			fmt.Fprintf(options.Report, "loom-runner serve: unit %s runs without a cgroup of its own: %v\n", unit.Unit, err)
		}
		unitShare.cgroup = group
		start(unit, unitShare)
	}
	// Serving has stopped: every unit in hand runs to its finish (or stops with serveContext).
	for len(hands) > 0 {
		settle(<-finished)
	}
	summary.Seconds = time.Since(started).Seconds()
	return summary, nil
}

// ReadTokenFile reads the pool token from a file and removes the file, so the token is never on the runner's command
// line (where any process of the same user, a unit's tests included, could read it from /proc) and never left on disk.
func ReadTokenFile(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the pool token file: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("removing the pool token file after reading it: %w", err)
	}
	token := strings.TrimSpace(string(content))
	if token == "" {
		return "", fmt.Errorf("the pool token file %s is empty", path)
	}
	return token, nil
}

// trimRoot runs prepare.sh trim-only on the root, the script read from stdin, so a full disk needn't hold a copy. Only
// an exclusive runner's trim reaches HOME's caches.
func trimRoot(trimContext context.Context, root string, exclusive bool, report io.Writer) error {
	command := exec.CommandContext(trimContext, "bash", "-s", "--", "trim-only", root, owner(Options{Exclusive: exclusive}))
	command.Stdin = bytes.NewReader(prepareScript)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	command.Stdout, command.Stderr = report, report
	return command.Run()
}

// unfitDisk names the first disk with less room than a unit needs ("1200 MB free on /tmp"), or is empty. A disk it
// can't read counts as unfit: serving blind is how a full instance eats the queue.
func unfitDisk(options ServeOptions, disks []string) string {
	for _, disk := range disks {
		free, err := options.freeMegabytes(disk)
		if err != nil {
			return fmt.Sprintf("free room on %s unreadable: %v", disk, err)
		}
		if free < options.MinimumFreeMegabytes {
			return fmt.Sprintf("%d MB free on %s, under %d", free, disk, options.MinimumFreeMegabytes)
		}
	}
	return ""
}

// freeMegabytes is the room an unprivileged process may use on path's file system, read at its nearest existing
// directory, since a fresh instance makes its workspace only with its first unit.
func freeMegabytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	err := syscall.Statfs(path, &stat)
	for errors.Is(err, syscall.ENOENT) && filepath.Dir(path) != path {
		path = filepath.Dir(path)
		err = syscall.Statfs(path, &stat)
	}
	if err != nil {
		return 0, err
	}
	return int64(stat.Bavail * uint64(stat.Bsize) >> 20), nil
}

// serveBackoff doubles from a second to at most thirty while the pool keeps failing.
func serveBackoff(failures int) time.Duration {
	return min(time.Second<<min(failures-1, 5), 30*time.Second)
}

// pause waits for delay, but never past until and never past a cancellation.
func pause(pauseContext context.Context, until time.Time, delay time.Duration) {
	delay = min(delay, time.Until(until))
	if delay <= 0 {
		return
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-pauseContext.Done():
	case <-timer.C:
	}
}

// unreadableUnit is a 200 from the pool whose body isn't a unit.
type unreadableUnit struct{ err error }

func (unreadable *unreadableUnit) Error() string {
	return "the pool handed out a unit that doesn't decode: " + unreadable.err.Error()
}

// askForUnit asks the pool for its next unit once: found is false when the pool had none within its wait.
// The request isn't cut at the deadline, because a unit the pool has taken off its queue must be run here or
// it is lost; serving stops asking at Margin before the deadline instead, which covers the pool's 20 s wait.
func askForUnit(askContext context.Context, options ServeOptions, client *http.Client, cpus int) (protocol.Unit, bool, error) {
	var unit protocol.Unit
	body, err := json.Marshal(struct {
		Worker string `json:"worker"`
		Cpus   int    `json:"cpus"`
	}{options.Worker, cpus})
	if err != nil {
		return unit, false, err
	}
	requestContext, cancel := context.WithTimeout(askContext, 90*time.Second)
	defer cancel()
	url := strings.TrimSuffix(options.Pool, "/") + "/next"
	request, err := http.NewRequestWithContext(requestContext, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return unit, false, fmt.Errorf("%w: %v", errPoolRefused, err)
	}
	request.Header.Set("Authorization", "Bearer "+options.Token)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return unit, false, err
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == http.StatusOK:
		if err := protocol.Decode(io.LimitReader(response.Body, 16<<20), &unit); err != nil {
			return unit, false, &unreadableUnit{err: err}
		}
		return unit, true, nil
	case response.StatusCode == http.StatusNoContent:
		return unit, false, nil
	}
	answer, _ := io.ReadAll(io.LimitReader(response.Body, 512))
	failure := fmt.Sprintf("POST %s: %s %s", url, response.Status, bytes.TrimSpace(answer))
	if response.StatusCode/100 == 4 && response.StatusCode != http.StatusRequestTimeout && response.StatusCode != http.StatusTooManyRequests {
		return unit, false, fmt.Errorf("%w: %s", errPoolRefused, failure)
	}
	return unit, false, errors.New(failure)
}
