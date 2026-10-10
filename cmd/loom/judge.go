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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/coordinator"
	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

// judgeLoop is the judge's pull loop on the coordinator host, beside `loom plan` (#82tz9ty): every future Queue holds
// planned and undecided, once its run has finished, is decided by judge.Decide with alone reruns at RerunPriority,
// and its batch is posted to Queue's /futures/<tree>/verdicts. Main's recorded verdicts are judge.NoMainRecords until
// Queue's index answers by unit at a base, so a failure main shares is the change's until then (it errs toward red).
func judgeLoop(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "carried" {
		return judgeCarried(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "witness" {
		return judgeWitness(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "gate" {
		return judgeGate(arguments[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("judge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	queue := flags.String("queue", "", "loom-pipeline's base URL")
	tokenFile := flags.String("token-file", "", "file holding the coordinator token")
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin, where runs' events live and reruns are placed")
	source := flags.String("source", defaultSource(), "this repository's checkout, to build the pool runner's version from")
	local := flags.Int("local", 0, "rerun on this machine with this many slots instead of pools")
	var pools poolSlotsFlag
	flags.Var(&pools, "pool", "place reruns on a pool on the wire, <name>=<slots>; repeatable")
	poolHas := poolHasFlag{}
	flags.Var(poolHas, "pool-has", "the toolchains every worker of a pool has, <name>=<toolchain>,...; repeatable")
	interval := flags.Duration("interval", 10*time.Second, "time between pulls")
	once := flags.Bool("once", false, "pull once and exit")
	dryRun := flags.Bool("dry-run", false, "judge and print each future's batch, posting nothing (before cutover, a posted green can land)")
	postOnly := flags.String("post", "", "with --dry-run, post this one future's batch (its tree sha) and print the rest: the first live batch, on Loom's word")
	postParity := flags.Bool("post-parity", false, "with --dry-run, post the batches of parity futures (which never land) and print the rest: the steady judge before cutover")
	void := flags.String("void", "", "post one listed future's run as void and exit, <tree>:<attempt> (the attempt Queue lists); needs --cause")
	cause := flags.String("cause", "", "with --void, why the run is void, which leads the decision's problems")
	censusRows := flags.String("census-rows", "", "the skip census's rows, comma-separated files (the tools tree's skips.json and census-extra.json); every unit whose tests pass is held to it")
	censusHeavy := flags.String("census-heavy", "", "with --census-rows, the gate tools' cloud/fast-gate/heavy-units.tsv: declared heavy deferrals, classed heavy")
	censusGit := flags.String("census-git", "", "with --census-rows, a clone of Adamic whose origin answers whether a pending skip's awaited branch is on main")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *queue == "" || *tokenFile == "" || (*local == 0 && len(pools) == 0) {
		fmt.Fprintln(stderr, "usage: loom judge --queue <url> --token-file <path> (--pool <name>=<slots>... | --local N) [--pool-has <name>=<toolchains>] [--wire <url>] [--interval 10s] [--once] [--dry-run [--post <tree> | --post-parity]] [--void <tree>:<attempt> --cause <why>]; loom judge carried --tree <tree> --attempt <N> ...")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "judge:", err)
		return 1
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintln(stderr, "judge:", err)
		return 1
	}
	var slots []coordinator.Machine
	for range *local {
		slots = append(slots, coordinator.LocalMachine{})
	}
	if len(pools) > 0 {
		version, err := runnerVersion(*source)
		if err != nil {
			fmt.Fprintln(stderr, "judge:", err)
			return 1
		}
		for _, wanted := range pools {
			// RerunAlone lifts each pool to RerunPriority itself.
			machine := &coordinator.PoolMachine{Pool: wanted.name, Has: poolHas[wanted.name], Wire: *wire, Secret: secret, Version: version, GoPlatform: poolPlatform, Log: stdout}
			for range wanted.slots {
				slots = append(slots, machine)
			}
		}
	}
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	config := coordinator.Config{Wire: *wire, Secret: secret, Slots: slots, Uncached: true, Log: stdout}
	client := strings.TrimSpace(string(token))
	puller := judge.NewPuller(judge.Puller{
		Source: judge.HTTPFutures{Base: *queue, Token: client},
		RunOf:  coordinator.FutureRun,
		Read: func(run string) ([]protocol.Event, error) {
			return coordinator.ReadRunEvents(runContext, *wire, secret, run)
		},
		Rerun: func(keyParts json.RawMessage, sha string) ([]protocol.Event, error) {
			var parts planner.KeyParts
			if err := json.Unmarshal(keyParts, &parts); err != nil {
				return nil, fmt.Errorf("a planned unit's keyParts: %w", err)
			}
			unit, err := planner.JobUnitFor(parts, sha)
			if err != nil {
				return nil, err
			}
			result, err := coordinator.RerunAlone(runContext, config, unit)
			return result.Events, err
		},
		Log: func(run, sha256 string) ([]byte, error) {
			return coordinator.ReadRunBlob(runContext, *wire, secret, run, sha256)
		},
		Main:  judge.NoMainRecords{},
		Queue: judge.Queue(judge.HTTPQueue{Base: *queue, Token: client}),
		Loop: judge.Loop{Now: time.Now, RequireTestLog: true, Reused: judge.HTTPReused{Base: *queue, Token: client}, Blobs: judge.HTTPBlobs{Base: *queue, Token: func() (string, error) {
			// Each record's tests list goes to the action store, which takes a build token only (Loom, Oct 10 01:17Z).
			return protocol.MintToken(secret, protocol.TokenClaims{Run: "judge", Scope: protocol.ScopeBuild, Expires: time.Now().Add(time.Hour).Unix()})
		}}},
		Stale: judge.StaleAfter,
	})
	if *censusRows != "" {
		census, err := censusConfig(strings.Split(*censusRows, ","), *censusHeavy, *censusGit)
		if err != nil {
			fmt.Fprintln(stderr, "judge:", err)
			return 1
		}
		puller.Loop.Census = census
	}
	if *void != "" {
		// A void moves nothing toward main: it is posted live even beside --dry-run, and Queue lists the next attempt.
		tree, attemptText, found := strings.Cut(*void, ":")
		attempt, err := strconv.Atoi(attemptText)
		if !found || err != nil || attempt < 1 || *cause == "" {
			fmt.Fprintln(stderr, "judge: --void <tree>:<attempt> --cause <why>")
			return 2
		}
		post, err := puller.VoidOne(tree, attempt, *cause)
		if err != nil {
			fmt.Fprintln(stderr, "judge:", err)
			return 1
		}
		fmt.Fprintf(stdout, "posted void: /futures/%s/verdicts run %s, %d units: %s\n", tree, post.Run, len(post.Plan), strings.Join(post.Decision.Problems, "; "))
		return 0
	}
	if (*postOnly != "" || *postParity) && !*dryRun {
		fmt.Fprintln(stderr, "judge: --post and --post-parity name what posts while every other future stays dry, so they need --dry-run")
		return 2
	}
	if *dryRun {
		parity := parityFutures{source: puller.Source, trees: map[string]bool{}}
		puller.Source = parity
		puller.Queue = scopedQueue{post: *postOnly, parity: *postParity, trees: parity.trees, live: puller.Queue, dry: printedQueue{out: stdout}}
	}
	for runContext.Err() == nil {
		count, err := puller.PullOnce()
		if count > 0 {
			fmt.Fprintf(stdout, "judged %d futures\n", count)
		}
		if err != nil {
			fmt.Fprintln(stderr, "judge:", err)
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

// printedQueue is --dry-run's queue: it prints each batch as the line Queue would have been sent, and posts nothing.
type printedQueue struct {
	out io.Writer
}

func (queue printedQueue) PostVerdicts(future string, post judge.FuturePost) error {
	encoded, err := json.Marshal(post)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(queue.out, "dry run, not posted: /futures/%s/verdicts %s\n", future, encoded)
	return err
}

// scopedQueue posts the named future's batch (--post), or every parity future's (--post-parity), to Queue and prints
// every other: a parity future never lands, so posting it can't move main before cutover.
type scopedQueue struct {
	post   string
	parity bool
	trees  map[string]bool // the listed futures that are parity runs, as the listing said
	live   judge.Queue
	dry    judge.Queue
}

func (queue scopedQueue) PostVerdicts(future string, post judge.FuturePost) error {
	if (queue.post != "" && future == queue.post) || (queue.parity && queue.trees[future]) {
		return queue.live.PostVerdicts(future, post)
	}
	return queue.dry.PostVerdicts(future, post)
}

// parityFutures notes which listed futures are parity runs, for scopedQueue.
type parityFutures struct {
	source judge.FutureSource
	trees  map[string]bool
}

func (futures parityFutures) Planned() ([]judge.PlannedFuture, error) {
	listed, err := futures.source.Planned()
	for _, future := range listed {
		futures.trees[future.Future] = future.Parity
	}
	return listed, err
}

// censusConfig loads the census's rows and binds its pending check to a clone of Adamic. The units run on the pool's
// platform, so rows with platforms are held to its GOOS.
func censusConfig(paths []string, heavyPath, repository string) (*judge.CensusConfig, error) {
	config := &judge.CensusConfig{Platform: strings.Split(poolPlatform, "/")[0]}
	if heavyPath != "" {
		content, err := os.ReadFile(heavyPath)
		if err != nil {
			return nil, err
		}
		if config.Heavy, err = judge.ParseHeavyUnits(string(content)); err != nil {
			return nil, err
		}
	}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		rows, err := judge.LoadCensusRows(file)
		file.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		config.Rows = append(config.Rows, rows...)
	}
	if repository != "" {
		config.Landed = landedOnMain(repository)
	}
	return config, nil
}

// landedOnMain is skipcensus's (cmd/main.go): it asks origin for main's tip and the branch's, fetches both, and says
// whether main contains the branch's tip. A branch origin no longer has is an error, which fails the census closed.
// Answers are kept for the process's life, so a branch read on main is never asked again.
func landedOnMain(repository string) judge.Landed {
	answers := map[string]bool{}
	return func(branch string) (bool, error) {
		if answer, ok := answers[branch]; ok {
			return answer, nil
		}
		main, err := remoteTip(repository, "main")
		if err != nil {
			return false, err
		}
		tip, err := remoteTip(repository, branch)
		if err != nil {
			return false, err
		}
		if output, err := exec.Command("git", "-C", repository, "fetch", "-q", "origin", main, tip).CombinedOutput(); err != nil {
			return false, fmt.Errorf("fetching main and %s: %v: %s", branch, err, strings.TrimSpace(string(output)))
		}
		err = exec.Command("git", "-C", repository, "merge-base", "--is-ancestor", tip, main).Run()
		var exit *exec.ExitError
		switch {
		case err == nil:
			answers[branch] = true
		case errors.As(err, &exit) && exit.ExitCode() == 1:
			// Off main today; asked again next time, since it may land.
			return false, nil
		default:
			return false, fmt.Errorf("is %s on main: %v", branch, err)
		}
		return true, nil
	}
}

func remoteTip(repository, branch string) (string, error) {
	output, err := exec.Command("git", "-C", repository, "ls-remote", "origin", "refs/heads/"+branch).Output()
	if err != nil {
		return "", fmt.Errorf("asking origin for %s: %v", branch, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) < 2 || fields[1] != "refs/heads/"+branch {
		return "", fmt.Errorf("origin has no branch %s", branch)
	}
	return fields[0], nil
}

// judgeCarried prints the units the judge will carry into one attempt of a listed future, one "<unitKey> <source run>"
// line each (Loom, Oct 10 01:27Z): Fabric's placer places every other planned unit, so placer and judge can't disagree
// about which units an attempt runs. It reads the same listing and runs, through the same code, as the judge's loop,
// and any failure exits nonzero with nothing printed, so the placer fails closed.
func judgeCarried(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("judge carried", flag.ContinueOnError)
	flags.SetOutput(stderr)
	queue := flags.String("queue", "", "loom-pipeline's base URL")
	tokenFile := flags.String("token-file", "", "file holding the coordinator token")
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin, where runs' events live")
	tree := flags.String("tree", "", "the future's tested tree sha")
	attempt := flags.Int("attempt", 0, "the attempt being placed")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *queue == "" || *tokenFile == "" || *tree == "" || *attempt < 1 {
		fmt.Fprintln(stderr, "usage: loom judge carried --queue <url> --token-file <path> --tree <tree> --attempt <N> [--wire <url>]")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "judge carried:", err)
		return 1
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintln(stderr, "judge carried:", err)
		return 1
	}
	puller := judge.Puller{
		Source: judge.HTTPFutures{Base: *queue, Token: strings.TrimSpace(string(token))},
		RunOf:  coordinator.FutureRun,
		Read: func(run string) ([]protocol.Event, error) {
			return coordinator.ReadRunEvents(context.Background(), *wire, secret, run)
		},
	}
	units, err := puller.Carried(*tree, *attempt)
	if err != nil {
		fmt.Fprintln(stderr, "judge carried:", err)
		return 1
	}
	for _, unit := range units {
		fmt.Fprintf(stdout, "%s %s\n", unit.UnitKey, unit.Run)
	}
	return 0
}

// judgeWitness compares an uncached witness run's verdicts with the planner's plan at the same tree (#82d430f, proof 3,
// Loom Oct 10 01:29Z), prints the report, and exits 1 on any key fault (a reused key red uncached) or unwitnessed reuse
// (Loom 01:30Z: a reused unit the witness never ran would pass the proof vacuously, so it fails it like a fault), 3
// when the witness has a void to rerun, and 0 only when it's clean.
func judgeWitness(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("judge witness", flag.ContinueOnError)
	flags.SetOutput(stderr)
	planPath := flags.String("plan", "", "the planner's plan at the tree, PlannedUnitWire JSON")
	verdictsPath := flags.String("verdicts", "", "the uncached run's verdict records, one JSON line each (records or Queue log events)")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *planPath == "" || *verdictsPath == "" {
		fmt.Fprintln(stderr, "usage: loom judge witness --plan <plan.json> --verdicts <verdicts.jsonl>")
		return 2
	}
	content, err := os.ReadFile(*planPath)
	if err != nil {
		fmt.Fprintln(stderr, "judge witness:", err)
		return 2
	}
	plan, err := judge.ReadWitnessPlan(content)
	if err != nil {
		fmt.Fprintln(stderr, "judge witness:", *planPath+":", err)
		return 2
	}
	file, err := os.Open(*verdictsPath)
	if err != nil {
		fmt.Fprintln(stderr, "judge witness:", err)
		return 2
	}
	defer file.Close()
	verdicts, err := judge.ReadWitnessVerdicts(file)
	if err != nil {
		fmt.Fprintln(stderr, "judge witness:", *verdictsPath+":", err)
		return 2
	}
	report := judge.CompareWitness(plan, verdicts)
	for _, line := range report.Lines() {
		fmt.Fprintln(stdout, line)
	}
	switch {
	case len(report.KeyFaults) > 0, len(report.Unwitnessed) > 0:
		return 1
	case !report.Clean():
		return 3
	}
	return 0
}

// judgeGate gates the gate through the new path (#82tz9ty, #4rtjr81): it reads Queue's log, rebuilds the newest decided
// batch of each mutant in the suite (its future is the mutant's own tree, a parity run) and of the canary of main's tip,
// reads each as GateTheGate does, prints one line per mutant and the verdict, and exits 0 only when the tools may promote.
func judgeGate(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("judge gate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	queue := flags.String("queue", "", "loom-pipeline's base URL")
	tokenFile := flags.String("token-file", "", "file holding the coordinator token")
	suitePath := flags.String("suite", "", "the gate-mutant suite, Adamic's cloud/gate-mutants.tsv")
	canaryTree := flags.String("canary", "", "the tree of main's canary, a parity run of main's tip")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *queue == "" || *tokenFile == "" || *suitePath == "" {
		fmt.Fprintln(stderr, "usage: loom judge gate --queue <url> --token-file <path> --suite <gate-mutants.tsv> [--canary <tree>]")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "judge gate:", err)
		return 2
	}
	suiteText, err := os.ReadFile(*suitePath)
	if err != nil {
		fmt.Fprintln(stderr, "judge gate:", err)
		return 2
	}
	events, err := judge.HTTPLog{Base: *queue, Token: strings.TrimSpace(string(token))}.Events()
	if err != nil {
		fmt.Fprintln(stderr, "judge gate:", err)
		return 2
	}
	lines, promote, err := gateReport(string(suiteText), events, *canaryTree)
	if err != nil {
		fmt.Fprintln(stderr, "judge gate:", err)
		return 2
	}
	for _, line := range lines {
		fmt.Fprintln(stdout, line)
	}
	if !promote {
		return 1
	}
	return 0
}

// gateReport is judgeGate's reading of a log: a line per mutant (its name, sha and JudgeMutant's word, or "unread"
// when its future has no decided batch), the canary's, and GateTheGate's verdict last.
func gateReport(suiteText string, events []judge.LogEvent, canaryTree string) ([]string, bool, error) {
	suite, err := judge.ParseMutants(suiteText)
	if err != nil {
		return nil, false, err
	}
	batches, err := judge.DecidedBatches(events)
	if err != nil {
		return nil, false, err
	}
	lines := []string{}
	readings := map[string]judge.Reading{}
	for _, mutant := range suite {
		post, found := batches[mutant.Sha]
		if !found {
			lines = append(lines, fmt.Sprintf("%s %s unread: no decided batch for its future yet", mutant.Name, mutant.Sha[:8]))
			continue
		}
		reading, err := judge.ReadingOf(mutant.Sha, post)
		if err != nil {
			return nil, false, fmt.Errorf("mutant %s: %w", mutant.Name, err)
		}
		readings[mutant.Name] = reading
		lines = append(lines, fmt.Sprintf("%s %s %s (run %s, %s at %q, %d passed)", mutant.Name, mutant.Sha[:8], judge.JudgeMutant(mutant, reading), post.Run, reading.Status, reading.FirstStep, reading.PassedTests))
	}
	canary := judge.Reading{Status: "unread"}
	if post, found := batches[canaryTree]; canaryTree != "" && found {
		if canary, err = judge.ReadingOf(canaryTree, post); err != nil {
			return nil, false, fmt.Errorf("canary: %w", err)
		}
	}
	lines = append(lines, fmt.Sprintf("canary %s %s with %d passed", strings.TrimSpace(canaryTree), canary.Status, canary.PassedTests))
	verdict := judge.GateTheGate(suite, readings, canary)
	if verdict.Promote {
		return append(lines, "promote: every mutant reads as declared and main's canary is green"), true, nil
	}
	for _, reason := range verdict.Hold {
		lines = append(lines, "hold: "+reason)
	}
	return lines, false, nil
}
