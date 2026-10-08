// Command loom is the coordinator: it runs a job file on Loom's slots and prints the verdict. It exits 0 when
// the run is green, 1 when red, 2 when void, and 3 when the run couldn't be set up.
//
//	loom run [--uncached] [--local <slots> | --slots <file>] [--wire <url>] <job.json>
//
// The slots come from ~/.loom/slots, one "box class" per line (class B is a box's area slot, S a small one),
// unless --local runs every unit on this machine. The token secret is ~/.loom/token-secret. For each box
// the runner is built from this repository's source for the box's platform, named for its version, and
// installed under the staged-rollout law (~/.loom/rollout.tsv): a version, which is the whole Go module's
// commit, so a new coordinator too, reaches a second box only after a green run on its first.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/system-inc/loom/coordinator"
	"github.com/system-inc/loom/protocol"
)

const usage = `usage:
  loom run [--uncached] [--local <slots> | --slots <file>] [--wire <url>] <job.json>
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer) int {
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
		slots, err = boxSlots(runContext, *slotsPath, *source, version, loomDirectory, rollout, stdout)
		if err != nil {
			return fail(err)
		}
	}
	result, err := coordinator.Run(runContext, coordinator.Config{
		Wire: *wire, Secret: secret, Slots: slots, Uncached: *uncached, Durations: durations, Log: stdout,
	}, job)
	if err != nil {
		return fail(err)
	}
	switch result.Verdict.Status {
	case "green":
		if version != "" {
			for _, box := range result.Machines {
				rollout.Green(version, box)
			}
		}
		return 0
	case "red":
		return 1
	default:
		return 2
	}
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
			platform, err := coordinator.Probe(setupContext, box)
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
			machine = &coordinator.SSHMachine{Box: box, Runner: remote, Version: version, GoPlatform: platform}
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
