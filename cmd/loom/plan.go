package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/system-inc/loom/planner"
)

// plan is the planner's pull loop on the coordinator host (contract v1.1, the planning seam): it plans every future
// Queue serves at GET /futures?state=unplanned and posts each plan back.
func plan(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	queue := flags.String("queue", "", "loom-pipeline's base URL")
	tokenFile := flags.String("token-file", "", "file holding the coordinator token")
	repository := flags.String("repository", "", "a local clone of the repository the futures are in")
	gateTools := flags.String("gate-tools", "", "the gate tools checkout, for executors.txt's reads lines")
	runner := flags.String("runner", "", "the runner binary, whose sha256 is in every key")
	interval := flags.Duration("interval", 10*time.Second, "time between pulls")
	once := flags.Bool("once", false, "pull once and exit")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *queue == "" || *tokenFile == "" || *repository == "" || *gateTools == "" {
		fmt.Fprintln(stderr, "usage: loom plan --queue <url> --token-file <path> --repository <clone> --gate-tools <dir> [--runner <binary>] [--interval 10s] [--once]")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "plan:", err)
		return 1
	}
	tools, err := planner.ProbeTools(*runner)
	if err != nil {
		fmt.Fprintln(stderr, "plan: tools:", err)
		return 1
	}
	client := planner.QueueClient{Base: *queue, Token: strings.TrimSpace(string(token))}
	for {
		count, err := planner.PullOnce(client, planner.GitCheckout(*repository), *gateTools, tools, planner.HTTPIndex{Client: client})
		if count > 0 {
			fmt.Fprintf(stdout, "planned %d futures\n", count)
		}
		if err != nil {
			fmt.Fprintln(stderr, "plan:", err)
			if *once {
				return 1
			}
		}
		if *once {
			return 0
		}
		time.Sleep(*interval)
	}
}
