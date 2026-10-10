package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/placer"
)

// A placement's `loom run` is the future's run id, uncached, on its pools alone, each strict with the toolchains,
// kinds, cpus and memory the placer chose it by, so the coordinator's own fit can't move a unit off its pool.
func TestAPlacementRunsOnItsPoolsUnderTheFuturesRunId(t *testing.T) {
	tree := strings.Repeat("a", 40)
	phase := placer.RunPool{Pool: placer.Pool{PoolEntry: judge.PoolEntry{Name: "box-phase", MemoryMegabytes: 65536, Cpus: 8, Kinds: []string{"phase"}}, Has: []string{"go", "clang"}}, Slots: 1}
	codex := placer.RunPool{Pool: placer.Pool{PoolEntry: judge.PoolEntry{Name: "codex-strict", MemoryMegabytes: 16384, Cpus: 4}}, Slots: 3}
	arguments := strings.Join(runArguments(placer.Placement{Run: "future-" + tree + "-2", Pools: []placer.RunPool{phase, codex}},
		runOptions{runs: "/runs", wire: "https://wire", source: "/loom", priority: 40}), " ")
	for _, want := range []string{"run --uncached --slots none --run-id future-" + tree + "-2 ", "--priority 40", "--silence-drop 1800",
		"--strict-pool box-phase=1 --pool-cpus box-phase=8 --pool-memory box-phase=65536 --pool-has box-phase=go,clang --pool-kinds box-phase=phase",
		"--strict-pool codex-strict=3 --pool-cpus codex-strict=4 --pool-memory codex-strict=16384 /runs/future-" + tree + "-2.json"} {
		if !strings.Contains(arguments, want) {
			t.Errorf("loom %s\nlacks %q", arguments, want)
		}
	}
	if strings.Contains(arguments, "--pool-has codex-strict") {
		t.Errorf("a pool with no toolchains was given some: %s", arguments)
	}
}

// --pool-has naming a pool the table doesn't hold stops the placer at start; once running, a pool taken out of the
// table only warns, once, and every other pool is still read. Mutant: the reader refusing it after start too.
func TestAPoolLeavingTheTableWarnsAndPlacementGoesOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pools.json")
	runner := strings.Repeat("d", 64)
	write := func(names ...string) {
		pools := []string{}
		for _, name := range names {
			pools = append(pools, fmt.Sprintf(`{"name": %q, "tier": "t", "runner": %q, "memoryMegabytes": 1024, "cpus": 2}`, name, runner))
		}
		os.WriteFile(path, []byte(`{"pools": [`+strings.Join(pools, ",")+`]}`), 0o644)
	}
	has := poolHasFlag{}
	has.Set("codex-strict=go,wasiSdk")
	has.Set("box-phase=go")
	write("codex-strict")
	var warned bytes.Buffer
	if _, err := poolReader(path, has, &warned); err == nil || !strings.Contains(err.Error(), "box-phase") {
		t.Fatalf("a --pool-has naming no pool was taken at start (%v)", err)
	}
	write("codex-strict", "box-phase")
	read, err := poolReader(path, has, &warned)
	if err != nil {
		t.Fatal(err)
	}
	write("codex-strict")
	for range 2 {
		pools, err := read()
		if err != nil || len(pools) != 1 || strings.Join(pools[0].Has, ",") != "go,wasiSdk" {
			t.Fatalf("pools %+v (%v)", pools, err)
		}
	}
	if strings.Count(warned.String(), "pool box-phase left the pool table") != 1 {
		t.Fatalf("warned %q, want once", warned.String())
	}
}

// The shipped unit runs `loom place` with flags it takes, and KillMode=process, so a restart stops the placer and
// never the runs it started. Mutant: the KillMode line taken out.
func TestTheShippedUnitKeepsItsRunsAcrossARestart(t *testing.T) {
	unit, err := os.ReadFile("../../placer/systemd/loom-place.service")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unit, []byte("\nKillMode=process\n")) {
		t.Fatal("the unit's restart would kill every run in flight: no KillMode=process")
	}
	arguments, err := placeUnitCommand(unit, "/home/loom")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := parsePlaceFlags(arguments, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loom place %v: %v", arguments, err)
	}
	if len(settings.poolHas["box-phase"]) == 0 || *settings.ledgerPath != "/home/loom/loom-placer/placed.jsonl" || *settings.once || *settings.dryRun {
		t.Fatalf("the unit's placer is %v", arguments)
	}
	// Workshop's builds stay off until the fleet decodes a test job's tree; the tree builder's ledger is the one its
	// unit writes. Mutant: --trees shipped on.
	if *settings.trees || *settings.treesLedger != "/home/loom/loom-trees/trees.jsonl" || *settings.treeWait != placer.TreeWaitBound {
		t.Fatalf("the unit's placer names trees %v from %s, waiting %v", *settings.trees, *settings.treesLedger, *settings.treeWait)
	}
	builderUnit, err := os.ReadFile("../../treebuilder/systemd/loom-build-trees.service")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(builderUnit, []byte("--ledger %h/loom-trees/trees.jsonl")) {
		t.Fatal("the placer reads a ledger the tree builder's unit doesn't write")
	}
}

