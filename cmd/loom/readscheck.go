package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/system-inc/loom/planner"
)

// readsCheck is the read lock (#ed4p461): a test unit's traced reads against its declared inputs. It prints each
// finding as a JSON line and exits 1 when there is one, so an undeclared read reds the unit and reaches its owner.
func readsCheck(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("reads-check", flag.ContinueOnError)
	flags.SetOutput(stderr)
	tree := flags.String("tree", "", "the checkout the unit ran in")
	gateTools := flags.String("gate-tools", "", "the gate tools checkout, for executors.txt's reads lines")
	importPath := flags.String("package", "", "the unit's package import path")
	tracePath := flags.String("trace", "", "the unit's strace-format trace (strace -f --decode-fds=path -e trace=open,openat,openat2,execve)")
	unitKey := flags.String("unit-key", "", "the unit's key, carried on each finding")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *tree == "" || *gateTools == "" || *importPath == "" || *tracePath == "" {
		fmt.Fprintln(stderr, "usage: loom reads-check --tree <dir> --gate-tools <dir> --package <import path> --trace <file> [--unit-key <key>]")
		return 2
	}
	trace, err := os.Open(*tracePath)
	if err != nil {
		fmt.Fprintln(stderr, "reads-check:", err)
		return 1
	}
	defer trace.Close()
	findings, err := planner.CheckUnitReads(*tree, *gateTools, *importPath, trace)
	if err != nil {
		fmt.Fprintln(stderr, "reads-check:", err)
		return 1
	}
	encoder := json.NewEncoder(stdout)
	for _, finding := range findings {
		finding.UnitKey = *unitKey
		if err := encoder.Encode(finding); err != nil {
			fmt.Fprintln(stderr, "reads-check:", err)
			return 1
		}
	}
	if len(findings) > 0 {
		fmt.Fprintf(stderr, "reads-check: %s read %d tracked files outside its declared inputs\n", *importPath, len(findings))
		return 1
	}
	return 0
}
