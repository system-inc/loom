// Command loom is the coordinator: it runs a job file on Loom's slots and prints the verdict. It exits 0 when
// the run is green, 1 when red, 2 when void, and 3 when the run couldn't be set up.
//
//	loom run [--uncached] [--local <slots> | --slots <file>] [--pool <name>=<slots>]... [--priority <n> [--pool-age-every <duration> [--pool-age-step <n>] [--pool-age-ceiling <n>]]] [--yield-to <gate slots>] [--record <file>] [--wire <url>] <job.json>
//	loom board [--days <n>]   prints the board's address with a board token
//	loom pool status <name>   prints a pool's queue and the workers serving it
//
// The slots come from ~/.loom/slots, one "box class" per line (class B is a box's area slot, S a small one),
// unless --local runs every unit on this machine. The token secret is ~/.loom/token-secret. For each box
// the runner is built from this repository's source for the box's platform, named for its version, and
// installed under the staged-rollout law (~/.loom/rollout.tsv): a version, which is the whole Go module's
// commit, so a new coordinator too, reaches a second box only after a green run on its first.
//
// --pool adds slots on a pool beside the others: units queued on the wire for machines Loom can't ssh into
// (Codex instances running loom-runner serve). The staged-rollout law doesn't reach a pool, since Loom
// doesn't install its runner, and a green there unlocks no box. --priority ranks the run's units on every pool it
// uses: a pool hands out the highest first, so the star's candidates (30) pass main's own gate (20), and main's
// pass a side candidate's (10). A unit already running finishes; the star takes each worker as its unit ends.
// --pool-age-every lifts a unit still waiting on a pool by --pool-age-step each time it passes, up to
// --pool-age-ceiling, so a lower tier is never starved for good by a stream of higher ones.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/coordinator"
	"github.com/system-inc/loom/gatelines"
	"github.com/system-inc/loom/ownerbridge"
	"github.com/system-inc/loom/protocol"
)