// A run's job and log are this user's alone (its log prints the run's viewer token), its ending reaches the placer
// with its status, and the tail it hands on has no token in it. Mutants: the log world-readable; the tail unredacted.
func TestARunsFilesArePrivateAndItsEndingReachesThePlacer(t *testing.T) {
	directory := t.TempDir()
	fake := filepath.Join(directory, "fake-loom")
	token := "eyJydW4iOiJyLXZlY3RvciIsInNjb3BlIjoidmlld2VyIn0.5yFFC9AOwC9L6zqwWz8V0aCJNrLwusF5LjbOv_hoDWY"
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"run x: 3 units on 2 slots\"\necho \"  https://wire/runs/x?token="+token+"\"\necho \"loom: posting the plan: refused\"\nexit 3\n"), 0o755)
	tree := strings.Repeat("a", 40)
	placement := placer.Placement{Future: tree, Attempt: 1, Run: "future-" + tree + "-1"}
	exits := make(chan placer.Exit, 1)
	if err := startRun(placement, runOptions{runs: directory, binary: fake}, exits); err != nil {
		t.Fatal(err)
	}
	var exit placer.Exit
	select {
	case exit = <-exits:
	case <-time.After(10 * time.Second):
		t.Fatal("the run's ending never reached the placer")
	}
	if exit.Run != placement.Run || exit.Status != "exit status 3" || !strings.Contains(exit.Tail, "refused") || strings.Contains(exit.Tail, token) || !strings.Contains(exit.Tail, "<token>") {
		t.Fatalf("exit %+v", exit)
	}
	for _, name := range []string{placement.Run + ".log", placement.Run + ".json"} {
		if info, err := os.Stat(filepath.Join(directory, name)); err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want mode 600", name, info.Mode(), err)
		}
	}
}

// Pruning removes a run's files once they're over keep old, by name, and nothing else in the directory. Mutant: age
// ignored.
func TestPruningRemovesOnlyOldRunFiles(t *testing.T) {
	directory := t.TempDir()
	now := time.Now()
	old, recent := now.Add(-8*24*time.Hour), now.Add(-time.Hour)
	files := map[string]time.Time{"future-a-1.log": old, "future-a-1.json": old, "future-b-1.log": recent, "placed.jsonl": old}
	for name, at := range files {
		os.WriteFile(filepath.Join(directory, name), []byte("x"), 0o600)
		os.Chtimes(filepath.Join(directory, name), at, at)
	}
	removed, err := pruneRuns(directory, 7*24*time.Hour, now)
	if err != nil || removed != 2 {
		t.Fatalf("removed %d (%v)", removed, err)
	}
	for name, want := range map[string]bool{"future-a-1.log": false, "future-a-1.json": false, "future-b-1.log": true, "placed.jsonl": true} {
		if _, err := os.Stat(filepath.Join(directory, name)); (err == nil) != want {
			t.Errorf("%s kept %v, want %v", name, err == nil, want)
		}
	}
}

// placeUnitCommand is the shipped unit's `loom place` arguments: its ExecStart after `loom place`, each $NAME word
// replaced by its Environment= value split at whitespace and %h by home, as systemd does.
func placeUnitCommand(unit []byte, home string) ([]string, error) {
	environment, command := map[string]string{}, ""
	for _, line := range bytes.Split(unit, []byte("\n")) {
		text := strings.TrimSpace(string(line))
		if value, found := strings.CutPrefix(text, "Environment="); found {
			name, setting, _ := strings.Cut(strings.Trim(value, `"`), "=")
			environment[name] = setting
		}
		if value, found := strings.CutPrefix(text, "ExecStart="); found {
			command = value
		}
	}
	words := []string{}
	for _, word := range strings.Fields(strings.ReplaceAll(command, "%h", home)) {
		if name, found := strings.CutPrefix(word, "$"); found {
			words = append(words, strings.Fields(environment[name])...)
			continue
		}
		words = append(words, word)
	}
	if len(words) < 2 || filepath.Base(words[0]) != "loom" || words[1] != "place" {
		return nil, fmt.Errorf("ExecStart %q isn't loom place", command)
	}
	return words[2:], nil
}
