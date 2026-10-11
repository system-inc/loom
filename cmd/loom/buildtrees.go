package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/resident"
	"github.com/system-inc/loom/treebuilder"
)

// buildTreesSettings are `loom build-trees`'s flags, parsed in one place so the shipped unit's command line is
// checked against them (treebuilder/systemd/loom-build-trees.service).
type buildTreesSettings struct {
	queue, tokenFile, clone, ledgerPath, requests, logs, cache *string
	jobs, compile                                              *int
	floorGB, tempFloorGB, goCacheGB                            *uint64
	bound, keep, interval                                      *time.Duration
	once, resident                                             *bool
	store                                                      storeFlags
}

func parseBuildTreesFlags(arguments []string, stderr io.Writer) (buildTreesSettings, error) {
	flags := flag.NewFlagSet("build-trees", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	settings := buildTreesSettings{}
	settings.queue = flags.String("queue", "", "loom's base URL")
	settings.tokenFile = flags.String("token-file", "", "file holding the coordinator token, which reads Queue's planned listing")
	settings.clone = flags.String("clone", filepath.Join(home, "loom-trees", "adamic"), "the builder's own clone of "+protocol.AdamicRepository+", made when missing; each future is checked out in it keyless")
	settings.ledgerPath = flags.String("ledger", filepath.Join(home, "loom-trees", "trees.jsonl"), "what was built, by tree key, which the placer reads; one builder holds it at a time")
	settings.requests = flags.String("requests", filepath.Join(home, "loom-trees", "requests.jsonl"), "the trees Judge asks for, base trees its reruns wait on (loom judge --tree-requests), built ahead of the listing's")
	settings.logs = flags.String("logs", filepath.Join(home, "loom-trees", "logs"), "each build's build-tree output, <tree key>.log")
	settings.cache = flags.String("cache", filepath.Join(home, "loom-builder", "trees"), "build-tree's --cache, the base of each tree's own build directory")
	settings.jobs = flags.Int("jobs", 8, "build-tree's --jobs")
	settings.compile = flags.Int("compile", 0, "build-tree's --compile (0: every thread but four)")
	settings.floorGB = flags.Uint64("floor-gb", 100, "build-tree's --floor-gb, which the clone's filesystem keeps too: under it nothing is checked out or built")
	settings.tempFloorGB = flags.Uint64("temp-floor-gb", 20, "build-tree's --temp-floor-gb")
	settings.goCacheGB = flags.Uint64("go-cache-gb", 500, "build-tree's --go-cache-gb")
	settings.bound = flags.Duration("bound", 2*time.Hour, "the longest one build-tree runs: past it, it's killed with everything it started and the tree is failed")
	settings.keep = flags.Duration("keep", 7*24*time.Hour, "how long the ledger's records and the builds' logs are kept")
	settings.interval = flags.Duration("interval", 10*time.Second, "time between pulls")
	settings.once = flags.Bool("once", false, "build at most one tree and exit")
	settings.resident = flags.Bool("resident", false, "keep the trees built warm in memory (package resident) and hand each build its keys, read from the nearest warm tree's diff")
	settings.store = addStoreFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return settings, err
	}
	if *settings.queue == "" || *settings.tokenFile == "" || *settings.bound <= 0 || flags.NArg() != 0 {
		return settings, errors.New("usage: loom build-trees --queue <url> --token-file <path> [--clone <dir>] [--ledger <file>] [--requests <file>] [--logs <dir>] [--r2 <key file>] [--bucket <name>] [--cache <dir>] [--jobs N] [--compile N] [--floor-gb N] [--temp-floor-gb N] [--go-cache-gb N] [--bound 2h] [--keep 168h] [--interval 10s] [--once] [--resident]")
	}
	return settings, nil
}

