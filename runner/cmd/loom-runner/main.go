// Command loom-runner runs one Loom unit on this machine and streams its events to stdout, one JSON line
// each. It exits 0 when the unit passed, 1 when it failed and 2 when it is broken or couldn't be read.
//
//	loom-runner run [--workspace <directory>] [--keep] <unit.json | https URL | ->
//	loom-runner serve --pool <wire>/pools/<pool> --token-file <file> --worker <name> --until <duration> [--strict] [--root <directory>] [--tree <directory>] [--workspace <directory>] [--log <file>]
//	loom-runner version
//
// serve asks a pool for units and runs them until its deadline, each unit posting its own events to the
// wire. --token-file names a file holding the pool token, which serve reads and removes, so the token is never on its
// command line. With --strict (the Codex pool's) it runs only structured test jobs and refuses every other unit before
// anything runs (README.md, "What a strict worker does"). Its events go to --log, never stdout: it prints one summary line when it ends, since that line is
// all a Codex turn should show. It exits 0 at the deadline or on SIGTERM, and 2 when the pool refuses it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner"
)

const usage = `usage:
  loom-runner run [--workspace <directory>] [--keep] <unit.json | https URL | ->
  loom-runner serve --pool <wire>/pools/<pool> --token-file <file> --worker <name> --until <duration> [--strict] [--root <directory>] [--tree <directory>] [--workspace <directory>] [--log <file>]
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
	case "serve":
		return serve(arguments[1:], stdout, stderr)
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

// serve runs units from a pool until the deadline. Diagnostics go to stderr, which is quiet unless something
// is wrong; the events go to the log file or nowhere, since each unit posts them to the wire itself.
func serve(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	pool := flags.String("pool", "", "the pool's address, <wire>/pools/<pool>")
	tokenFile := flags.String("token-file", "", "a file holding the pool token; serve reads it and removes it")
	hostname, _ := os.Hostname()
	worker := flags.String("worker", hostname, "this instance's name in the pool's status")
	until := flags.Duration("until", 0, "how long to serve; no unit is taken in its last minute")
	workspace := flags.String("workspace", "", "where to make each unit's workspace (default $TMPDIR)")
	logPath := flags.String("log", "", "append every unit's events here, one JSON line each (default nowhere)")
	strict := flags.Bool("strict", false, "run only structured test jobs of the public adamic repository; refuse argv")
	phaseJobs := flags.Bool("phase-jobs", false, "also take phase jobs (a box fast gate phase through run.py at the gate tools' commit); only box services pass it")
	root := flags.String("root", "", "where a test job keeps its caches between units (default /tmp with --strict)")
	tree := flags.String("tree", "", "where a test job's checkout is kept across units (default <root>/adamic)")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *pool == "" || *tokenFile == "" || *worker == "" || *until <= 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	token, err := runner.ReadTokenFile(*tokenFile)
	if err != nil {
		fmt.Fprintf(stderr, "loom-runner: %v\n", err)
		return 2
	}
	events := io.Discard
	if *logPath != "" {
		file, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(stderr, "loom-runner: %v\n", err)
			return 2
		}
		defer file.Close()
		events = file
	}
	serveContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	summary, err := runner.Serve(serveContext, runner.ServeOptions{
		Pool:     *pool,
		Token:    token,
		Worker:   *worker,
		Deadline: time.Now().Add(*until),
		Unit:     runner.Options{WorkspaceParent: *workspace, Events: events, Diagnostics: stderr, Strict: *strict, PhaseJobs: *phaseJobs, Root: *root, Tree: *tree},
		Report:   stderr,
	})
	fmt.Fprintln(stdout, summary)
	if err != nil {
		return 2
	}
	return 0
}
