package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/coordinator"
	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/placer"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/treebuilder"
)

// placeSettings are `loom place`'s flags, parsed in one place so the shipped unit's command line is checked against
// them (placer/systemd/loom-place.service).
type placeSettings struct {
	queue, tokenFile, wire, source, poolsPath, needsGit *string
	warmRunnersFile, warmAttempts, ledgerPath, runs     *string
	treesLedger                                         *string
	poolHas                                             poolHasFlag
	priority, poolSlots                                 *int
	unfitEvery, keep, interval, treeWait                *time.Duration
	once, dryRun, trees                                 *bool
	store                                               storeFlags
}

func parsePlaceFlags(arguments []string, stderr io.Writer) (placeSettings, error) {
	flags := flag.NewFlagSet("place", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	settings := placeSettings{poolHas: poolHasFlag{}}
	settings.queue = flags.String("queue", "", "loom's base URL")
	settings.tokenFile = flags.String("token-file", "", "file holding the coordinator token")
	settings.wire = flags.String("wire", "https://runs.loom.system.inc", "the wire's origin, where runs' events live and units are queued")
	settings.source = flags.String("source", defaultSource(), "this repository's checkout, which `loom run` builds the pool runner's version from")
	settings.poolsPath = flags.String("pools", filepath.Join(home, ".loom", "pools.json"), "the pool table, read every pass: each pool's runner, kinds, memory, cpus and cold mark")
	flags.Var(settings.poolHas, "pool-has", "the toolchains every worker of a pool has, <name>=<toolchain>,..., as the judge's; a pool without one has none; repeatable")
	settings.needsGit = flags.String("needs-git", "", "a clone of Adamic whose origin's loom/planner-reads holds unit-needs.json, for a unit whose plan carried no need")
	settings.warmRunnersFile = flags.String("warm-runners-file", filepath.Join(home, "loom-judge", "warm-runners.txt"), "the steady judge's warm runners: a test unit keyed on one goes only to a cold pool; a missing file means none")
	settings.warmAttempts = flags.String("warm-attempts", filepath.Join(home, "loom-judge", "warm-attempts.txt"), "the steady judge's warm-attempts file, for the units the judge carries; a missing file means none")
	settings.ledgerPath = flags.String("ledger", filepath.Join(home, "loom-placer", "placed.jsonl"), "what was placed, by future and attempt, so a restart never places twice; one placer holds it at a time")
	settings.runs = flags.String("runs", filepath.Join(home, "loom-placer", "runs"), "where each run's job, log and record go, readable by this user only")
	settings.priority = flags.Int("priority", 40, "the runs' priority on their pools, 0 to 1000 (40, a landing's)")
	settings.poolSlots = flags.Int("pool-slots", 32, "the most units of one run queued on one pool at once")
	settings.unfitEvery = flags.Duration("unfit-every", 30*time.Minute, "how soon a future voided for a cause is voided again for the same one, and how long its reads may fail before it's voided, under the judge's 45-minute backstop")
	settings.keep = flags.Duration("keep", 7*24*time.Hour, "how long a run's files, and the ledger's attempts of futures Queue no longer lists, are kept")
	settings.interval = flags.Duration("interval", 10*time.Second, "time between pulls")
	settings.once = flags.Bool("once", false, "pull once and exit")
	settings.dryRun = flags.Bool("dry-run", false, "print each placement and void, start and post nothing, and write no ledger")
	settings.trees = flags.Bool("trees", false, "every runner of the fleet runs a release that decodes a test job's tree: name Workshop's build on every test unit, holding an attempt until its tree's index is up (off: none named, none held)")
	settings.treesLedger = flags.String("trees-ledger", filepath.Join(home, "loom-trees", "trees.jsonl"), "the tree builder's ledger (`loom build-trees`), read without its lock: why a tree an attempt waits on isn't up")
	settings.treeWait = flags.Duration("tree-wait", placer.TreeWaitBound, "how long an attempt waits on its tree's index before it's voided, named, under the judge's 45-minute backstop")
	settings.store = addStoreFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return settings, err
	}
	if *settings.queue == "" || *settings.tokenFile == "" || *settings.priority < 0 || *settings.priority > 1000 || *settings.poolSlots < 1 || flags.NArg() != 0 {
		return settings, errors.New("usage: loom place --queue <url> --token-file <path> --pool-has <name>=<toolchains>... [--pools <file>] [--needs-git <clone>] [--wire <url>] [--r2 <key file>] [--ledger <file>] [--runs <dir>] [--priority 40] [--pool-slots 32] [--keep 168h] [--interval 10s] [--once] [--dry-run]")
	}
	// The placer's named void must land before the judge's silent one.
	if *settings.treeWait <= 0 || *settings.treeWait >= judge.StaleAfter {
		return settings, fmt.Errorf("--tree-wait %v: an attempt waits on its tree under the judge's %v backstop, so its void is named", *settings.treeWait, judge.StaleAfter)
	}
	return settings, nil
}

