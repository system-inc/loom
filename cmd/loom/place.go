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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/coordinator"
	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/placer"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

// place is the placer's pull loop on the coordinator host, beside `loom plan` and `loom judge`: every attempt Queue
// lists planned and undecided (GET /futures?state=planned, read only) that the ledger doesn't hold is placed whole in
// one detached `loom run --run-id future-<tree>-<attempt>` on the pools that take its units, or posted void as Loom's,
// neverPlaced, naming each unit it can't place (package placer).
func place(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("place", flag.ContinueOnError)
	flags.SetOutput(stderr)
	home, _ := os.UserHomeDir()
	queue := flags.String("queue", "", "loom's base URL")
	tokenFile := flags.String("token-file", "", "file holding the coordinator token")
	wire := flags.String("wire", "https://runs.loom.system.inc", "the wire's origin, where runs' events live and units are queued")
	source := flags.String("source", defaultSource(), "this repository's checkout, which `loom run` builds the pool runner's version from")
	poolsPath := flags.String("pools", filepath.Join(home, ".loom", "pools.json"), "the pool table, read every pass: each pool's runner, kinds, memory, cpus and cold mark")
	poolHas := poolHasFlag{}
	flags.Var(poolHas, "pool-has", "the toolchains every worker of a pool has, <name>=<toolchain>,..., as the judge's; a pool without one has none; repeatable")
	needsGit := flags.String("needs-git", "", "a clone of Adamic whose origin's loom/planner-reads holds unit-needs.json, for a unit whose plan carried no need")
	warmRunnersFile := flags.String("warm-runners-file", filepath.Join(home, "loom-judge", "warm-runners.txt"), "the steady judge's warm runners: a test unit keyed on one goes only to a cold pool; a missing file means none")
	warmAttempts := flags.String("warm-attempts", filepath.Join(home, "loom-judge", "warm-attempts.txt"), "the steady judge's warm-attempts file, for the units the judge carries; a missing file means none")
	ledgerPath := flags.String("ledger", filepath.Join(home, "loom-placer", "placed.jsonl"), "what was placed, by future and attempt, so a restart never places twice")
	runs := flags.String("runs", filepath.Join(home, "loom-placer", "runs"), "where each run's job, log and record go")
	priority := flags.Int("priority", 40, "the runs' priority on their pools, 0 to 1000 (40, a landing's)")
	poolSlots := flags.Int("pool-slots", 32, "the most units of one run queued on one pool at once")
	unfitEvery := flags.Duration("unfit-every", 30*time.Minute, "how soon a future voided as unplaceable is voided again for the same cause, under the judge's 45-minute backstop")
	interval := flags.Duration("interval", 10*time.Second, "time between pulls")
	once := flags.Bool("once", false, "pull once and exit")
	dryRun := flags.Bool("dry-run", false, "print each placement and void, start and post nothing, and write no ledger")
	storeFlags := addStoreFlags(flags)
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *queue == "" || *tokenFile == "" || *priority < 0 || *priority > 1000 || *poolSlots < 1 {
		fmt.Fprintln(stderr, "usage: loom place --queue <url> --token-file <path> --pool-has <name>=<toolchains>... [--pools <file>] [--needs-git <clone>] [--wire <url>] [--r2 <key file>] [--ledger <file>] [--runs <dir>] [--priority 40] [--pool-slots 32] [--interval 10s] [--once] [--dry-run]")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
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
	if content, err := os.ReadFile(*warmRunnersFile); err == nil {
		warmRunner = warmRunnerSet(string(content))
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stderr, "place:", err)
		return 1
	}
	if _, err := os.Stat(*warmAttempts); errors.Is(err, os.ErrNotExist) {
		*warmAttempts = ""
	}
	readTable := func() ([]placer.Pool, error) {
		table, err := readPools(*poolsPath)
		if err != nil {
			return nil, err
		}
		return placerPools(table, poolHas)
	}
	// Read once here so a bad table, or a --pool-has naming no pool in it, stops the placer at start.
	if _, err := readTable(); err != nil {
		fmt.Fprintln(stderr, "place:", err)
		return 1
	}
	client := strings.TrimSpace(string(token))
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	read := func(run string) ([]protocol.Event, error) {
		return coordinator.ReadRunEvents(runContext, *wire, secret, run)
	}
	carrier := judge.Puller{Loop: judge.Loop{Warm: warmRule(warmRunner, *warmAttempts, *poolsPath)}, RunOf: coordinator.FutureRun, Read: read}
	voider := judge.NewPuller(judge.Puller{RunOf: coordinator.FutureRun, Read: read, Queue: judge.HTTPQueue{Base: *queue, Token: client},
		Loop: judge.Loop{Now: time.Now, Reused: judge.HTTPReused{Base: *queue, Token: client}}})
	options := runOptions{runs: *runs, wire: *wire, source: *source, priority: *priority}
	start := func(placement placer.Placement) error { return startRun(placement, options) }
	var ledger placer.Ledger
	if *dryRun {
		voider.Queue, voider.Loop.Blobs = printedQueue{out: stdout}, &judge.StubBlobs{}
		ledger = &placer.MemoryLedger{}
		start = func(placement placer.Placement) error {
			fmt.Fprintf(stdout, "dry run, not started: loom %s\n", strings.Join(runArguments(placement, options), " "))
			return nil
		}
	} else {
		// A void's records go to the action store as the judge's do, straight to R2 with this machine's key.
		store, err := storeFlags.open(nil)
		if err != nil {
			fmt.Fprintln(stderr, "place:", err)
			return 1
		}
		voider.Loop.Blobs = judge.StoreBlobs{Store: store}
		if err := os.MkdirAll(*runs, 0o755); err != nil {
			fmt.Fprintln(stderr, "place:", err)
			return 1
		}
		if ledger, err = placer.OpenLedger(*ledgerPath); err != nil {
			fmt.Fprintln(stderr, "place:", err)
			return 1
		}
	}
	queueClient := planner.QueueClient{Base: *queue, Token: client}
	loop := &placer.Placer{
		Source:  judge.HTTPFutures{Base: *queue, Token: client},
		Carried: carrier.CarriedFrom,
		ChangePaths: func(change string) ([]string, error) {
			return queueClient.ChangePaths(planner.Future{Changes: []string{change}})
		},
		Pools:       readTable,
		Needs:       func() (planner.UnitNeeds, error) { return loadNeeds(*needsGit) },
		WarmRunners: warmRunner,
		Start:       start,
		Void: func(future judge.PlannedFuture, attempt int, cause string) error {
			_, err := voider.VoidListed(future, attempt, judge.InfraNeverPlaced, cause)
			return err
		},
		Ledger:     ledger,
		PoolSlots:  *poolSlots,
		UnfitEvery: *unfitEvery,
		Now:        time.Now,
		Log:        stdout,
	}
	for runContext.Err() == nil {
		count, err := loop.PlaceOnce()
		if count > 0 {
			fmt.Fprintf(stdout, "placed %d futures\n", count)
		}
		if err != nil {
			fmt.Fprintln(stderr, "place:", err)
			if *once {
				return 1
			}
		}
		if *once {
			return 0
		}
		select {
		case <-runContext.Done():
		case <-time.After(*interval):
		}
	}
	return 0
}

