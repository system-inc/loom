package main

import (
	"strings"
	"testing"

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

// --pool-has joins the table by name, and one naming a pool the table doesn't hold stops the placer.
func TestPoolHasJoinsThePoolTable(t *testing.T) {
	table := []judge.PoolEntry{{Name: "codex-strict"}, {Name: "box-phase"}}
	has := poolHasFlag{}
	has.Set("codex-strict=go,wasiSdk")
	pools, err := placerPools(table, has)
	if err != nil || strings.Join(pools[0].Has, ",") != "go,wasiSdk" || len(pools[1].Has) != 0 {
		t.Fatalf("pools %+v (%v)", pools, err)
	}
	has.Set("box-stict=go")
	if _, err := placerPools(table, has); err == nil {
		t.Fatal("a --pool-has naming no pool was taken")
	}
}