// place is the placer's pull loop on the coordinator host, beside `loom plan` and `loom judge`: every attempt Queue
// lists planned and undecided (GET /futures?state=planned, read only) that the ledger doesn't hold is placed whole in
// one detached `loom run --run-id future-<tree>-<attempt>` on the pools that take its units, or posted void as Loom's,
// neverPlaced, naming each unit it can't place (package placer).
func place(arguments []string, stdout io.Writer, stderr io.Writer) int {
	settings, err := parsePlaceFlags(arguments, stderr)
	if err != nil {
		if strings.HasPrefix(err.Error(), "usage:") {
			fmt.Fprintln(stderr, err)
		}
		return 2
	}
	home, _ := os.UserHomeDir()
	token, err := os.ReadFile(*settings.tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "place:", err)
		return 1
	}
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintln(stderr, "place:", err)
		return 1
	}
	warmRunner := map[string]bool{}
	if content, err := os.ReadFile(*settings.warmRunnersFile); err == nil {
		warmRunner = warmRunnerSet(string(content))
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, "place:", err)
		return 1
	}
	if _, err := os.Stat(*settings.warmAttempts); errors.Is(err, os.ErrNotExist) {
		*settings.warmAttempts = ""
	}
	readTable, err := poolReader(*settings.poolsPath, settings.poolHas, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "place:", err)
		return 1
	}
	client := strings.TrimSpace(string(token))
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	read := func(run string) ([]protocol.Event, error) {
		return coordinator.ReadRunEvents(runContext, *settings.wire, secret, run)
	}
	carrier := judge.Puller{Loop: judge.Loop{Warm: warmRule(warmRunner, *settings.warmAttempts, *settings.poolsPath)}, RunOf: coordinator.FutureRun, Read: read}
	voider := judge.NewPuller(judge.Puller{RunOf: coordinator.FutureRun, Read: read, Queue: judge.HTTPQueue{Base: *settings.queue, Token: client},
		Loop: judge.Loop{Now: time.Now, Reused: judge.HTTPReused{Base: *settings.queue, Token: client}}})
	options := runOptions{runs: *settings.runs, wire: *settings.wire, source: *settings.source, priority: *settings.priority}
	exits := make(chan placer.Exit, 64)
	start := func(placement placer.Placement) error { return startRun(placement, options, exits) }
	stopper := func(run string) error { return stopRun(run, stdout) }
	var ledger placer.Ledger
	// The action store: a void's records go there as the judge's do, straight to R2 with this machine's key, and a
	// tree's index is read there.
	var store builder.Store
	if !*settings.dryRun || *settings.trees {
		if store, err = settings.store.open(nil); err != nil {
			fmt.Fprintln(stderr, "place:", err)
			return 1
		}
	}
	if *settings.dryRun {
		voider.Queue, voider.Loop.Blobs = printedQueue{out: stdout}, &judge.StubBlobs{}
		ledger = &placer.MemoryLedger{}
		start = func(placement placer.Placement) error {
			fmt.Fprintf(stdout, "dry run, not started: loom %s\n", strings.Join(runArguments(placement, options), " "))
			return nil
		}
		stopper = func(run string) error {
			fmt.Fprintf(stdout, "dry run, not stopped: %s\n", run)
			return nil
		}
	} else {
		voider.Loop.Blobs = judge.StoreBlobs{Store: store}
		// A run's log holds its live page's viewer token: the directory is this user's alone.
		if err := os.MkdirAll(*settings.runs, 0o700); err == nil {
			err = os.Chmod(*settings.runs, 0o700)
		}
		if err != nil {
			fmt.Fprintln(stderr, "place:", err)
			return 1
		}
		fileLedger, err := placer.OpenLedger(*settings.ledgerPath)
		if err != nil {
			fmt.Fprintln(stderr, "place:", err)
			return 1
		}
		defer fileLedger.Close()
		ledger = fileLedger
	}
	queueClient := planner.QueueClient{Base: *settings.queue, Token: client}
	loop := &placer.Placer{
		Source:  judge.HTTPFutures{Base: *settings.queue, Token: client},
		Carried: carrier.CarriedFrom,
		ChangePaths: func(change string) ([]string, error) {
			return queueClient.ChangePaths(planner.Future{Changes: []string{change}})
		},
		Pools:       readTable,
		Needs:       func() (planner.UnitNeeds, error) { return loadNeeds(*settings.needsGit) },
		WarmRunners: warmRunner,
		Start:       start,
		Exits:       exits,
		RunStarted: func(run string) (bool, error) {
			events, err := read(run)
			return len(events) > 0, err
		},
		Void: func(future judge.PlannedFuture, attempt int, cause string) error {
			_, err := voider.VoidListed(future, attempt, judge.InfraNeverPlaced, cause)
			return err
		},
		ChangeOf: queueClient.ChangeState,
		Stop:     stopper,
		Unplan: func(future judge.PlannedFuture, reason string) error {
			return queueClient.Unplan(future.Future, "loom place", reason)
		},
		Ledger:     ledger,
		PoolSlots:  *settings.poolSlots,
		UnfitEvery: *settings.unfitEvery,
		Keep:       *settings.keep,
		Trees:      *settings.trees,
		TreeState:  treeStateReader(store, *settings.treesLedger),
		TreeWait:   *settings.treeWait,
		Now:        time.Now,
		Log:        stdout,
	}
	var prunedAt time.Time
	for runContext.Err() == nil {
		count, err := loop.PlaceOnce()
		if count > 0 {
			fmt.Fprintf(stdout, "placed %d futures\n", count)
		}
		if err != nil {
			fmt.Fprintln(stderr, "place:", err)
			if *settings.once {
				return 1
			}
		}
		if !*settings.dryRun && time.Since(prunedAt) > time.Hour {
			prunedAt = time.Now()
			if removed, err := pruneRuns(*settings.runs, *settings.keep, prunedAt); err != nil {
				fmt.Fprintln(stderr, "place: pruning runs:", err)
			} else if removed > 0 {
				fmt.Fprintf(stdout, "pruned %d run files over %s old\n", removed, *settings.keep)
			}
		}
		if *settings.once {
			return 0
		}
		select {
		case <-runContext.Done():
		case <-time.After(*settings.interval):
		}
	}
	return 0
}

