package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/system-inc/loom/planner"
)

// unitNeeds prints a unit's declared machine needs for the placer (Loom, Oct 10 01:24Z), resolved by the same planner
// code that stamps them on a plan, so the placer keeps no list of its own: for a plan posted before its need was
// declared. Fabric passes the gate tools at the ref the future's plan was keyed on. It prints
// {"cpus":N,"memoryMegabytes":N}, or nothing when none is declared, and exits 1 when no pool of the key's runner holds it.
func unitNeeds(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("unit-needs", flag.ContinueOnError)
	flags.SetOutput(stderr)
	gateTools := flags.String("gate-tools", "", "the gate tools checkout holding cloud/fast-gate/unit-needs.json")
	directory := flags.String("package", "", "the unit's package directory, repo-relative")
	run := flags.String("run", "", "the unit's run pattern, for a need narrowed to one")
	pools := flags.String("pools", planner.PoolsFile, "the pool table, which pools have room for the need")
	runner := flags.String("runner", "", "the unit key's runner sha256; only its pools count (empty: any)")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *gateTools == "" || *directory == "" {
		fmt.Fprintln(stderr, "usage: loom unit-needs --gate-tools <dir> --package <directory> [--run <pattern>] [--runner <sha256>] [--pools <file>]")
		return 2
	}
	needs, err := planner.LoadUnitNeeds(*gateTools)
	if err != nil {
		fmt.Fprintln(stderr, "unit-needs:", err)
		return 1
	}
	var table []planner.Pool
	if len(needs.Units) > 0 {
		if table, err = planner.LoadPools(*pools); err != nil {
			fmt.Fprintln(stderr, "unit-needs:", err)
			return 1
		}
	}
	resources, err := needs.For(*directory, *run, table, *runner)
	if err != nil {
		fmt.Fprintln(stderr, "unit-needs:", err)
		return 1
	}
	if resources != nil {
		encoded, _ := json.Marshal(resources)
		fmt.Fprintln(stdout, string(encoded))
	}
	return 0
}
