package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/treebuilder"
)

// The shipped unit runs `loom build-trees` with flags it takes, under adamic's toolchain and GOTOOLCHAIN=local (the
// environment a tree's key is read in, the planner's too), and with KillMode=process, so a restart ends the builder
// alone and the next one adopts the build in flight from its running record (#apsj7zp). Mutants: GOTOOLCHAIN=local or
// env.sh taken out; the KillMode line taken out.
func TestTheShippedTreeBuilderUnitBuildsUnderAdamicsToolchain(t *testing.T) {
	unit, err := os.ReadFile("../../treebuilder/systemd/loom-build-trees.service")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unit, []byte("\nEnvironment=GOTOOLCHAIN=local\n")) || !bytes.Contains(unit, []byte("'. %h/adamic-tools/env.sh && exec ")) {
		t.Fatal("the unit doesn't build under adamic's toolchain with GOTOOLCHAIN=local")
	}
	if !bytes.Contains(unit, []byte("\nKillMode=process\n")) {
		t.Fatal("the unit's restart would kill the build in flight: no KillMode=process")
	}
	arguments, err := buildTreesUnitCommand(unit, "/home/loom")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := parseBuildTreesFlags(arguments, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loom build-trees %v: %v", arguments, err)
	}
	if *settings.ledgerPath != "/home/loom/loom-trees/trees.jsonl" || *settings.clone != "/home/loom/loom-trees/adamic" || *settings.floorGB != 100 || *settings.goCacheGB != 500 || *settings.once {
		t.Fatalf("the unit's builder is %v", arguments)
	}
}

// buildTreesUnitCommand is the shipped unit's `loom build-trees` arguments, from inside its bash -c, with %h made home.
func buildTreesUnitCommand(unit []byte, home string) ([]string, error) {
	for _, line := range strings.Split(string(unit), "\n") {
		command, found := strings.CutPrefix(line, "ExecStart=/bin/bash -c '")
		if !found {
			continue
		}
		_, command, _ = strings.Cut(strings.TrimSuffix(command, "'"), "&& exec ")
		words := strings.Fields(strings.ReplaceAll(command, "%h", home))
		if len(words) < 2 || filepath.Base(words[0]) != "loom" || words[1] != "build-trees" {
			return nil, fmt.Errorf("ExecStart %q isn't loom build-trees", command)
		}
		return words[2:], nil
	}
	return nil, fmt.Errorf("no ExecStart=/bin/bash -c")
}

// A build-tree is run with the plan's tree key and the future's commit, its output in a log of the builder's alone; one
// that ends badly says so with the log's last lines, and one past the bound is killed with everything it started.
// Mutants: no bound; only the child killed, not its group.
func TestABuildTreeChildIsBoundedAndSaysHowItEnded(t *testing.T) {
	directory := t.TempDir()
	settings, err := parseBuildTreesFlags([]string{"--queue", "https://queue", "--token-file", "token"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := treebuilder.Want{Tree: strings.Repeat("b", 64), Future: strings.Repeat("2", 40), Go: "go1.27.1"}
	arguments := buildTreeArguments(settings, "/trees/adamic", want)
	if arguments[0] != "build-tree" || !slices.Contains(arguments, "--tree-key") || arguments[slices.Index(arguments, "--tree-key")+1] != want.Tree ||
		arguments[slices.Index(arguments, "--future")+1] != want.Future || arguments[slices.Index(arguments, "--go")+1] != want.Go || arguments[slices.Index(arguments, "--tree")+1] != "/trees/adamic" {
		t.Fatalf("build-tree's arguments: %v", arguments)
	}

	failing := filepath.Join(directory, "failing")
	os.WriteFile(failing, []byte("#!/bin/bash\necho \"build-tree: the plan carries tree key $3\"\nexit 1\n"), 0o755)
	log := filepath.Join(directory, "b.log")
	pids := []int{}
	recorded := func(pid int) error { pids = append(pids, pid); return nil }
	err = runBuildTree(context.Background(), failing, []string{"--tree-key", want.Tree}, log, time.Minute, recorded)
	if err == nil || !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "the plan carries tree key") {
		t.Fatalf("a failing build-tree: %v", err)
	}
	if info, err := os.Stat(log); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("its log: %v %v", info.Mode(), err)
	}
	if len(pids) != 1 || pids[0] <= 0 {
		t.Fatalf("the child's pid was recorded as %v", pids)
	}

	child := filepath.Join(directory, "child")
	hanging := filepath.Join(directory, "hanging")
	os.WriteFile(hanging, []byte("#!/bin/bash\nsleep 30 &\necho $! > \""+child+"\"\nwait\n"), 0o755)
	started := time.Now()
	err = runBuildTree(context.Background(), hanging, nil, log, 300*time.Millisecond, recorded)
	if err == nil || !strings.Contains(err.Error(), "past its 300ms bound") || time.Since(started) > 5*time.Second {
		t.Fatalf("a build-tree that never ends gave %v after %v", err, time.Since(started))
	}
	content, _ := os.ReadFile(child)
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatalf("the stub's child: %q", content)
	}
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("what build-tree started outlived the bound")
		}
	}
}