// treeStateReader reads a tree's build as the placer sees it: whether the bucket holds its index, and the tree
// builder's newest record of it from its ledger at ledgerPath, read without the builder's lock.
func treeStateReader(store builder.Store, ledgerPath string) func(tree string) (placer.TreeState, error) {
	return func(tree string) (placer.TreeState, error) {
		indexed, err := store.TreeIndexed(tree)
		if err != nil {
			return placer.TreeState{}, err
		}
		newest, found, err := treebuilder.Newest(ledgerPath, tree)
		return placer.TreeState{Indexed: indexed, Newest: newest, Found: found}, err
	}
}

// poolReader reads the pool table joined with --pool-has. At start a --pool-has naming a pool the table doesn't hold
// is refused, since it says nothing; after start such a pool was taken out of the table, which only warns (once per
// name), so one pool leaving never stops every placement.
func poolReader(path string, has poolHasFlag, warn io.Writer) (func() ([]placer.Pool, error), error) {
	table, err := readPools(path)
	if err != nil {
		return nil, err
	}
	if _, unknown := placerPools(table, has); len(unknown) > 0 {
		return nil, fmt.Errorf("--pool-has names pools the table doesn't hold: %s", strings.Join(unknown, ", "))
	}
	warned := map[string]bool{}
	return func() ([]placer.Pool, error) {
		table, err := readPools(path)
		if err != nil {
			return nil, err
		}
		pools, unknown := placerPools(table, has)
		for _, name := range unknown {
			if !warned[name] {
				warned[name] = true
				fmt.Fprintf(warn, "place: pool %s left the pool table; its --pool-has is unused\n", name)
			}
		}
		return pools, nil
	}, nil
}

