package runner

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/housecache"
	"github.com/system-inc/loom/protocol"
)

// baseEnvironment is all a unit inherits from the runner's environment; the unit's own variables go on top.
// LOOM_SLOT and LOOM_SLOT_CPUS are the machine's facts, set by whatever started the runner (the coordinator's
// slot script): which of the box's slots the unit holds and its CPUs, so a unit may use that slot's own warm
// checkout. A unit that reads them depends on its machine, so it isn't cacheable.
var baseEnvironment = []string{"PATH", "HOME", "TMPDIR", "LANG", "LOOM_SLOT", "LOOM_SLOT_CPUS",
	// Where adamic's setup put the toolchain, when the machine's environment says (a Codex instance's does).
	"ADAMIC_TOOLS",
	// How the machine reaches the network: a Codex instance goes through a proxy with its own CA, and a unit
	// that can't reach github or a package registry proves nothing.
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "no_proxy", "all_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "GIT_SSL_CAINFO"}

// An exitOutcome is how the command ended, as the exit event reports it.
type exitOutcome struct {
	code        *int
	signal      string
	timedOut    bool
	interrupted bool // the runner was stopped, not the unit
	readFailed  bool // the output stream couldn't be read whole
}

// environment is the command's whole environment: the base, then the unit's variables, sorted.
func (run *unitRun) environment() []string {
	values := map[string]string{}
	for _, name := range baseEnvironment {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for name, value := range run.unit.Environment {
		values[name] = value
	}
	result := make([]string, 0, len(values))
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result
}

// prepareEnvironment is what prepare.sh runs with: the unit's environment, and the house cache it asks first for the
// gate inputs' chunks, unless there is none or this process is leaving it alone (housecache.Skipping).
func (run *unitRun) prepareEnvironment() []string {
	environment := run.environment()
	if run.options.HouseCache != "" && !housecache.Skipping(run.options.HouseCache) {
		environment = append(environment, housecache.Variable+"="+run.options.HouseCache)
	}
	return environment
}

// lookPath finds a bare command name in the unit's PATH, not the runner's. A name with a slash is used as
// given; a relative one is relative to the unit's directory, where the command starts.
func lookPath(name string, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, directory := range filepath.SplitList(path) {
		if !filepath.IsAbs(directory) {
			continue
		}
		candidate := filepath.Join(directory, name)
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s: not found in the unit's PATH", name)
}

// runCommand runs the unit's argv in the workspace (or its directory) and emits its exit event. An error means
// the command never started; everything after a start is in the exitOutcome and the events.
func (run *unitRun) runCommand(runContext context.Context) (exitOutcome, error) {
	directory := filepath.Join(run.workspace, filepath.FromSlash(run.unit.Directory))
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return exitOutcome{}, fmt.Errorf("directory %q isn't a directory in the workspace", run.unit.Directory)
	}
	outcome, state, wall, err := run.stream(runContext, run.unit.Argv, run.environment(), directory, time.Duration(run.unit.TimeoutSeconds)*time.Second)
	if err != nil {
		return outcome, err
	}
	run.emitExit(outcome, state, wall)
	return outcome, nil
}

// stream starts argv in its own process group and streams its output as events until it exits. At the timeout
// the whole group gets SIGTERM, then SIGKILL after KillGrace; once the leader exits, whatever it left in the group
// is killed too, so nothing it started outlives it. An error means the command never started.
func (run *unitRun) stream(runContext context.Context, argv []string, environment []string, directory string, limit time.Duration) (exitOutcome, *os.ProcessState, time.Duration, error) {
	var outcome exitOutcome
	path := ""
	for _, variable := range environment {
		if value, found := strings.CutPrefix(variable, "PATH="); found {
			path = value
		}
	}
	executable, err := lookPath(argv[0], path)
	if err != nil {
		return outcome, nil, 0, err
	}

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return outcome, nil, 0, err
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		stdoutReader.Close()
		stdoutWriter.Close()
		return outcome, nil, 0, err
	}
	command := &exec.Cmd{
		Path:        executable,
		Args:        argv,
		Env:         environment,
		Dir:         directory,
		Stdout:      stdoutWriter,
		Stderr:      stderrWriter,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	started := time.Now()
	err = command.Start()
	stdoutWriter.Close()
	stderrWriter.Close()
	if err != nil {
		stdoutReader.Close()
		stderrReader.Close()
		return outcome, nil, 0, err
	}
	group := command.Process.Pid

	var readers sync.WaitGroup
	readErrors := make([]error, 2)
	for index, stream := range []struct {
		name   string
		reader *os.File
	}{{"stdout", stdoutReader}, {"stderr", stderrReader}} {
		readers.Add(1)
		go func() {
			defer readers.Done()
			readErrors[index] = splitLines(stream.reader, maximumLineBytes, func(line []byte) {
				run.emitter.emit(outputEvent(stream.name, line))
			})
		}()
	}

	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	timeout := time.NewTimer(limit)
	defer timeout.Stop()
	heartbeat := time.NewTicker(run.options.Heartbeat / 4)
	defer heartbeat.Stop()
	stopped := runContext.Done()
	var escalate <-chan time.Time
	var waitError error
waiting:
	for {
		select {
		case waitError = <-waited:
			break waiting
		case <-timeout.C:
			outcome.timedOut = true
			signalGroup(group, syscall.SIGTERM)
			escalate = time.After(run.options.KillGrace)
		case <-stopped:
			stopped = nil
			outcome.interrupted = true
			signalGroup(group, syscall.SIGTERM)
			escalate = time.After(run.options.KillGrace)
		case <-escalate:
			signalGroup(group, syscall.SIGKILL)
		case <-heartbeat.C:
			run.emitter.beat(run.options.Heartbeat, fmt.Sprintf("loom-runner: still running after %.0f s", time.Since(started).Seconds()))
		}
	}
	wall := time.Since(started)
	signalGroup(group, syscall.SIGKILL)

	readersDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readersDone)
	}()
	select {
	case <-readersDone:
	case <-time.After(run.options.OutputGrace):
		stdoutReader.Close()
		stderrReader.Close()
		<-readersDone
		run.fail(protocol.PhaseRun, fmt.Errorf("a process outside the unit's group held its output open %v after the command exited; stopped reading", run.options.OutputGrace))
	}
	stdoutReader.Close()
	stderrReader.Close()
	for _, readError := range readErrors {
		if readError != nil && !errors.Is(readError, os.ErrClosed) {
			outcome.readFailed = true
			run.fail(protocol.PhaseRun, fmt.Errorf("reading the command's output: %w", readError))
		}
	}

	state := command.ProcessState
	if state == nil {
		return outcome, nil, wall, fmt.Errorf("waiting for the command: %w", waitError)
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		outcome.signal = signalName(status.Signal())
	} else {
		code := state.ExitCode()
		outcome.code = &code
	}
	return outcome, state, wall, nil
}