const usage = `usage:
  loom run [--uncached] [--local <slots> | --slots <file>] [--pool <name>=<slots>]... [--priority <n> [--pool-age-every <duration> [--pool-age-step <n>] [--pool-age-ceiling <n>]]] [--yield-to <gate slots>] [--record <file>] [--wire <url>] <job.json>
  loom board [--days <n>] [--wire <url>]
  loom pool status [--wire <url>] <name>
  loom pool token <pool> [--hours N]
  loom pool prompt <pool> --runner <sha256> [--until 55m]
  loom gate-lines [--once] [--interval <duration>] [--wire <url>]
  loom owner-bridge [--once] [--interval <duration>] [--pipeline <url>]
  loom top [--once] [--wire <url>]
  loom publish-token <name> [--days N] [--candidate]
  loom submit-token [--days N] <owner>
  loom coordinator-token <service> [--days N]
  loom judge --queue <url> --token-file <path> (--pool <name>=<slots>... | --local N) [--once] [--dry-run]
  loom place --queue <url> --token-file <path> --pool-has <name>=<toolchains>... [--pools <file>] [--once] [--dry-run]
  loom unit-needs --gate-tools <dir> --package <directory> [--run <pattern>]
  loom reads-check --tree <dir> --gate-tools <dir> --package <import path> --trace <file> [--unit-key <key>] [--key-parts <file> [--read-sets <dir>] [--no-reuse <file>]]
  loom build-actions --tree <dir> --gate-tools <dir> [--r2 <key file>] [--packages a,b] [--list]
  loom fetch-actions --cache <dir> [--read <url>] [--skip-native] <productKey>...
  loom store-audit [--r2 <key file>]
  loom build-tree --tree <dir> [--r2 <key file>] [--future <sha>] [--tree-key <key>] [--jobs N]
  loom build-trees --queue <url> --token-file <path> [--clone <dir>] [--ledger <file>] [--r2 <key file>] [--once]
  loom gate-inputs publish [--dir <dir>] [--manifest-file <path>] [--r2 <key file>] [--lifecycle-unchecked] [--dry-run]
  loom gate-inputs check [--manifest-file <path>] [<name>]
  loom push [--config <push.conf>]
  loom push install
  loom release watch [--config <file>] [--wire <url>]
  loom release status [--config <file>] [--wire <url>]
  loom release install [--config <file>]
  loom release mark <commit> <step> [--config <file>]
  loom release rollback [--config <file>]
  loom release resume [--retry] [--config <file>]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "plan" {
		return plan(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "judge" {
		return judgeLoop(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "place" {
		return place(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "unit-needs" {
		return unitNeeds(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "reads-check" {
		return readsCheck(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "board" {
		return board(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "top" {
		return top(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "owner-bridge" {
		return ownerBridge(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "gate-lines" {
		return gateLines(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "pool" {
		return pool(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "fleet" {
		return fleetCommand(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "submit-token" {
		return submitToken(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "coordinator-token" {
		return coordinatorToken(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "build-actions" {
		return buildActions(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "fetch-actions" {
		return fetchActions(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "store-audit" {
		return storeAudit(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "build-tree" {
		return buildTree(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "build-trees" {
		return buildTrees(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "gate-inputs" {
		return gateInputs(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "push" {
		return push(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "release" {
		return releaseCommand(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "publish-token" {
		return publishToken(arguments[1:], stdout, stderr)
	}
	if len(arguments) == 0 || arguments[0] != "run" {
		fmt.Fprint(stderr, usage)
		return 3
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	uncached := flags.Bool("uncached", false, "read and write no cache: every unit runs (what lands main)")
	local := flags.Int("local", 0, "run on this machine with this many slots instead of ~/.loom/slots")
	wire := flags.String("wire", "https://runs.loom.system.inc", "the wire's origin")
	source := flags.String("source", defaultSource(), "this repository's checkout, to build the runner from")
	slotsPath := flags.String("slots", "", "the slot allowance, one \"box class\" per line (default ~/.loom/slots)")
	recordPath := flags.String("record", "", "write the run's record, every event as a JSON line, to this file")
	yieldTo := flags.String("yield-to", "", "the gate's slot table: Loom uses a box's slots only while the gate's table doesn't hold them")
	var pools poolSlotsFlag
	flags.Var(&pools, "pool", "also place units on a pool on the wire, <name>=<slots>; repeatable")
	var strictPools poolSlotsFlag
	flags.Var(&strictPools, "strict-pool", "a pool whose workers serve --strict, <name>=<slots>: it takes the job's test jobs and nothing else; repeatable")
	runId := flags.String("run-id", "", "the run's id, such as a pipeline future's future-<tree>-<attempt>; empty makes one from the job's name")
	recordPlatform := flags.String("record-platform", "", "the platform whose verdict the run records, such as linux/amd64: units not marked portable go only to its machines")
	poolPlatforms := poolPlatformFlag{}
	flags.Var(poolPlatforms, "pool-platform", "the platform a pool's workers run when it isn't Linux's, <name>=<goos>/<goarch>; repeatable")
	poolHas := poolHasFlag{}
	flags.Var(poolHas, "pool-has", "the toolchains every worker of a pool has, <name>=<toolchain>,...: units that require one go only there; repeatable")
	silenceDrop := flags.Int("silence-drop", 0, "seconds a strict pool's started unit may go silent before its worker counts as gone; 0 means three of its runner's heartbeats")
	poolKinds := poolKindsFlag{}
	flags.Var(poolKinds, "pool-kinds", "the only unit kinds a pool takes, <name>=<kind>,... (box-phase=phase); a pool without it takes test and product units; repeatable")
	poolCpus := poolMemoryFlag{}
	flags.Var(poolCpus, "pool-cpus", "each worker's cpus in a pool, <name>=<cpus>: a unit declaring more never goes there; repeatable")
	poolMemory := poolMemoryFlag{}
	flags.Var(poolMemory, "pool-memory", "each worker's memory in a pool, <name>=<megabytes>: a unit declaring more never goes there; repeatable")
	priority := flags.Int("priority", 0, "the run's units' priority on its pools, 0 to 1000, highest handed out first")
	ageEvery := flags.Duration("pool-age-every", 0, "lift a pool unit that waits this long, and again each time after, so a lower tier never starves; 0 is off")
	ageStep := flags.Int("pool-age-step", 10, "how much higher each --pool-age-every lifts a waiting unit")
	ageCeiling := flags.Int("pool-age-ceiling", 0, "the highest priority a waiting unit is lifted to, 0 to 1000; 0 is 1000")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 1 {
		fmt.Fprint(stderr, usage)
		return 3
	}
	if *priority < 0 || *priority > 1000 {
		fmt.Fprintf(stderr, "loom: --priority is 0 to 1000, not %d\n", *priority)
		return 3
	}
	if *ageEvery < 0 || *ageStep < 0 || *ageCeiling < 0 || *ageCeiling > 1000 {
		fmt.Fprintf(stderr, "loom: --pool-age-every and --pool-age-step are 0 or more, --pool-age-ceiling 0 to 1000\n")
		return 3
	}
	home, _ := os.UserHomeDir()
	loomDirectory := filepath.Join(home, ".loom")
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fail := func(err error) int {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	job, err := readJob(flags.Arg(0))
	if err != nil {
		return fail(err)
	}
	secret, err := protocol.ReadTokenSecret(filepath.Join(loomDirectory, "token-secret"))
	if err != nil {
		return fail(err)
	}
	durations, err := coordinator.LoadDurations(filepath.Join(loomDirectory, "durations.tsv"))
	if err != nil {
		return fail(err)
	}
	rollout, err := coordinator.LoadRollout(filepath.Join(loomDirectory, "rollout.tsv"))
	if err != nil {
		return fail(err)
	}
	var slots []coordinator.Machine
	version := ""
	if *local > 0 {
		for range *local {
			slots = append(slots, coordinator.LocalMachine{})
		}
	} else {
		version, err = runnerVersion(*source)
		if err != nil {
			return fail(err)
		}
		if *slotsPath == "" {
			*slotsPath = filepath.Join(loomDirectory, "slots")
		}
		// --slots none runs on pools alone (--pool), with no box of ours.
		if *slotsPath != "none" {
			slots, err = boxSlots(runContext, *slotsPath, *source, version, loomDirectory, rollout, stdout)
			if err != nil {
				return fail(err)
			}
		}
	}
	poolMachines := map[string]bool{}
	if len(pools) > 0 || len(strictPools) > 0 {
		// The pool's workers are built from this source, the version every pool unit's cache key names.
		poolVersion, err := runnerVersion(*source)
		if err != nil {
			return fail(err)
		}
		for _, wanted := range append(append(poolSlotsFlag{}, pools...), strictPools...) {
			strict := slices.Contains(strictPools, wanted)
			machine := &coordinator.PoolMachine{Pool: wanted.name, Strict: strict, Priority: *priority, AgeEvery: *ageEvery, AgeStep: *ageStep, AgeCeiling: *ageCeiling,
				SilenceDrop: map[bool]time.Duration{true: time.Duration(*silenceDrop) * time.Second}[strict],
				Has:         poolHas[wanted.name], MemoryMegabytes: poolMemory[wanted.name], CoreCount: poolCpus[wanted.name], Kinds: poolKinds[wanted.name], Wire: *wire, Secret: secret, Version: poolVersion, GoPlatform: platformOf(poolPlatforms, wanted.name), Log: stdout}
			poolMachines[machine.Name()] = true
			for range wanted.slots {
				slots = append(slots, machine)
			}
		}
	}
	config := coordinator.Config{Wire: *wire, Secret: secret, Slots: slots, Uncached: *uncached, Durations: durations, RecordPlatform: *recordPlatform, Run: *runId, Log: stdout}
	if *yieldTo != "" {
		config.SlotLimit = yieldLimit(slots, *yieldTo)
	}
	result, err := coordinator.Run(runContext, config, job)
	// A run stopped by a signal takes its queued units off every pool it used, so they never sit ahead of live
	// work; a unit a worker already took runs on, unread.
	if runContext.Err() != nil && result.Run != "" {
		for _, wanted := range append(append(poolSlotsFlag{}, pools...), strictPools...) {
			if dropped, err := cancelPoolRun(&http.Client{Timeout: 15 * time.Second}, *wire, secret, wanted.name, result.Run); err != nil {
				fmt.Fprintf(stderr, "loom: dropping the run's queued units from pool %s: %v\n", wanted.name, err)
			} else {
				fmt.Fprintf(stdout, "stopped: dropped %d queued units from pool %s\n", dropped, wanted.name)
			}
		}
	}
	if err != nil {
		return fail(err)
	}
	if *recordPath != "" {
		if err := writeRecord(*recordPath, result); err != nil {
			fmt.Fprintf(stderr, "loom: writing the record: %v\n", err)
		}
	}
	switch result.Verdict.Status {
	case "green":
		if version != "" {
			for _, box := range result.Machines {
				if !poolMachines[box] {
					rollout.Green(version, box)
				}
			}
		}
		return 0
	case "red":
		return 1
	default:
		return 2
	}
}

// gateLines reads what the gate is doing from its own files and posts it to the board as the gate's lines,
// every interval, until stopped. --once prints one reading as JSON and posts nothing.
// ownerBridge sends each change's owner what they act on (landed, red, parked) from loom's owners' feed,
// with `ahra os send` from ~/Projects/ahra, once each: the last sequence sent is kept in ~/.loom/owner-bridge.seq.
func ownerBridge(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("owner-bridge", flag.ContinueOnError)
	flags.SetOutput(stderr)
	once := flags.Bool("once", false, "send what is owed once, then exit")
	// Each pass reads the log after the last owner event sent, so it is read at the pace an owner would notice.
	interval := flags.Duration("interval", 30*time.Second, "how often to read the feed")
	pipeline := flags.String("pipeline", "https://loom.system.inc", "loom's origin")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	bridge := &ownerbridge.Bridge{
		Pipeline:  *pipeline,
		Client:    &http.Client{Timeout: 30 * time.Second},
		StatePath: filepath.Join(home, ".loom", "owner-bridge.seq"),
		Token: func() string {
			token, _ := protocol.MintToken(secret, protocol.TokenClaims{Run: "owner-bridge", Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Hour).Unix()})
			return token
		},
		Send: func(owner string, text string) error {
			command := exec.Command("./node_modules/.bin/ahra", "os", "send", owner, text, "--from", "system_adamic_loom_web")
			command.Dir = filepath.Join(home, "Projects", "ahra")
			if output, err := command.CombinedOutput(); err != nil || !strings.Contains(string(output), "sent") {
				return fmt.Errorf("ahra os send: %v: %s", err, bytes.TrimSpace(output))
			}
			return nil
		},
	}
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	for runContext.Err() == nil {
		started := time.Now()
		sent, err := bridge.Once(runContext)
		if sent > 0 {
			fmt.Fprintf(stdout, "loom owner-bridge: sent %d\n", sent)
		}
		if err != nil {
			fmt.Fprintf(stderr, "loom owner-bridge: %v\n", err)
		}
		if *once {
			if err != nil {
				return 1
			}
			return 0
		}
		select {
		case <-runContext.Done():
		case <-time.After(max(0, *interval-time.Since(started))):
		}
	}
	return 0
}

func gateLines(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("gate-lines", flag.ContinueOnError)
	flags.SetOutput(stderr)
	once := flags.Bool("once", false, "print one reading as JSON and post nothing")
	interval := flags.Duration("interval", 3*time.Second, "how often to read and post")
	wire := flags.String("wire", "https://runs.loom.system.inc", "the wire's origin")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 3
	}
	home, _ := os.UserHomeDir()
	reader := gatelines.NewReader(filepath.Join(home, ".adamic-fast-gate-watch"), filepath.Join(home, ".adamic-full-gate"))
	runContext, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if *once {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		encoder.Encode(reader.Read(runContext))
		return 0
	}
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	client := &http.Client{Timeout: 15 * time.Second}
	last := ""
	lastPosted := time.Time{}
	for runContext.Err() == nil {
		started := time.Now()
		lines := reader.Read(runContext)
		body, _ := json.Marshal(lines)
		// The time changes every reading; post when anything else did, and at least every 30 s as a heartbeat.
		withoutTime := strings.Replace(string(body), lines.At, "", 1)
		if withoutTime != last || time.Since(lastPosted) > 30*time.Second {
			token, _ := protocol.MintToken(secret, protocol.TokenClaims{Run: "gate-lines", Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Hour).Unix()})
			request, _ := http.NewRequestWithContext(runContext, http.MethodPost, strings.TrimSuffix(*wire, "/")+"/board/gate", strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			if response, err := client.Do(request); err != nil {
				fmt.Fprintf(stderr, "loom gate-lines: posting: %v\n", err)
			} else {
				if response.StatusCode != http.StatusOK {
					fmt.Fprintf(stderr, "loom gate-lines: posting: %s\n", response.Status)
				}
				response.Body.Close()
				last, lastPosted = withoutTime, time.Now()
			}
		}
		select {
		case <-runContext.Done():
		case <-time.After(max(0, *interval-time.Since(started))):
		}
	}
	return 0
}

// board prints the board's address with a fresh board token after the #, which a browser never sends.
func board(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("board", flag.ContinueOnError)
	flags.SetOutput(stderr)
	days := flags.Int("days", 30, "how long the token lasts")
	wire := flags.String("wire", "https://runs.loom.system.inc", "the wire's origin")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fmt.Fprint(stderr, usage)
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: protocol.BoardRun, Scope: protocol.ScopeBoard,
		Expires: time.Now().Add(time.Duration(*days) * 24 * time.Hour).Unix()})
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	origin := strings.TrimSuffix(*wire, "/")
	fmt.Fprintf(stdout, "%s/board#%s\n", origin, token)
	fmt.Fprintf(stdout, "snapshot: curl -s -H 'Authorization: Bearer %s' %s/board/snapshot\n", token, origin)
	return 0
}

// yieldLimit lets Loom use a box's slots only while the gate doesn't: a box's limit is Loom's allowance on it
// less the lines of the same class the gate's slot table holds for it. The gate's owner pulls lines out of
// the table to hand slots over and puts them back to take them, and Loom follows within seconds.
func yieldLimit(slots []coordinator.Machine, table string) func(string) int {
	allowance := map[string]map[string]int{}
	for _, machine := range slots {
		if ssh, ok := machine.(coordinator.SSHMachine); ok {
			if allowance[ssh.Box] == nil {
				allowance[ssh.Box] = map[string]int{}
			}
			allowance[ssh.Box][ssh.Class]++
		}
	}
	return func(box string) int {
		if allowance[box] == nil {
			// The gate's table names boxes; a machine it doesn't name, such as a pool, is never the gate's.
			return math.MaxInt
		}
		content, err := os.ReadFile(table)
		if err != nil {
			return 0 // can't read the gate's table: take nothing from it
		}
		// A box running a front-file tip, the star, is the star's alone (@system_adamic, Oct 8): Loom takes nothing there.
		if starRunsOn(filepath.Dir(table), box) {
			return 0
		}
		held := map[string]int{}
		for _, line := range strings.Split(string(content), "\n") {
			fields := strings.Fields(line)
			if len(fields) == 2 && fields[0] == box {
				held[fields[1]]++
			}
		}
		limit := 0
		for class, count := range allowance[box] {
			limit += max(0, count-held[class])
		}
		return limit
	}
}

// starRunsOn says whether box is the star's: the watcher's star-boxes file (one box per line, written every
// pass, naming a box the star runs on or has reserved) lists it, or a front-file tip runs there now.
func starRunsOn(state string, box string) bool {
	if content, err := os.ReadFile(filepath.Join(state, "star-boxes")); err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			if strings.TrimSpace(line) == box {
				return true
			}
		}
	}
	content, err := os.ReadFile(filepath.Join(state, "front"))
	if err != nil {
		return false
	}
	var globs []string
	for _, line := range strings.Split(string(content), "\n") {
		line, _, _ = strings.Cut(line, "#")
		if line = strings.TrimSpace(line); line != "" {
			globs = append(globs, line)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(state, "running"))
	for _, entry := range entries {
		running, err := os.ReadFile(filepath.Join(state, "running", entry.Name()))
		if err != nil {
			continue
		}
		fields := strings.Fields(string(running))
		if len(fields) < 4 || fields[3] != box {
			continue
		}
		for _, glob := range globs {
			if matched, _ := path.Match(glob, fields[0]); matched {
				return true
			}
		}
	}
	return false
}

// writeRecord keeps a run's record on this machine: a first line naming the run, then every event.
func writeRecord(path string, result coordinator.Result) error {
	var text strings.Builder
	header, _ := json.Marshal(map[string]any{"run": result.Run, "verdict": result.Verdict})
	text.Write(header)
	text.WriteByte('\n')
	for _, event := range result.Events {
		line, _ := json.Marshal(event)
		text.Write(line)
		text.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(text.String()), 0o644)
}

func readJob(path string) (protocol.Job, error) {
	var job protocol.Job
	file, err := os.Open(path)
	if err != nil {
		return job, err
	}
	defer file.Close()
	if err := protocol.Decode(file, &job); err != nil {
		return job, fmt.Errorf("job %s: %w", path, err)
	}
	return job, nil
}

func defaultSource() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Projects", "system", "loom")
}

// runnerVersion names the runner the source would build: its commit, and when the Go module has changes, a
// hash of them, so two different dirty trees never share a name on a box. Any change to the module, the
// coordinator's included, is a new version, so the staged-rollout law holds the coordinator to one box first
// just as it holds the runner.
func runnerVersion(source string) (string, error) {
	commit, err := exec.Command("git", "-C", source, "rev-parse", "--short=12", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("reading the runner's commit in %s: %w", source, err)
	}
	version := "git-" + strings.TrimSpace(string(commit))
	changes, err := exec.Command("git", "-C", source, "diff", "HEAD", "--", ".", ":(exclude)wire").Output()
	if err != nil {
		return "", err
	}
	if len(changes) > 0 {
		sum := sha256.Sum256(changes)
		version += "-dirty-" + hex.EncodeToString(sum[:])[:8]
	}
	return version, nil
}

// boxSlots reads the allowance, probes each box, and builds, checks and installs the runner on it.
func boxSlots(setupContext context.Context, path string, source string, version string, loomDirectory string, rollout *coordinator.Rollout, log io.Writer) ([]coordinator.Machine, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("%w (one \"box class\" per line; or pass --local)", err)
	}
	defer file.Close()
	ready := map[string]*coordinator.SSHMachine{}
	refused := map[string]bool{}
	var slots []coordinator.Machine
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") {
			continue
		}
		if len(fields) != 2 || (fields[1] != "B" && fields[1] != "S") {
			return nil, fmt.Errorf("%s: %q isn't \"box B\" or \"box S\"", path, scanner.Text())
		}
		box, class := fields[0], fields[1]
		if refused[box] {
			continue
		}
		machine := ready[box]
		if machine == nil {
			if ok, why := rollout.MayRun(version, box); !ok {
				fmt.Fprintf(log, "%s: left out, %s\n", box, why)
				refused[box] = true
				continue
			}
			platform, cores, err := coordinator.Probe(setupContext, box)
			if err != nil {
				return nil, err
			}
			binary, err := buildRunner(source, version, platform, loomDirectory)
			if err != nil {
				return nil, err
			}
			remote := ".loom/bin/loom-runner-" + version
			installed, err := coordinator.Install(setupContext, box, binary, remote)
			if err != nil {
				return nil, err
			}
			rollout.Installed(version, box)
			if installed {
				fmt.Fprintf(log, "%s: installed runner %s\n", box, version)
			}
			machine = &coordinator.SSHMachine{Box: box, Runner: remote, Version: version, GoPlatform: platform, CoreCount: cores}
			ready[box] = machine
		}
		slot := *machine
		slot.Class = class
		slots = append(slots, slot)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(slots) == 0 {
		return nil, fmt.Errorf("no slot in %s may run runner %s", path, version)
	}
	return slots, nil
}

// buildRunner builds a static runner for a platform once per version, into ~/.loom/build.
func buildRunner(source string, version string, platform string, loomDirectory string) (string, error) {
	system, architecture, _ := strings.Cut(platform, "/")
	binary := filepath.Join(loomDirectory, "build", "loom-runner-"+version+"-"+system+"-"+architecture)
	if _, err := os.Stat(binary); err == nil {
		return binary, nil
	}
	command := exec.Command("go", "build", "-trimpath", "-ldflags", "-X github.com/system-inc/loom/runner.Version="+version, "-o", binary, "./runner/cmd/loom-runner")
	command.Dir = source
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+system, "GOARCH="+architecture)
	if output, err := command.CombinedOutput(); err != nil {
		return "", fmt.Errorf("building the runner for %s: %v: %s", platform, err, output)
	}
	return binary, nil
}

// submitToken mints an owner's submit token: it submits that owner's changes to main through the wire and reads
// changes, and nothing else. The owner (the token's run claim) is a username, and the token expires after --days.
func submitToken(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("submit-token", flag.ContinueOnError)
	days := flags.Int("days", 7, "days until the token expires")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 1 || *days < 1 {
		fmt.Fprint(stderr, "usage: loom submit-token [--days N] <owner>\n")
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: flags.Arg(0), Scope: protocol.ScopeSubmit, Expires: time.Now().Add(time.Duration(*days) * 24 * time.Hour).Unix()})
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, token)
	return 0
}

// coordinatorToken mints a standing coordinator token for a long-lived service, like the planner's pull loop on the
// coordinator host: it reaches every coordinator seam (Queue's futures, plans and verdict index included). The name
// (the token's run claim) says which service holds it, and it expires after --days.
func coordinatorToken(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("coordinator-token", flag.ContinueOnError)
	days := flags.Int("days", 7, "days until the token expires")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 1 || *days < 1 {
		fmt.Fprint(stderr, "usage: loom coordinator-token <service> [--days N]\n")
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: flags.Arg(0), Scope: protocol.ScopeCoordinator, Expires: time.Now().Add(time.Duration(*days) * 24 * time.Hour).Unix()})
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, token)
	return 0
}

// publishToken mints a publish token: it writes the public store's blobs and refs through the wire and nothing
// else, so a gate box can share build products without any R2 key or a coordinator's reach. The name (the token's
// run claim) says whose it is, a box's name, and it expires after --days.
func publishToken(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("publish-token", flag.ContinueOnError)
	days := flags.Int("days", 30, "days until the token expires")
	candidate := flags.Bool("candidate", false, "a candidate's token: public blobs and refs/build-candidate only, never refs/build")
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 1 || *days < 1 {
		fmt.Fprint(stderr, "usage: loom publish-token <name> [--days N] [--candidate]\n")
		return 3
	}
	home, _ := os.UserHomeDir()
	secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret"))
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	scope := protocol.ScopePublish
	if *candidate {
		scope = protocol.ScopePublishCandidate
	}
	token, err := protocol.MintToken(secret, protocol.TokenClaims{Run: flags.Arg(0), Scope: scope, Expires: time.Now().Add(time.Duration(*days) * 24 * time.Hour).Unix()})
	if err != nil {
		fmt.Fprintf(stderr, "loom: %v\n", err)
		return 3
	}
	fmt.Fprintln(stdout, token)
	return 0
}

// platformOf is the platform a pool's workers run: what --pool-platform says, or Linux's.
func platformOf(platforms poolPlatformFlag, pool string) string {
	if platform := platforms[pool]; platform != "" {
		return platform
	}
	return poolPlatform
}