// placerPools joins the pool table with --pool-has: each pool with the toolchains its workers have, and the names
// --pool-has gives that the table doesn't hold.
func placerPools(table []judge.PoolEntry, has poolHasFlag) ([]placer.Pool, []string) {
	pools := []placer.Pool{}
	for _, entry := range table {
		pools = append(pools, placer.Pool{PoolEntry: entry, Has: has[entry.Name]})
	}
	unknown := []string{}
	for name := range has {
		if !slices.ContainsFunc(table, func(entry judge.PoolEntry) bool { return entry.Name == name }) {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	return pools, unknown
}

// runOptions are what every placed run shares: where its files go, the wire, the source its pool version is named
// from, and its priority.
type runOptions struct {
	runs     string
	wire     string
	source   string
	priority int
	binary   string // the loom that runs it; empty is this one
}

// runArguments is the `loom run` that runs a placement: uncached, on its pools alone, under the future's run id, every
// pool strict (each unit is a test job) with its toolchains, kinds, cpus and memory, so the coordinator's own fit
// holds the pool choice the placer made. A strict unit may go quiet for the unit ceiling's 1800 s, as the judge's
// reruns allow.
func runArguments(placement placer.Placement, options runOptions) []string {
	arguments := []string{"run", "--uncached", "--slots", "none", "--run-id", placement.Run, "--wire", options.wire, "--source", options.source,
		"--priority", strconv.Itoa(options.priority), "--silence-drop", strconv.Itoa(int(strictSilence.Seconds())),
		"--record", filepath.Join(options.runs, placement.Run+".record")}
	for _, pool := range placement.Pools {
		arguments = append(arguments, "--strict-pool", fmt.Sprintf("%s=%d", pool.Name, pool.Slots),
			"--pool-cpus", fmt.Sprintf("%s=%d", pool.Name, pool.Cpus), "--pool-memory", fmt.Sprintf("%s=%d", pool.Name, pool.MemoryMegabytes))
		if len(pool.Has) > 0 {
			arguments = append(arguments, "--pool-has", pool.Name+"="+strings.Join(pool.Has, ","))
		}
		if len(pool.Kinds) > 0 {
			arguments = append(arguments, "--pool-kinds", pool.Name+"="+strings.Join(pool.Kinds, ","))
		}
	}
	return append(arguments, filepath.Join(options.runs, placement.Run+".json"))
}

// startRun writes the placement's job to <runs>/<run>.json and starts this binary's `loom run` on it in a session of
// its own, its output in <run>.log, mode 600 since it prints the run's viewer token. It outlives the placer: the
// shipped unit has KillMode=process, so a restart stops only the placer. The placer never waits on its verdict, the
// judge's to read; when it ends, its status and log tail go to exits, so a run that ended before its first event is
// voided at once.
func startRun(placement placer.Placement, options runOptions, exits chan<- placer.Exit) error {
	job, err := json.MarshalIndent(placement.Job, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(options.runs, placement.Run+".json"), job, 0o600); err != nil {
		return err
	}
	binary := options.binary
	if binary == "" {
		if binary, err = os.Executable(); err != nil {
			return err
		}
	}
	logPath := filepath.Join(options.runs, placement.Run+".log")
	log, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer log.Close()
	command := exec.Command(binary, runArguments(placement, options)...)
	command.Stdout, command.Stderr = log, log
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		return err
	}
	go func() {
		// Reaped when it ends, so a long-lived placer leaves no zombies, and told to the placer.
		status := "exit 0"
		if err := command.Wait(); err != nil {
			status = err.Error()
		}
		content, _ := os.ReadFile(logPath)
		exits <- placer.Exit{Future: placement.Future, Attempt: placement.Attempt, Run: placement.Run, Status: status, Tail: logTail(content, 5)}
	}()
	return nil
}

// stopRun ends a placed run by its id: each `loom run` process whose arguments name exactly `--run-id <run>` gets
// SIGTERM, on which the coordinator drops the run's queued units from its pools and stops (Loom, Oct 10: what a hand
// stop did for three withdrawn branches). Runs outlive the placer that started them, so they're found by their own
// arguments, never by a handle or a pid file a restart would lose. A run with no process left has ended: no error.
func stopRun(run string, log io.Writer) error {
	listed, err := exec.Command("ps", "-eo", "pid=,args=").Output()
	if err != nil {
		return fmt.Errorf("listing processes: %w", err)
	}
	for _, pid := range runProcesses(string(listed), run) {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("signalling %s's process %d: %w", run, pid, err)
		}
		fmt.Fprintf(log, "sent SIGTERM to %s's loom run, process %d\n", run, pid)
	}
	return nil
}

// runProcesses are the pids in a `ps -eo pid=,args=` listing whose arguments are a `loom run` naming exactly
// `--run-id <run>`: future-<tree>-1 is never future-<tree>-10.
func runProcesses(listing, run string) []int {
	pids := []int{}
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		arguments := fields[1:]
		names, isRun := false, slices.Contains(arguments, "run")
		for index := 0; index+1 < len(arguments); index++ {
			if arguments[index] == "--run-id" && arguments[index+1] == run {
				names = true
			}
		}
		if isRun && names {
			pids = append(pids, pid)
		}
	}
	return pids
}

// tokenPattern is a Loom token, base64url claims and signature: a log line naming a run's page carries a viewer one.
var tokenPattern = regexp.MustCompile(`[A-Za-z0-9_-]{16,}\.[A-Za-z0-9_-]{16,}`)

// logTail is a run log's last lines, joined, with every token taken out: it goes into a void's cause, which Queue keeps.
func logTail(content []byte, lines int) string {
	all := strings.Split(strings.TrimSpace(string(content)), "\n")
	tail := all[max(0, len(all)-lines):]
	return tokenPattern.ReplaceAllString(strings.Join(tail, " | "), "<token>")
}

// pruneRuns removes each run file under directory (its job, log and record) last written over keep ago, by name, and
// says how many it removed. Nothing but those files is touched.
func pruneRuns(directory string, keep time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if !entry.Type().IsRegular() || !strings.HasPrefix(name, "future-") || !slices.Contains([]string{".json", ".log", ".record"}, filepath.Ext(name)) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) < keep {
			continue
		}
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}
