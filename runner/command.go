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

	"github.com/system-inc/loom/protocol"
)

// baseEnvironment is all a unit inherits from the runner's environment; the unit's own variables go on top.
var baseEnvironment = []string{"PATH", "HOME", "TMPDIR", "LANG"}

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

// runCommand starts argv in its own process group and streams its output until it exits. At the timeout
// the whole group gets SIGTERM, then SIGKILL after KillGrace; once the leader exits, whatever it left in
// the group is killed too, so nothing the unit started outlives it. An error means the command never
// started; everything after a start is in the exitOutcome and the events.
func (run *unitRun) runCommand(runContext context.Context) (exitOutcome, error) {
	var outcome exitOutcome
	environment := run.environment()
	path := ""
	for _, variable := range environment {
		if value, found := strings.CutPrefix(variable, "PATH="); found {
			path = value
		}
	}
	executable, err := lookPath(run.unit.Argv[0], path)
	if err != nil {
		return outcome, err
	}
	directory := filepath.Join(run.workspace, filepath.FromSlash(run.unit.Directory))
	if info, err := os.Stat(directory); err != nil || !info.IsDir() {
		return outcome, fmt.Errorf("directory %q isn't a directory in the workspace", run.unit.Directory)
	}

	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return outcome, err
	}
	stderrReader, stderrWriter, err := os.Pipe()
	if err != nil {
		stdoutReader.Close()
		stdoutWriter.Close()
		return outcome, err
	}
	command := &exec.Cmd{
		Path:        executable,
		Args:        run.unit.Argv,
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
		return outcome, err
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
	timeout := time.NewTimer(time.Duration(run.unit.TimeoutSeconds) * time.Second)
	defer timeout.Stop()
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
		run.fail("run", fmt.Errorf("a process outside the unit's group held its output open %v after the command exited; stopped reading", run.options.OutputGrace))
	}
	stdoutReader.Close()
	stderrReader.Close()
	for _, readError := range readErrors {
		if readError != nil && !errors.Is(readError, os.ErrClosed) {
			outcome.readFailed = true
			run.fail("run", fmt.Errorf("reading the command's output: %w", readError))
		}
	}

	state := command.ProcessState
	if state == nil {
		return outcome, fmt.Errorf("waiting for the command: %w", waitError)
	}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		outcome.signal = signalName(status.Signal())
	} else {
		code := state.ExitCode()
		outcome.code = &code
	}
	run.emitter.emit(protocol.Event{
		Type:          "exit",
		Code:          outcome.code,
		Signal:        outcome.signal,
		TimedOut:      outcome.timedOut,
		WallSeconds:   seconds(wall),
		UserSeconds:   seconds(state.UserTime()),
		SystemSeconds: seconds(state.SystemTime()),
	})
	return outcome, nil
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
