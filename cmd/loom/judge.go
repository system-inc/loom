package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
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
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *queue == "" || *tokenFile == "" || (*local == 0 && len(pools) == 0) {
		fmt.Fprintln(stderr, "usage: loom judge --queue <url> --token-file <path> (--pool <name>=<slots>... | --local N) [--pool-has <name>=<toolchains>] [--wire <url>] [--interval 10s] [--once]")
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
	puller := judge.Puller{
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
		Main:  judge.NoMainRecords{},
		Queue: judge.Queue(judge.HTTPQueue{Base: *queue, Token: client}),
		Loop:  judge.Loop{Now: time.Now},
	}
	if *dryRun {
		puller.Queue = printedQueue{out: stdout}
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
