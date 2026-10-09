// Package runner runs one Loom unit on this machine: it fetches the unit's inputs by hash, runs its argv in
// a fresh workspace, streams protocol events as JSON lines (and to the wire when the unit names one), then
// hashes and uploads its outputs. It holds no credential but the unit's token and keeps nothing afterward.
package runner

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/system-inc/loom/poster"
	"github.com/system-inc/loom/protocol"
)

// Version is what started events report as runnerVersion. A release build sets it with
// -ldflags "-X github.com/system-inc/loom/runner.Version=<version>".
var Version = "v0-dev"

// Options are the runner's own settings. None of them comes from the unit.
type Options struct {
	// WorkspaceParent is where the unit's workspace is made; empty means os.TempDir().
	WorkspaceParent string
	// Keep leaves the workspace in place after the run, for a person to look at.
	Keep bool
	// Events receives the event stream, one JSON line per event. The command line passes stdout.
	Events io.Writer
	// Diagnostics receives what can't be an event, such as a wire that lost the events after finished.
	Diagnostics io.Writer
	// Client makes every HTTP request, to the store and to the wire. Nil means a client with sane timeouts.
	Client *http.Client
	// KillGrace is how long a timed-out process group has between SIGTERM and SIGKILL. Zero means 5 s.
	KillGrace time.Duration
	// Heartbeat is how long a unit may go silent before the runner says it's still running, an output event
	// on the runner stream, so whoever watches can tell a quiet unit from a runner that's gone. Zero means 2 min.
	Heartbeat time.Duration
	// OutputGrace is how long the runner keeps reading output after the command exits and its group is
	// killed, for a process that left the group and still holds the pipes. Zero means 2 s.
	OutputGrace time.Duration
	// WireInterval is the longest an event waits before it is posted to the wire. Zero means 250 ms.
	WireInterval time.Duration
	// WireDrainTimeout bounds how long the end of a unit waits for the wire to take its events. Zero means 30 s.
	WireDrainTimeout time.Duration
	// Strict runs only structured test jobs (strict.go): a unit with argv, or anything a test job doesn't take, is
	// refused before anything runs. The Codex pool's runners serve this way.
	Strict bool
	// Root is where a test job keeps what outlives a unit (its npm trees, gate inputs and setup marker) and where its
	// preparation clears earlier units' leavings. Empty means /tmp for a strict runner, whose instance is the runner's
	// alone, and loom-test-root under WorkspaceParent otherwise.
	Root string
	// Tree is where a test job's checkout is kept across units. Empty means adamic under Root.
	Tree string
}

func (options Options) withDefaults() Options {
	if options.WorkspaceParent == "" {
		options.WorkspaceParent = os.TempDir()
	}
	if options.Events == nil {
		options.Events = io.Discard
	}
	if options.Diagnostics == nil {
		options.Diagnostics = io.Discard
	}
	if options.Client == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = time.Minute
		options.Client = &http.Client{Transport: transport}
	}
	if options.Heartbeat == 0 {
		options.Heartbeat = 2 * time.Minute
	}
	if options.KillGrace == 0 {
		options.KillGrace = 5 * time.Second
	}
	if options.OutputGrace == 0 {
		options.OutputGrace = 2 * time.Second
	}
	if options.WireInterval == 0 {
		options.WireInterval = 250 * time.Millisecond
	}
	if options.WireDrainTimeout == 0 {
		options.WireDrainTimeout = 30 * time.Second
	}
	return options
}

// A Result is how one unit ended: its status as the finished event reported it, and the workspace it ran
// in, which is gone by now unless Options.Keep was set.
type Result struct {
	Status    string
	Workspace string
}

// LoadUnit reads a unit from a file, from standard input ("-"), or from an https URL, and decodes it
// strictly. A unit carries its run's token, so a plain http URL is refused.
func LoadUnit(runContext context.Context, source string, client *http.Client) (protocol.Unit, error) {
	var unit protocol.Unit
	var reader io.Reader
	switch {
	case source == "-":
		reader = os.Stdin
	case strings.HasPrefix(source, "https://"):
		if client == nil {
			client = Options{}.withDefaults().Client
		}
		request, err := http.NewRequestWithContext(runContext, http.MethodGet, source, nil)
		if err != nil {
			return unit, err
		}
		response, err := client.Do(request)
		if err != nil {
			return unit, err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return unit, fmt.Errorf("GET %s: %s", source, response.Status)
		}
		reader = io.LimitReader(response.Body, 16<<20)
	case strings.HasPrefix(source, "http://"):
		return unit, fmt.Errorf("a unit carries its run's token; fetch it over https, not %s", source)
	default:
		file, err := os.Open(source)
		if err != nil {
			return unit, err
		}
		defer file.Close()
		reader = file
	}
	if err := protocol.Decode(reader, &unit); err != nil {
		return unit, fmt.Errorf("unit %s: %w", source, err)
	}
	return unit, nil
}

// A unitRun is one unit on its way through the runner.
type unitRun struct {
	unit      protocol.Unit
	options   Options
	emitter   *emitter
	directory string // made for this unit; holds the workspace and the staging area for fetched blobs
	workspace string // where the inputs land and the command runs
	staging   string // fetched blobs wait here, verified, until they are placed
}

