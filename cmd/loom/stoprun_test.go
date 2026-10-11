package main

import (
	"bytes"
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// TestHelperRunStandIn is no test: run with LOOM_STOPRUN_HELPER=1, it is a stand-in `loom run` process that sleeps
// until a signal ends it, its arguments whatever the test gave it.
func TestHelperRunStandIn(t *testing.T) {
	if os.Getenv("LOOM_STOPRUN_HELPER") != "1" {
		t.Skip("a helper process, run by TestStopRunEndsExactlyItsOwnRun")
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

func standIn(t *testing.T, arguments ...string) *exec.Cmd {
	t.Helper()
	command := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperRunStandIn$", "--"}, arguments...)...)
	command.Env = append(os.Environ(), "LOOM_STOPRUN_HELPER=1")
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { command.Process.Kill(); command.Wait() })
	return command
}

// Only the `loom run` naming exactly the run id gets SIGTERM: future-x-1 never touches future-x-10, nor a process that
// merely mentions the id outside a loom run. A run with no process left is no error (#drrnnkh). Mutants: matching the
// id as a substring, or any process naming it.
func TestStopRunEndsExactlyItsOwnRun(t *testing.T) {
	target := standIn(t, "run", "--uncached", "--run-id", "future-x-1", "/runs/future-x-1.json")
	neighbour := standIn(t, "run", "--uncached", "--run-id", "future-x-10", "/runs/future-x-10.json")
	bystander := standIn(t, "tail", "--run-id", "future-x-1")
	var log bytes.Buffer
	if err := stopRun("future-x-1", &log); err != nil {
		t.Fatal(err)
	}
	ended := make(chan error, 1)
	go func() { ended <- target.Wait() }()
	select {
	case err := <-ended:
		if status, ok := err.(*exec.ExitError); !ok || status.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
			t.Fatalf("the run ended with %v, not SIGTERM", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run never ended")
	}
	for name, command := range map[string]*exec.Cmd{"future-x-10": neighbour, "a process that isn't a loom run": bystander} {
		if err := command.Process.Signal(syscall.Signal(0)); err != nil {
			t.Errorf("%s was ended: %v", name, err)
		}
	}
	// Gone already: no error.
	if err := stopRun("future-x-1", &log); err != nil {
		t.Fatalf("a run already ended: %v", err)
	}
}

func TestRunProcessesReadsTheRunIdAsOneToken(t *testing.T) {
	listing := "  101 /home/ahra/.loom/bin/loom run --uncached --run-id future-aa-1 /runs/future-aa-1.json\n" +
		"  102 /home/ahra/.loom/bin/loom run --uncached --run-id future-aa-10 /runs/future-aa-10.json\n" +
		"  103 grep --run-id future-aa-1\n" +
		"  104 /home/ahra/.loom/bin/loom place --queue https://loom.system.inc\n" +
		"  junk\n"
	if pids := runProcesses(listing, "future-aa-1"); !reflect.DeepEqual(pids, []int{101}) {
		t.Fatalf("pids %v", pids)
	}
}
