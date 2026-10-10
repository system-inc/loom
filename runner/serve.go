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
	// the units' own floor, Unit.FreeFloorBytes.
	MinimumFreeMegabytes int64
	// UnfitPause is how long serve waits before looking at an unfit disk again. Zero means 30 s.
	UnfitPause time.Duration
	// freeMegabytes reads a path's free room; nil means the file system's. Tests plant a full disk through it.
	freeMegabytes func(path string) (int64, error)
	// trim clears what earlier units left on a strict runner's root (prepare.sh trim-only); nil means that script.
	trim func(trimContext context.Context, root string, report io.Writer) error
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

// Serve asks the pool for units and runs them one at a time until the deadline, or until serveContext is
// cancelled, which stops the unit in hand (it finishes broken, as with any stopped runner). The error is
// the pool refusing this runner; the summary is always filled in.
func Serve(serveContext context.Context, options ServeOptions) (ServeSummary, error) {
	started := time.Now()
	unitOptions := options.Unit.withDefaults()
	if options.Margin == 0 {
		options.Margin = time.Minute
	}
	if options.Report == nil {
		options.Report = io.Discard
	}
	if options.MinimumFreeMegabytes == 0 {
		// A prebuilt unit refuses itself under the runner's free floor, so serve stands down at the same room, or it
		// would take units only to refuse them.
		options.MinimumFreeMegabytes = int64(unitOptions.FreeFloorBytes >> 20)
	}
	if options.UnfitPause == 0 {
		options.UnfitPause = 30 * time.Second
	}
	if options.freeMegabytes == nil {
		options.freeMegabytes = freeMegabytes
	}
	if options.trim == nil {
		options.trim = trimRoot
	}
	// The disks a unit writes: its workspace's, and a strict runner's root, where its test job keeps the checkout.
	disks := []string{unitOptions.WorkspaceParent}
	root := unitOptions.Root
	if root == "" && unitOptions.Strict {
		root = "/tmp"
	}
	if root != "" {
		disks = append(disks, root)
	}
	cpus := describeMachine().cpus
	summary := ServeSummary{}
	failures := 0
	for {
		if serveContext.Err() != nil {
			summary.Stopped = "by a signal"
			break
		}
		if time.Until(options.Deadline) < options.Margin {
			summary.Stopped = "at the deadline"
			break
		}
		// The blob cache gives up what it must before the disks are read, so a full cache never stands an instance down;
		// a disk under the floor with the cache empty is unfit just below.
		ready := newBlobCache(unitOptions, unitOptions.testRoot()).ready()
		if errors.Is(ready, errUnfit) {
			// Then every tree's source no unit holds.
			newSourceCache(unitOptions.testRoot()).trim(0)
		} else if ready != nil {
			fmt.Fprintf(options.Report, "loom-runner serve: readying the blob cache: %v\n", ready)
		}
		unfit := unfitDisk(options, disks)
		// A strict runner's instance is its alone, so what earlier units left on its root is no one's: the first time
		// it finds no room it clears that once, as every unit's preparation does, and looks again. A box worker shares
		// its machine, so it only stands down.
		if unfit != "" && summary.Unfit == "" && unitOptions.Strict {
			fmt.Fprintf(options.Report, "loom-runner serve: unfit: %s; trimming earlier units' leavings on %s\n", unfit, root)
			trimContext, cancel := context.WithTimeout(serveContext, 5*time.Minute)
			if err := options.trim(trimContext, root, options.Report); err != nil {
				fmt.Fprintf(options.Report, "loom-runner serve: trimming %s: %v\n", root, err)
			}
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
		}
		if summary.Unfit != "" {
			pause(serveContext, options.Deadline.Add(-options.Margin), options.UnfitPause)
			continue
		}
		asked := time.Now()
		unit, found, err := askForUnit(serveContext, options, unitOptions.Client, cpus)
		var unreadable *unreadableUnit
		switch {
		case serveContext.Err() != nil:
			continue
		case errors.Is(err, errPoolRefused):
			fmt.Fprintf(options.Report, "loom-runner serve: %v\n", err)
			summary.Stopped = "because " + err.Error()
			summary.Seconds = time.Since(started).Seconds()
			return summary, err
		case errors.As(err, &unreadable):
			// The pool took it off its queue and it can't run here; the coordinator sees it dropped.
			summary.Units++
			summary.Broken++
			fmt.Fprintf(options.Report, "loom-runner serve: %v\n", err)
			continue
		case err != nil:
			failures++
			if failures == 1 {
				fmt.Fprintf(options.Report, "loom-runner serve: asking the pool failed, retrying: %v\n", err)
			}
			pause(serveContext, options.Deadline.Add(-options.Margin), serveBackoff(failures))
			continue
		}
		failures = 0
		if !found {
			// The pool waits up to 20 s before it says none; one that answers at once mustn't be asked in a spin.
			pause(serveContext, options.Deadline.Add(-options.Margin), time.Second-time.Since(asked))
			continue
		}
		result := Run(serveContext, unit, unitOptions)
		summary.Units++
		switch result.Status {
		case protocol.StatusPassed:
			summary.Passed++
		case protocol.StatusFailed:
			summary.Failed++
		default:
			summary.Broken++
		}
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

// trimRoot runs prepare.sh trim-only on the root, the script read from stdin, so a full disk needn't hold a copy.
func trimRoot(trimContext context.Context, root string, report io.Writer) error {
	command := exec.CommandContext(trimContext, "bash", "-s", "--", "trim-only", root)
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