// buildTrees is Workshop's tree builder's loop (package treebuilder), beside `loom place`: every tree a planned future
// runs, and every base tree Judge asks for (--requests), that the action store lacks is built, one at a time, by this binary's own `loom build-tree` in a child process
// (a build that dies, or is killed at --bound, takes only itself), on the future's commit checked out keyless in the
// builder's own clone, and recorded in the ledger the placer reads.
func buildTrees(arguments []string, stdout io.Writer, stderr io.Writer) int {
	settings, err := parseBuildTreesFlags(arguments, stderr)
	if err != nil {
		if strings.HasPrefix(err.Error(), "usage:") {
			fmt.Fprintln(stderr, err)
		}
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintln(stderr, "build-trees:", err)
		return 1
	}
	token, err := os.ReadFile(*settings.tokenFile)
	if err != nil {
		return fail(err)
	}
	store, err := settings.store.open(nil)
	if err != nil {
		return fail(err)
	}
	if err := readyClone(*settings.clone); err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*settings.logs, 0o700); err != nil {
		return fail(err)
	}
	binary, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	goCache, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		return fail(fmt.Errorf("go env GOCACHE: %w", err))
	}
	floor, err := builder.Gigabytes(*settings.floorGB)
	if err != nil {
		return fail(err)
	}
	tempFloor, err := builder.Gigabytes(*settings.tempFloorGB)
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*settings.cache, 0o755); err != nil {
		return fail(err)
	}
	watched := buildTreeWatches(*settings.cache, strings.TrimSpace(string(goCache)), os.TempDir(), floor, tempFloor)
	watched["the builder's clone"] = builder.Watch{Path: *settings.clone, Floor: floor}
	ledger, err := treebuilder.OpenLedger(*settings.ledgerPath, time.Now())
	if err != nil {
		return fail(err)
	}
	defer ledger.Close()
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	checkout := planner.GitCheckout(*settings.clone)
	var warm *resident.Resident
	if *settings.resident {
		warm = resident.New()
	}
	loop := &treebuilder.Builder{
		Source: judge.HTTPFutures{Base: *settings.queue, Token: strings.TrimSpace(string(token))},
		Requests: func() ([]treebuilder.Request, error) {
			return treebuilder.ReadRequests(*settings.requests, *settings.keep, time.Now())
		},
		Indexed: store.TreeIndexed,
		Floor:   func() error { return builder.CheckFloor(watched, nil) },
		Build: func(want treebuilder.Want, running func(pid int) error) (*builder.TreePhases, error) {
			return buildWant(runContext, checkout, binary, settings, want, warm, running)
		},
		Phases: func(tree string) *builder.TreePhases {
			return readTreePhases(filepath.Join(*settings.logs, tree+".log"))
		},
		Alive:    buildAlive,
		Kill:     killGroup,
		Bound:    *settings.bound,
		Stopping: func() bool { return runContext.Err() != nil },
		Ledger:   ledger,
		Keep:     *settings.keep,
		Now:      time.Now,
		Log:      stdout,
	}
	var prunedAt time.Time
	for runContext.Err() == nil {
		built, err := loop.BuildOnce()
		if err != nil {
			fmt.Fprintln(stderr, "build-trees:", err)
			if *settings.once {
				return 1
			}
		}
		if time.Since(prunedAt) > time.Hour {
			prunedAt = time.Now()
			if err := pruneBuildLogs(*settings.logs, *settings.keep, prunedAt); err != nil {
				fmt.Fprintln(stderr, "build-trees: pruning logs:", err)
			}
		}
		if *settings.once {
			return 0
		}
		if built {
			// The next missing tree goes at once, after the listing is read again.
			continue
		}
		select {
		case <-runContext.Done():
		case <-time.After(*settings.interval):
		}
	}
	return 0
}

// buildWant checks the wanted future out and runs build-tree on it, and returns the build's phases: the checkout's
// seconds, the resident's keying's, and build-tree's own from its summary line when it printed one. A checkout that
// fails is transient (GitHub's 5xx, the network), retried soon and never the tree's failure; one the builder's stop cut
// short is a stop. With a resident (warm), the tree is keyed against the nearest warm tree and its keys go to
// build-tree in a file beside its log; a resident that can't key it is said in the log and the tree builds cold, as it
// would without one.
func buildWant(runContext context.Context, checkout planner.Checkout, binary string, settings buildTreesSettings, want treebuilder.Want, warm *resident.Resident, running func(pid int) error) (*builder.TreePhases, error) {
	started := time.Now()
	tree, cleanup, err := checkout(want.Future)
	phases := &builder.TreePhases{Checkout: time.Since(started).Seconds()}
	if err != nil && runContext.Err() != nil {
		return phases, fmt.Errorf("%w: checking %s out: %v", treebuilder.ErrStopped, want.Future, err)
	}
	if err != nil {
		return phases, fmt.Errorf("%w: checking %s out keyless: %v", treebuilder.ErrTransient, want.Future, err)
	}
	defer cleanup()
	arguments := buildTreeArguments(settings, tree, want)
	if warm != nil {
		keyingStarted := time.Now()
		keysFile := filepath.Join(*settings.logs, want.Tree+".keys.json")
		keyed, err := warm.Key(tree, want.Future)
		if err == nil {
			err = keyed.WriteKeys(keysFile)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "build-trees: the resident couldn't key tree %s of %s, so it builds cold: %v\n", want.Tree, want.Future, err)
		} else {
			fmt.Fprintf(os.Stderr, "build-trees: tree %s of %s keyed by the resident in %.1f s, %d paths from %s, %d packages listed again\n",
				want.Tree, want.Future, keyed.Seconds, len(keyed.Changed), keyed.From, len(keyed.Relisted))
			arguments = append(arguments, "--keys", keysFile)
		}
		phases.Keying = time.Since(keyingStarted).Seconds()
	}
	log := filepath.Join(*settings.logs, want.Tree+".log")
	err = runBuildTree(runContext, binary, arguments, log, *settings.bound, running)
	if built := readTreePhases(log); built != nil {
		built.Checkout, built.Keying, phases = phases.Checkout, phases.Keying, built
	}
	return phases, err
}