// The builder's clone is made when missing with the public repository as its origin, and one whose origin is anything
// else is refused: every tree comes from what that repository serves anyone. Mutant: the origin not checked.
func TestTheBuildersCloneHasOnlyThePublicOrigin(t *testing.T) {
	clone := filepath.Join(t.TempDir(), "adamic")
	if err := readyClone(clone); err != nil {
		t.Fatal(err)
	}
	if origin, err := exec.Command("git", "-C", clone, "config", "--get", "remote.origin.url").Output(); err != nil || strings.TrimSpace(string(origin)) != protocol.AdamicRepository {
		t.Fatalf("the clone's origin is %q (%v)", origin, err)
	}
	if err := readyClone(clone); err != nil {
		t.Fatalf("an existing clone: %v", err)
	}
	exec.Command("git", "-C", clone, "remote", "set-url", "origin", "https://github.com/someone/adamic").Run()
	if err := readyClone(clone); err == nil || !strings.Contains(err.Error(), "someone/adamic") {
		t.Fatalf("a clone with another origin: %v", err)
	}
}

// A release's restart mid-build (SIGTERM, #apsj7zp) never kills the build: the builder leaves its child running, its
// running record standing with the child's pid, and the next builder adopts it, waiting for it and recording it built
// from the store, never building the tree again. Mutants: the child killed on the builder's stop; the restarted
// builder building the tree again instead of adopting.
func TestARestartMidBuildLeavesItRunningAndTheNextBuilderAdoptsIt(t *testing.T) {
	key := strings.Repeat("a", 64)
	parts, _ := json.Marshal(planner.KeyParts{Kind: "test", Package: "x", Tools: planner.Tools{Go: "go1.27.1"}})
	source := listedTrees{{Future: strings.Repeat("f", 40), Attempt: 1, Units: []judge.PlannedUnitWire{{UnitKey: strings.Repeat("1", 64), KeyParts: parts, Decision: "run", Tree: key}}}}
	path := filepath.Join(t.TempDir(), "trees.jsonl")
	ledger, err := treebuilder.OpenLedger(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The child: two seconds of building, then its index is up.
	index := filepath.Join(t.TempDir(), "index")
	child := filepath.Join(t.TempDir(), "build-tree")
	os.WriteFile(child, []byte("#!/bin/bash\nsleep 2\ntouch '"+index+"'\n"), 0o755)
	indexed := func(string) (bool, error) { _, err := os.Stat(index); return err == nil, nil }
	alive := func(pid int, _ string) bool { return syscall.Kill(pid, 0) == nil }
	runContext, stop := context.WithCancel(context.Background())
	builds := 0
	loop := &treebuilder.Builder{Source: source, Indexed: indexed, Floor: func() error { return nil },
		Build: func(want treebuilder.Want, running func(int) error) error {
			builds++
			time.AfterFunc(300*time.Millisecond, stop) // the release's SIGTERM
			return runBuildTree(runContext, child, nil, filepath.Join(t.TempDir(), "b.log"), 2*time.Hour, running)
		},
		Alive: alive, Kill: killGroup, Bound: 2 * time.Hour, Poll: 50 * time.Millisecond,
		Stopping: func() bool { return runContext.Err() != nil }, Ledger: ledger, Now: time.Now, Log: io.Discard}
	if _, err := loop.BuildOnce(); err != nil {
		t.Fatal(err)
	}
	ledger.Close()
	reopened, err := treebuilder.OpenLedger(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	newest, _ := reopened.Newest(key)
	if newest.Event != treebuilder.Running || newest.Pid <= 0 || !alive(newest.Pid, key) {
		t.Fatalf("after the stop the build is recorded %+v, alive %v: it must still be running", newest, alive(newest.Pid, key))
	}
	loop.Ledger, loop.Stopping = reopened, func() bool { return false }
	loop.Build = func(treebuilder.Want, func(int) error) error { builds++; return nil }
	if adopted, err := loop.BuildOnce(); err != nil || !adopted {
		t.Fatalf("the restarted builder: adopted %v, %v", adopted, err)
	}
	if newest, _ = reopened.Newest(key); newest.Event != treebuilder.Built || !strings.Contains(newest.Cause, "adopted") || builds != 1 {
		t.Fatalf("the adopted build is recorded %+v after %d builds; want it built once, adopted", newest, builds)
	}
	if built, err := loop.BuildOnce(); err != nil || built || builds != 1 {
		t.Fatalf("after adopting: built %v (%v), %d builds", built, err, builds)
	}
}

type listedTrees []judge.PlannedFuture

func (futures listedTrees) Planned() ([]judge.PlannedFuture, error) { return futures, nil }

// The planner and the tree builder read one tree's key under one environment, so their units start them alike: the
// same Environment lines and the same shell before `exec` (adamic's env.sh, GOTOOLCHAIN=local). Mutant: the planner's
// unit without GOTOOLCHAIN=local.
func TestThePlannerAndTheTreeBuilderShareOneEnvironment(t *testing.T) {
	environment := func(path string) string {
		t.Helper()
		unit, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := []string{}
		for _, line := range strings.Split(string(unit), "\n") {
			if strings.HasPrefix(line, "Environment=") {
				lines = append(lines, line)
			}
			if command, found := strings.CutPrefix(line, "ExecStart="); found {
				before, _, _ := strings.Cut(command, "exec ")
				lines = append(lines, before)
			}
		}
		return strings.Join(lines, "\n")
	}
	planner, builder := environment("../../planner/systemd/loom-plan.service"), environment("../../treebuilder/systemd/loom-build-trees.service")
	if planner != builder || !strings.Contains(planner, "Environment=GOTOOLCHAIN=local") || !strings.Contains(planner, "adamic-tools/env.sh") {
		t.Fatalf("the planner starts under\n%s\nand the tree builder under\n%s", planner, builder)
	}
}

// A checkout that fails (GitHub's 5xx, the network) is a transient failure, retried soon, and one the builder's own stop
// cut short is a stop: neither is the tree's failure. Mutant: a checkout's failure the tree's.
func TestACheckoutHiccupIsTransient(t *testing.T) {
	settings, err := parseBuildTreesFlags([]string{"--queue", "https://queue", "--token-file", "token", "--logs", t.TempDir()}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	failing := func(string) (string, func(), error) {
		return "", nil, errors.New("git fetch --quiet origin: The requested URL returned error: 502")
	}
	want := treebuilder.Want{Tree: strings.Repeat("b", 64), Future: strings.Repeat("2", 40), Go: "go1.27.1"}
	if err := buildWant(context.Background(), failing, "/bin/true", settings, want, func(int) error { return nil }); !errors.Is(err, treebuilder.ErrTransient) || !strings.Contains(err.Error(), "502") {
		t.Fatalf("a checkout's 502: %v", err)
	}
	stopped, stop := context.WithCancel(context.Background())
	stop()
	if err := buildWant(stopped, failing, "/bin/true", settings, want, func(int) error { return nil }); !errors.Is(err, treebuilder.ErrStopped) {
		t.Fatalf("a checkout the stop cut short: %v", err)
	}
}

// A running record's pid is adopted only while it's still that tree's build-tree, read from /proc: another tree's, or
// a pid gone or reused, is no build to wait for. Linux only, where Workshop's builder runs. Mutant: the tree not
// compared.
func TestOnlyThatTreesBuildIsAdopted(t *testing.T) {
	if _, err := os.Stat("/proc/self/cmdline"); err != nil {
		t.Skip("no /proc here")
	}
	key := strings.Repeat("a", 64)
	command := exec.Command("/bin/sh", "-c", "sleep 30; true", "build-tree", "--tree-key", key)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	pid := command.Process.Pid
	defer command.Process.Kill()
	// The shell's own command line shows once it runs.
	for deadline := time.Now().Add(2 * time.Second); !buildAlive(pid, key) && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
	}
	if !buildAlive(pid, key) || buildAlive(pid, strings.Repeat("b", 64)) {
		content, _ := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
		t.Fatalf("pid %d (%q): alive for its tree %v, for another %v", pid, content, buildAlive(pid, key), buildAlive(pid, strings.Repeat("b", 64)))
	}
	command.Process.Kill()
	command.Wait()
	if buildAlive(pid, key) {
		t.Fatal("a build that ended is still adopted")
	}
}