// placerPools joins the pool table with --pool-has: each pool with the toolchains its workers have. A --pool-has
// naming a pool the table doesn't hold is refused, since it would say nothing.
func placerPools(table []judge.PoolEntry, has poolHasFlag) ([]placer.Pool, error) {
	pools := []placer.Pool{}
	for _, entry := range table {
		pools = append(pools, placer.Pool{PoolEntry: entry, Has: has[entry.Name]})
	}
	for name := range has {
		if !slices.ContainsFunc(table, func(entry judge.PoolEntry) bool { return entry.Name == name }) {
			return nil, fmt.Errorf("--pool-has names pool %s, which the pool table doesn't hold", name)
		}
	}
	return pools, nil
}

// runOptions are what every placed run shares: where its files go, the wire, the source its pool version is named
// from, and its priority.
type runOptions struct {
	runs     string
	wire     string
	source   string
	priority int
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
// its own, its output in <run>.log: it outlives the placer (an update restarts the placer, never its runs; the unit
// needs KillMode=process for that), and the placer never waits on its verdict, which is the judge's to read.
func startRun(placement placer.Placement, options runOptions) error {
	job, err := json.MarshalIndent(placement.Job, "", "  ")
	if err != nil {
		return err
	}
	jobPath := filepath.Join(options.runs, placement.Run+".json")
	if err := os.WriteFile(jobPath, job, 0o644); err != nil {
		return err
	}
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(options.runs, placement.Run+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
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
	// Reaped when it ends, so a long-lived placer leaves no zombies.
	go command.Wait()
	return nil
}
