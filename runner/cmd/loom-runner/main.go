// Command loom-runner runs one Loom unit on this machine and streams its events to stdout, one JSON line
// each. It exits 0 when the unit passed, 1 when it failed and 2 when it is broken or couldn't be read.
//
//	loom-runner run [--workspace <directory>] [--keep] [--strict] [--phase-jobs] [--exclusive] [--root <directory>] [--tree <directory>] [--machine <name>] <unit.json | https URL | ->
//	loom-runner serve --pool <wire>/pools/<pool> --token-file <file> --worker <name> --until <duration> [--strict] [--exclusive] [--root <directory>] [--tree <directory>] [--workspace <directory>] [--log <file>]
//	loom-runner install-serve
//	loom-runner version
//
// serve asks a pool for units and runs them until its deadline, each unit posting its own events to the
// wire. --token-file names a file holding the pool token, which serve reads and removes, so the token is never on its
// command line. With --strict (the Codex pool's) it runs only structured test jobs and refuses every other unit before
// anything runs (README.md, "What a strict worker does"). Its events go to --log, never stdout: it prints one summary line when it ends, since that line is
// all a Codex turn should show. It exits 0 at the deadline, on SIGHUP once the unit in hand finishes (a drain), or on
// SIGTERM, which breaks the unit in hand, and 2 when the pool refuses it.
//
// install-serve readies this Linux box's loom-serve.service from ~/.loom/serve.conf and ~/.loom/serve-token and
// reloads it (package serving, docs/serving.md); the updater's hook runs it after every release.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner"
	"github.com/system-inc/loom/serving"
)

const usage = `usage:
  loom-runner run [--workspace <directory>] [--keep] [--strict] [--phase-jobs] [--exclusive] [--root <directory>] [--tree <directory>] [--machine <name>] <unit.json | https URL | ->
  loom-runner serve --pool <wire>/pools/<pool> --token-file <file> --worker <name> --until <duration> [--strict] [--exclusive] [--root <directory>] [--tree <directory>] [--workspace <directory>] [--log <file>]
  loom-runner install-serve
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
	case "install-serve":
		return installServe(arguments[1:], stdout, stderr)
	case "run":
	default:
		fmt.Fprint(stderr, usage)
		return 2
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	workspace := flags.String("workspace", "", "where to make the unit's workspace (default $TMPDIR)")
	keep := flags.Bool("keep", false, "leave the workspace in place after the run")
	// A serving runner hands a unit naming this runner to it with its own settings (runner/runners.go).
	strict := flags.Bool("strict", false, "run only a structured test job; refuse argv")
	phaseJobs := flags.Bool("phase-jobs", false, "also take a phase job")
	exclusive := flags.Bool("exclusive", false, "this machine is the runner's alone")
	root := flags.String("root", "", "where a test job keeps its caches between units")
	tree := flags.String("tree", "", "where a test job's checkout is kept across units")
	machine := flags.String("machine", "", "the name started events give this machine (default the host's)")
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
		Strict:          *strict,
		PhaseJobs:       *phaseJobs,
		Exclusive:       *exclusive,
		Root:            *root,
		Tree:            *tree,
		Machine:         *machine,
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
	// A hangup before the drain's handler is in place is ignored, never the default's exit.
	signal.Ignore(syscall.SIGHUP)
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
	exclusive := flags.Bool("exclusive", false, "this machine is the runner's alone (a Codex instance): a test job may clear HOME's caches and run adamic's setup there; a house box never passes it")
	root := flags.String("root", "", "where a test job keeps its caches between units (default /tmp with --exclusive)")
	tree := flags.String("tree", "", "where a test job's checkout is kept across units (default <root>/adamic)")
	releases := flags.String("releases", runner.DefaultReleases, "where a unit's runner is fetched by its sha256 when it names another than this one")
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
	// SIGHUP drains: loom-serve's reload, which a release's hook asks for, so the unit in hand finishes first.
	hangups := make(chan os.Signal, 1)
	signal.Notify(hangups, syscall.SIGHUP)
	defer signal.Stop(hangups)
	drain := make(chan struct{})
	go func() {
		<-hangups
		close(drain)
	}()
	summary, err := runner.Serve(serveContext, runner.ServeOptions{
		Pool:     *pool,
		Token:    token,
		Worker:   *worker,
		Deadline: time.Now().Add(*until),
		Unit:     runner.Options{WorkspaceParent: *workspace, Events: events, Diagnostics: stderr, Strict: *strict, PhaseJobs: *phaseJobs, Exclusive: *exclusive, Root: *root, Tree: *tree},
		Report:   stderr,
		Drain:    drain,
		Releases: *releases,
	})
	fmt.Fprintln(stdout, summary)
	if err != nil {
		return 2
	}
	return 0
}

// installServe is `loom-runner install-serve`: loom-serve.service rendered from this box's serve.conf, installed as a
// systemd user unit, enabled and reloaded, all under the user's systemd. Linux only, since only Linux boxes serve
// through systemd.
func installServe(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) != 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(stderr, "loom-runner: install-serve readies a systemd user unit, and this is %s\n", runtime.GOOS)
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "loom-runner: %v\n", err)
		return 2
	}
	systemctl := func(arguments ...string) (string, error) {
		command := exec.Command("systemctl", append([]string{"--user"}, arguments...)...)
		command.Stderr = stderr
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("systemctl --user %s: %w", strings.Join(arguments, " "), err)
		}
		return string(output), nil
	}
	if err := serving.Install(serving.HomePaths(home), systemctl, stdout); err != nil {
		fmt.Fprintf(stderr, "loom-runner: %v\n", err)
		return 2
	}
	return 0
}