// emitExit emits the exit event for a command stream ran.
func (run *unitRun) emitExit(outcome exitOutcome, state *os.ProcessState, wall time.Duration) {
	run.emitter.emit(protocol.Event{
		Type:          "exit",
		Code:          outcome.code,
		Signal:        outcome.signal,
		TimedOut:      outcome.timedOut,
		WallSeconds:   seconds(wall),
		UserSeconds:   seconds(state.UserTime()),
		SystemSeconds: seconds(state.SystemTime()),
	})
}

// signalGroup signals every process in the group. A group already gone is not an error.
func signalGroup(group int, signal syscall.Signal) {
	syscall.Kill(-group, signal)
}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "SIGHUP", syscall.SIGINT: "SIGINT", syscall.SIGQUIT: "SIGQUIT", syscall.SIGILL: "SIGILL",
	syscall.SIGTRAP: "SIGTRAP", syscall.SIGABRT: "SIGABRT", syscall.SIGBUS: "SIGBUS", syscall.SIGFPE: "SIGFPE",
	syscall.SIGKILL: "SIGKILL", syscall.SIGUSR1: "SIGUSR1", syscall.SIGSEGV: "SIGSEGV", syscall.SIGUSR2: "SIGUSR2",
	syscall.SIGPIPE: "SIGPIPE", syscall.SIGALRM: "SIGALRM", syscall.SIGTERM: "SIGTERM", syscall.SIGXCPU: "SIGXCPU",
}

func signalName(signal syscall.Signal) string {
	if name, ok := signalNames[signal]; ok {
		return name
	}
	return fmt.Sprintf("signal %d", int(signal))
}

// seconds rounds a duration to the millisecond, the precision an event's times carry.
func seconds(duration time.Duration) float64 {
	return math.Round(duration.Seconds()*1000) / 1000
}