// Run runs one unit and streams its events to options.Events. Every path through it ends with exactly one
// finished event, and the event stream is complete whatever happens to the wire. Cancelling runContext
// kills the unit's process group and finishes the unit broken: the runner was stopped, nothing was proved.
func Run(runContext context.Context, unit protocol.Unit, options Options) Result {
	options = options.withDefaults()
	run := &unitRun{unit: unit, options: options}
	run.emitter = &emitter{run: unit.Run, unit: unit.Unit, sequence: max(0, unit.SequenceStart), writer: options.Events, now: time.Now}
	if unit.Wire != nil && unit.Wire.Url != "" {
		run.emitter.wire = poster.New(unit.Wire.Url, unit.Token, options.Client, options.WireInterval)
		run.emitter.wire.Report = func(message string) {
			run.emitter.emit(protocol.Event{Type: "error", Phase: protocol.PhaseWire, Message: message})
		}
		go run.emitter.wire.Loop()
	}

	machine := describeMachine()
	inputHashes := map[string]string{}
	for _, input := range unit.Inputs {
		inputHashes[input.Path] = input.Sha256
	}
	run.emitter.emit(protocol.Event{
		Type:             "started",
		Machine:          machine.name,
		RunnerVersion:    Version,
		Cpus:             machine.cpus,
		MemoryMegabytes:  machine.memoryMegabytes,
		InputHashes:      inputHashes,
		HeartbeatSeconds: options.Heartbeat.Seconds(),
	})

	status := run.execute(runContext)
	if run.directory != "" && !options.Keep {
		if err := removeDirectory(run.directory); err != nil {
			fmt.Fprintf(options.Diagnostics, "loom-runner: removing workspace %s: %v\n", run.directory, err)
		}
	}
	run.finish(status)
	return Result{Status: status, Workspace: run.workspace}
}

// execute is the unit's life between started and finished, and returns the status finished reports.
func (run *unitRun) execute(runContext context.Context) string {
	if err := protocol.CheckUnit(run.unit); err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	if run.options.Strict {
		if err := checkStrict(run.unit); err != nil {
			run.fail(protocol.PhaseStart, err)
			return protocol.StatusBroken
		}
	}
	if err := run.makeWorkspace(); err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	if run.unit.Test != nil {
		// Outputs go up whatever the tests did, as for a command.
		return worse(run.runTest(runContext), run.uploadOutputs(runContext))
	}
	if err := run.fetchInputs(runContext); err != nil {
		run.fail(protocol.PhaseFetch, err)
		return protocol.StatusBroken
	}
	outcome, err := run.runCommand(runContext)
	if err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	status := protocol.StatusPassed
	switch {
	case outcome.interrupted:
		run.fail(protocol.PhaseRun, fmt.Errorf("the runner was stopped before the command finished"))
		status = protocol.StatusBroken
	case outcome.readFailed:
		status = protocol.StatusBroken
	case !outcome.timedOut && outcome.code != nil && run.unit.BrokenExit != 0 && *outcome.code == run.unit.BrokenExit:
		// The unit's own word that its machine couldn't run it (a full disk, a failed checkout): nothing was proved.
		run.fail(protocol.PhaseRun, fmt.Errorf("exit %d, the unit's brokenExit: its machine couldn't run it", *outcome.code))
		status = protocol.StatusBroken
	case outcome.timedOut || outcome.code == nil || *outcome.code != 0:
		status = protocol.StatusFailed
	}
	// Outputs go up whatever the exit, since a failed unit's logs are what a person reads next.
	return worse(status, run.uploadOutputs(runContext))
}

// worse returns the status that says less was proved: broken over failed over passed.
func worse(first, second string) string {
	rank := map[string]int{protocol.StatusPassed: 0, protocol.StatusFailed: 1, protocol.StatusBroken: 2}
	if rank[second] > rank[first] {
		return second
	}
	return first
}

// finish settles the wire and emits finished, the unit's last event. The wire gets every event before
// finished or an error event on stdout saying it didn't; what it misses after finished can only be told to
// Diagnostics, because nothing may follow finished on the stream.
func (run *unitRun) finish(status string) {
	wire := run.emitter.wire
	if wire == nil {
		run.emitter.emit(protocol.Event{Type: "finished", Status: status})
		return
	}
	deadline := time.Now().Add(run.options.WireDrainTimeout)
	if err := wire.Drain(deadline); err != nil {
		wire.Abandon()
		run.fail(protocol.PhaseWire, fmt.Errorf("the wire didn't take every event; stdout holds the whole stream: %w", err))
		run.emitter.emit(protocol.Event{Type: "finished", Status: status})
		return
	}
	run.emitter.emit(protocol.Event{Type: "finished", Status: status})
	if err := wire.Drain(deadline.Add(5 * time.Second)); err != nil {
		fmt.Fprintf(run.options.Diagnostics, "loom-runner: the wire missed the finished event (stdout has it): %v\n", err)
	}
}

func (run *unitRun) fail(phase string, err error) {
	run.emitter.emit(protocol.Event{Type: "error", Phase: phase, Message: err.Error()})
}

func (run *unitRun) makeWorkspace() error {
	// A fresh machine (a Codex instance's first unit) may not have the parent yet.
	if err := os.MkdirAll(run.options.WorkspaceParent, 0o755); err != nil {
		return fmt.Errorf("making the workspace: %w", err)
	}
	directory, err := os.MkdirTemp(run.options.WorkspaceParent, "loom-unit-")
	if err != nil {
		return fmt.Errorf("making the workspace: %w", err)
	}
	run.directory = directory
	run.workspace = filepath.Join(directory, "workspace")
	run.staging = filepath.Join(directory, "staging")
	for _, path := range []string{run.workspace, run.staging} {
		if err := os.Mkdir(path, 0o755); err != nil {
			return fmt.Errorf("making the workspace: %w", err)
		}
	}
	return nil
}

// removeDirectory deletes a unit's directory. Units leave read-only directories behind (Go's module cache
// does), so a first failure makes every directory writable and tries again.
func removeDirectory(directory string) error {
	if os.RemoveAll(directory) == nil {
		return nil
	}
	filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			os.Chmod(path, 0o700)
		}
		return nil
	})
	return os.RemoveAll(directory)
}
