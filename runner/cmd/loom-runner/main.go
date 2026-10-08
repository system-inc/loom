// Command loom-runner runs one Loom unit on this machine and streams its events to stdout, one JSON line
// each. It exits 0 when the unit passed, 1 when it failed and 2 when it is broken or couldn't be read.
//
//	loom-runner run [--workspace <directory>] [--keep] <unit.json | https URL | ->
//	loom-runner version
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner"
)

const usage = `usage:
  loom-runner run [--workspace <directory>] [--keep] <unit.json | https URL | ->
  loom-runner version
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch arguments[0] {
	case "version":
		fmt.Fprintln(stdout, runner.Version)
		return 0
	case "run":
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspace := flags.String("workspace", "", "where to make the unit's workspace (default $TMPDIR)")
	keep := flags.Bool("keep", false, "leave the workspace in place after the run")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return 2
	}

	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	unit, err := runner.LoadUnit(runContext, flags.Arg(0), nil)
	if err != nil {
		fmt.Fprintf(stderr, "loom-runner: %v\n", err)
		return 2
	}
	result := runner.Run(runContext, unit, runner.Options{
		WorkspaceParent: *workspace,
		Keep:            *keep,
		Events:          stdout,
		Diagnostics:     stderr,
	})
	if *keep && result.Workspace != "" {
		fmt.Fprintf(stderr, "loom-runner: workspace kept at %s\n", result.Workspace)
	}
	switch result.Status {
	case protocol.StatusPassed:
		return 0
	case protocol.StatusFailed:
		return 1
	default:
		return 2
	}
}
