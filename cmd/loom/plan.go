package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/system-inc/loom/planner"
)

// plan is the planner's pull loop on the coordinator host (contract v1.1, the planning seam): it plans every future
// Queue serves at GET /futures?state=unplanned and posts each plan back.
func plan(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "by-key" {
		return planByKey(arguments[1:], stdout, stderr)
	}
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	queue := flags.String("queue", "", "loom-pipeline's base URL")
	tokenFile := flags.String("token-file", "", "file holding the coordinator token")
	repository := flags.String("repository", "", "a local clone of the repository the futures are in")
	gateTools := flags.String("gate-tools", "", "the gate tools checkout, for executors.txt's reads lines")
	gateToolsRef := flags.String("gate-tools-ref", "", "a branch of the gate tools' origin to fetch and check out before each pull")
	runnerShaFile := flags.String("runner-sha-file", "", "the file holding the pool's pinned runner sha256, in every key")
	interval := flags.Duration("interval", 10*time.Second, "time between pulls")
	once := flags.Bool("once", false, "pull once and exit")
	only := flags.String("future", "", "plan only this future (its tree sha) and leave every other unplanned")
	gateInputsFile := flags.String("gate-inputs-file", "", "the file holding the gate inputs' manifest sha256 a parity run's box record ran with")
	poolsFile := flags.String("pools", planner.PoolsFile, "the pool table, for declared needs' plan-time check")
	noReuseFile := flags.String("no-reuse", planner.NoReuseFile, "unit keys whose passed verdicts may not be reused, one per line with why")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *queue == "" || *tokenFile == "" || *repository == "" || *gateTools == "" || *runnerShaFile == "" {
		fmt.Fprintln(stderr, "usage: loom plan --queue <url> --token-file <path> --repository <clone> --gate-tools <dir> [--gate-tools-ref <branch>] --runner-sha-file <path> [--interval 10s] [--once] [--future <tree sha>] [--gate-inputs-file <path>]")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "plan:", err)
		return 1
	}
	tools, err := planner.ProbeTools(*runnerShaFile)
	if err != nil {
		fmt.Fprintln(stderr, "plan: tools:", err)
		return 1
	}
	planner.PoolsFile = *poolsFile
	planner.NoReuseFile = *noReuseFile
	gateInputs := ""
	if *gateInputsFile != "" {
		content, err := os.ReadFile(*gateInputsFile)
		if err != nil {
			fmt.Fprintln(stderr, "plan: gate inputs:", err)
			return 1
		}
		gateInputs = strings.TrimSpace(string(content))
	}
	client := planner.QueueClient{Base: *queue, Token: strings.TrimSpace(string(token))}
	for {
		if *gateToolsRef != "" {
			// A stale executors.txt would key a unit without a reads line its tools now declare.
			if err := refreshGateTools(*gateTools, *gateToolsRef); err != nil {
				fmt.Fprintln(stderr, "plan: gate tools:", err)
				if *once {
					return 1
				}
				time.Sleep(*interval)
				continue
			}
		}
		count, err := planner.PullOnce(client, planner.GitCheckout(*repository), *gateTools, tools, planner.HTTPIndex{Client: client}, *only, gateInputs)
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

// refreshGateTools moves the gate tools checkout to its origin's branch tip.
func refreshGateTools(directory, branch string) error {
	for _, arguments := range [][]string{{"fetch", "--quiet", "origin", branch}, {"checkout", "--quiet", "--force", "--detach", "FETCH_HEAD"}} {
		if output, err := exec.Command("git", append([]string{"-C", directory}, arguments...)...).CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

// planByKey writes a parity future's plan by selection against its base's keys, for proof 3's compare with the
// future's uncached run (Loom, Oct 10 01:29Z): the base keyed with the future's own parity parts, every key passed.
func planByKey(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("plan by-key", flag.ContinueOnError)
	flags.SetOutput(stderr)
	queue := flags.String("queue", "", "loom-pipeline's base URL")
	tokenFile := flags.String("token-file", "", "file holding the coordinator token")
	repository := flags.String("repository", "", "the planner's clone")
	gateTools := flags.String("gate-tools", "", "the gate tools checkout, for executors.txt's reads lines")
	runnerShaFile := flags.String("runner-sha-file", "", "the file holding the pool's pinned runner sha256")
	gateInputsFile := flags.String("gate-inputs-file", "", "the file holding the gate inputs' manifest sha256")
	future := flags.String("future", "", "the parity future (its tree sha)")
	out := flags.String("out", "", "where to write the plan, a JSON list of planned units")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if *queue == "" || *tokenFile == "" || *repository == "" || *gateTools == "" || *runnerShaFile == "" || *gateInputsFile == "" || *future == "" || *out == "" {
		fmt.Fprintln(stderr, "usage: loom plan by-key --queue <url> --token-file <path> --repository <clone> --gate-tools <dir> --runner-sha-file <path> --gate-inputs-file <path> --future <tree sha> --out <file>")
		return 2
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		fmt.Fprintln(stderr, "plan by-key:", err)
		return 1
	}
	gateInputs, err := os.ReadFile(*gateInputsFile)
	if err != nil {
		fmt.Fprintln(stderr, "plan by-key:", err)
		return 1
	}
	tools, err := planner.ProbeTools(*runnerShaFile)
	if err != nil {
		fmt.Fprintln(stderr, "plan by-key: tools:", err)
		return 1
	}
	client := planner.QueueClient{Base: *queue, Token: strings.TrimSpace(string(token))}
	planned, err := client.PlannedFuture(*future)
	if err != nil {
		fmt.Fprintln(stderr, "plan by-key:", err)
		return 1
	}
	inputs, err := client.ParityInputs(planned, strings.TrimSpace(string(gateInputs)))
	if err != nil {
		fmt.Fprintln(stderr, "plan by-key:", err)
		return 1
	}
	results, err := planner.PlanByKey(planner.GitCheckout(*repository), planned.Base, *future, *gateTools, tools, planner.ParitySelect{}, inputs)
	if err != nil {
		fmt.Fprintln(stderr, "plan by-key:", err)
		return 1
	}
	encoded, err := json.MarshalIndent(results, "", "  ")
	if err == nil {
		err = os.WriteFile(*out, append(encoded, '\n'), 0o644)
	}
	if err != nil {
		fmt.Fprintln(stderr, "plan by-key:", err)
		return 1
	}
	reused := 0
	for _, result := range results {
		if result.Decision == "reuse" {
			reused++
		}
	}
	fmt.Fprintf(stdout, "%s: %d units, %d reused, %d run, base %s\n", *future, len(results), reused, len(results)-reused, planned.Base)
	return 0
}
