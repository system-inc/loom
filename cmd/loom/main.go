// Command loom is the coordinator: it runs a job file on Loom's slots and prints the verdict. It exits 0 when
// the run is green, 1 when red, 2 when void, and 3 when the run couldn't be set up.
//
//	loom run [--uncached] [--local <slots> | --slots <file>] [--pool <name>=<slots>]... [--yield-to <gate slots>] [--record <file>] [--wire <url>] <job.json>
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
// doesn't install its runner, and a green there unlocks no box.
package main

import (
	"bufio"
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
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/coordinator"
	"github.com/system-inc/loom/gatelines"
	"github.com/system-inc/loom/protocol"
)

const usage = `usage:
  loom run [--uncached] [--local <slots> | --slots <file>] [--pool <name>=<slots>]... [--yield-to <gate slots>] [--record <file>] [--wire <url>] <job.json>
  loom board [--days <n>] [--wire <url>]
  loom pool status [--wire <url>] <name>
  loom pool token <pool> [--hours N]
  loom pool publish-runner
  loom pool prompt <pool> --runner <sha256> [--until 55m]
  loom gate-lines [--once] [--interval <duration>] [--wire <url>]
  loom top [--once] [--wire <url>]
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) > 0 && arguments[0] == "board" {
		return board(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "top" {
		return top(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "gate-lines" {
		return gateLines(arguments[1:], stdout, stderr)
	}
	if len(arguments) > 0 && arguments[0] == "pool" {
		return pool(arguments[1:], stdout, stderr)
	}
	if len(arguments) == 0 || arguments[0] != "run" {
		fmt.Fprint(stderr, usage)
		return 3
	}
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(stderr)
	uncached := flags.Bool("uncached", false, "read and write no cache: every unit runs (what lands main)")
	local := flags.Int("local", 0, "run on this machine with this many slots instead of ~/.loom/slots")
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
	source := flags.String("source", defaultSource(), "this repository's checkout, to build the runner from")
	slotsPath := flags.String("slots", "", "the slot allowance, one \"box class\" per line (default ~/.loom/slots)")
	recordPath := flags.String("record", "", "write the run's record, every event as a JSON line, to this file")
	yieldTo := flags.String("yield-to", "", "the gate's slot table: Loom uses a box's slots only while the gate's table doesn't hold them")
	var pools poolSlotsFlag
	flags.Var(&pools, "pool", "also place units on a pool on the wire, <name>=<slots>; repeatable")
	if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 1 {
		fmt.Fprint(stderr, usage)
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
	if len(pools) > 0 {
		// The pool's workers are built from this source, the version every pool unit's cache key names.
		poolVersion, err := runnerVersion(*source)
		if err != nil {
			return fail(err)
		}
		for _, wanted := range pools {
			machine := &coordinator.PoolMachine{Pool: wanted.name, Wire: *wire, Secret: secret, Version: poolVersion, GoPlatform: poolPlatform}
			poolMachines[machine.Name()] = true
			for range wanted.slots {
				slots = append(slots, machine)
			}
		}
	}
	config := coordinator.Config{Wire: *wire, Secret: secret, Slots: slots, Uncached: *uncached, Durations: durations, Log: stdout}
	if *yieldTo != "" {
		config.SlotLimit = yieldLimit(slots, *yieldTo)
	}
	result, err := coordinator.Run(runContext, config, job)
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
func gateLines(arguments []string, stdout io.Writer, stderr io.Writer) int {
	flags := flag.NewFlagSet("gate-lines", flag.ContinueOnError)
	flags.SetOutput(stderr)
	once := flags.Bool("once", false, "print one reading as JSON and post nothing")
	interval := flags.Duration("interval", 3*time.Second, "how often to read and post")
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
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
	wire := flags.String("wire", "https://loom-wire.kirk-ouimet.workers.dev", "the wire's origin")
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