// readyClone makes the builder's clone when it's missing, an empty repository whose origin is the public adamic
// repository, and refuses one whose origin is anything else: every tree the builder checks out comes from what that
// repository serves anyone.
func readyClone(clone string) error {
	if _, err := os.Stat(filepath.Join(clone, ".git")); errors.Is(err, os.ErrNotExist) {
		for _, arguments := range [][]string{{"init", "-q", clone}, {"-C", clone, "remote", "add", "origin", protocol.AdamicRepository}} {
			if output, err := planner.KeylessGit(arguments...).CombinedOutput(); err != nil {
				return fmt.Errorf("making the clone: git %s: %v: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
			}
		}
	}
	// The URL as the clone records it, before any rewrite.
	origin, err := planner.KeylessGit("-C", clone, "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return fmt.Errorf("the clone %s's origin: %w", clone, err)
	}
	if strings.TrimSpace(string(origin)) != protocol.AdamicRepository {
		return fmt.Errorf("the clone %s's origin is %q, not %s", clone, strings.TrimSpace(string(origin)), protocol.AdamicRepository)
	}
	return nil
}

// buildTreeArguments are the `loom build-tree` that builds one wanted tree with the builder's settings, the plan's tree
// key among them, so a tree keying otherwise is refused before anything is built.
func buildTreeArguments(settings buildTreesSettings, tree string, want treebuilder.Want) []string {
	return []string{"build-tree", "--tree", tree, "--future", want.Future, "--tree-key", want.Tree, "--go", want.Go,
		"--read", *settings.store.read, "--r2", *settings.store.credentials, "--bucket", *settings.store.bucket, "--cache", *settings.cache,
		"--jobs", strconv.Itoa(*settings.jobs), "--compile", strconv.Itoa(*settings.compile),
		"--floor-gb", strconv.FormatUint(*settings.floorGB, 10), "--temp-floor-gb", strconv.FormatUint(*settings.tempFloorGB, 10),
		"--go-cache-gb", strconv.FormatUint(*settings.goCacheGB, 10)}
}

// runBuildTree runs binary with arguments, its output to log (mode 600, new each build), in a process group of its own,
// calls running with its pid once it runs, and says how it ended badly, with the log's last lines, or nil. Past bound
// its group is killed. Told to stop meanwhile (runContext's: a release's restart, #apsj7zp), it leaves the child
// running and returns treebuilder.ErrDetached: the unit's KillMode=process spares the child, and the next builder
// adopts it through its running record.
func runBuildTree(runContext context.Context, binary string, arguments []string, log string, bound time.Duration, running func(pid int) error) error {
	output, err := os.OpenFile(log, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer output.Close()
	command := exec.Command(binary, arguments...)
	command.Stdout, command.Stderr = output, output
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = command.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	if err = running(command.Process.Pid); err != nil {
		// Unrecorded, it could never be adopted: it goes now, and the build with it.
		killGroup(command.Process.Pid)
		<-done
		return fmt.Errorf("recording the build running: %w", err)
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case err = <-done:
	case <-runContext.Done():
		return fmt.Errorf("%w: pid %d", treebuilder.ErrDetached, command.Process.Pid)
	case <-timer.C:
		killGroup(command.Process.Pid)
		<-done
		content, _ := os.ReadFile(log)
		return fmt.Errorf("build-tree ran past its %v bound and was killed: %s", bound, logTail(content, 5))
	}
	if err == nil {
		return nil
	}
	content, _ := os.ReadFile(log)
	return fmt.Errorf("build-tree: %v: %s", err, logTail(content, 5))
}

// killGroup kills the process group pid leads, everything a build started.
func killGroup(pid int) {
	syscall.Kill(-pid, syscall.SIGKILL)
}

// buildAlive says whether pid is still the `loom build-tree` of tree, by its command line: a pid reused by another
// process since is no build to wait for.
func buildAlive(pid int, tree string) bool {
	content, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	arguments := strings.Split(string(content), "\x00")
	return slices.Contains(arguments, "build-tree") && slices.Contains(arguments, tree)
}

// pruneBuildLogs removes each build's log under directory last written over keep ago, by name.
func pruneBuildLogs(directory string, keep time.Duration, now time.Time) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() || filepath.Ext(entry.Name()) != ".log" {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < keep {
			continue
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
