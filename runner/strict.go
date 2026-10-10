package runner

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/protocol"
)

// prepareScript readies the checkout for a test job: the runner's own code, compiled in, never the server's.
//
//go:embed prepare.sh
var prepareScript []byte

// testOutputs are the only outputs a test unit may declare: what runTest writes.
var testOutputs = map[string]bool{"loom-out/test.jsonl.gz": true, "loom-out/cpu.tsv": true}

// wasiPattern is a -run pattern naming only TestWASI or its shards, which run with the WASI SDK's clang first.
var wasiPattern = regexp.MustCompile(`^\^\(TestWASI(Unit[0-9]+)?(\|TestWASI(Unit[0-9]+)?)*\)\$?$`)

// checkStrict is what a strict runner (Options.Strict, the Codex pool's) refuses before anything runs: any unit but a
// structured test job, so nothing the server sends is ever run as a command, and a test job that brings anything
// beyond its fields. CheckUnit has already checked the job's fields.
func checkStrict(unit protocol.Unit) error {
	if unit.Test == nil {
		return fmt.Errorf("refused: a strict runner runs only a structured test job, never argv or a shell string from the server")
	}
	if len(unit.Environment) > 0 {
		return fmt.Errorf("refused: a test job's environment is the runner's to make, not the server's")
	}
	if len(unit.Inputs) > 0 || unit.Directory != "" {
		return fmt.Errorf("refused: a test job takes no inputs or directory; the runner fetches its commit itself")
	}
	for _, output := range unit.Outputs {
		if !testOutputs[output.Glob] {
			return fmt.Errorf("refused: a test job's outputs are loom-out/test.jsonl.gz and loom-out/cpu.tsv, not %q", output.Glob)
		}
	}
	return nil
}

// goTestArguments is the go test command for one package, built here from the job's fields: each pattern is one
// argument (-run=<pattern>), never text a shell reads, and the package comes last, checked to be an import path.
func goTestArguments(testPackage protocol.TestPackage) []string {
	arguments := []string{"go", "test", "-count=1", "-json", "-timeout", "3h", "-run=" + testPackage.Run}
	if testPackage.Skip != "" {
		arguments = append(arguments, "-skip="+testPackage.Skip)
	}
	return append(arguments, testPackage.Package)
}

// goBuildArguments compiles the package's tests and runs none (-exec /bin/true), so cpu.tsv can say how much of the
// package's time was its build.
func goBuildArguments(testPackage protocol.TestPackage) []string {
	return []string{"go", "test", "-count=1", "-exec", "/bin/true", "-run=" + testPackage.Run, testPackage.Package}
}

// runTest runs a test job: prepare.sh fetches the commit from the public repository and readies the checkout, then each
// package's go test runs in it, as many at once as half the CPUs, each writing its go test -json lines to a part file.
// The parts become loom-out/test.jsonl.gz, their CPU seconds loom-out/cpu.tsv. Failed: a package's go test failed or
// the unit ran out of time. Broken: the job was refused, the instance couldn't be readied, or its disk filled.
func (run *unitRun) runTest(runContext context.Context) string {
	job := run.unit.Test
	if job.Phase != "" && !run.options.PhaseJobs {
		run.fail(protocol.PhaseStart, fmt.Errorf("refused: a phase job runs only on a runner started with --phase-jobs; this one runs only go test"))
		return protocol.StatusBroken
	}
	started := time.Now()
	deadline := started.Add(time.Duration(run.unit.TimeoutSeconds) * time.Second)
	root, tree := run.options.Root, run.options.Tree
	switch {
	case root != "":
	case run.options.Strict:
		root = "/tmp"
	default:
		root = filepath.Join(run.options.WorkspaceParent, "loom-test-root")
	}
	if tree == "" {
		// /tmp/adamic for a strict runner: the checkout the instance's opening clones, kept across units.
		tree = filepath.Join(root, "adamic")
	}
	script := filepath.Join(run.directory, "prepare.sh")
	environmentFile := filepath.Join(run.directory, "environment")
	if err := os.WriteFile(script, prepareScript, 0o700); err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	// Only a strict runner's instance runs one unit at a time, so only there are earlier units' leavings no one's.
	trim := "keep"
	if run.options.Strict {
		trim = "trim"
	}
	prepared, _, _, err := run.stream(runContext, []string{"bash", script, tree, job.Sha, job.Base, job.GateInputs, environmentFile, trim, root}, run.environment(), run.workspace, time.Until(deadline))
	switch {
	case err != nil:
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	case prepared.interrupted:
		run.fail(protocol.PhaseRun, fmt.Errorf("the runner was stopped while preparing the checkout"))
		return protocol.StatusBroken
	case prepared.code != nil && *prepared.code == 3:
		run.fail(protocol.PhaseStart, fmt.Errorf("refused: the test job's commit isn't what the public repository holds (prepare.sh's output says why)"))
		return protocol.StatusBroken
	case prepared.timedOut || prepared.code == nil || *prepared.code != 0:
		run.fail(protocol.PhaseStart, fmt.Errorf("preparing the checkout failed (%s): the instance's, never the change's", describeOutcome(prepared)))
		return protocol.StatusBroken
	}
	environment, err := run.testEnvironment(environmentFile, job)
	if err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	// A WASI spec on an instance without the WASI SDK's builtins would run every shard on the native clang, where each
	// skips and the unit passes; the instance can't run it, so the unit is broken and placed again.
	for _, testPackage := range job.Packages {
		if wasiPattern.MatchString(testPackage.Run) {
			if err := wasiReady(environment); err != nil {
				run.fail(protocol.PhaseStart, err)
				return protocol.StatusBroken
			}
			break
		}
	}
	out := filepath.Join(run.workspace, "loom-out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	if job.Phase != "" {
		return run.runPhase(runContext, job, environment, tree, root, out, deadline)
	}

	testContext, cancel := context.WithDeadline(runContext, deadline)
	defer cancel()
	results := make([]packageResult, len(job.Packages))
	// The packages' go test runs outside run.stream, so nothing would speak for a live unit while a package compiles
	// and tests in silence, and the coordinator dropped it after three missed heartbeats (Oct 10: every strict unit over
	// 360 s, ec123b7f three times). This ticker says the unit is still running for as long as its packages do.
	quiet := make(chan struct{})
	defer close(quiet)
	go func() {
		ticker := time.NewTicker(run.options.Heartbeat / 4)
		defer ticker.Stop()
		for {
			select {
			case <-quiet:
				return
			case <-ticker.C:
				if run.emitter.silentFor() >= run.options.Heartbeat {
					run.say(fmt.Sprintf("still running after %.0f s", time.Since(started).Seconds()))
				}
			}
		}
	}()
	slots := make(chan struct{}, max(1, runtime.NumCPU()/2))
	var group sync.WaitGroup
	for index, testPackage := range job.Packages {
		group.Add(1)
		go func() {
			defer group.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[index] = run.runPackage(testContext, testPackage, packageEnvironment(environment, testPackage), tree, filepath.Join(out, fmt.Sprintf("part-%d", index)))
		}()
	}
	group.Wait()

	status, code := protocol.StatusPassed, 0
	var userSeconds, systemSeconds float64
	var cpu bytes.Buffer
	for index, result := range results {
		testPackage := job.Packages[index]
		userSeconds += result.userSeconds
		systemSeconds += result.systemSeconds
		fmt.Fprintf(&cpu, "%s\t%.2f\t%.2f\n", testPackage.Package, result.buildSeconds, result.testSeconds)
		if result.err != nil {
			run.fail(protocol.PhaseRun, fmt.Errorf("%s: %w", testPackage.Package, result.err))
			status = worse(status, protocol.StatusBroken)
			continue
		}
		if result.code != 0 {
			code = 1
			status = worse(status, protocol.StatusFailed)
			run.say(fmt.Sprintf("%s exited %d", testPackage.Package, result.code))
			for _, line := range lastLines(filepath.Join(out, fmt.Sprintf("part-%d.stderr", index)), 5) {
				run.say("  " + line)
			}
		}
	}
	timedOut := runContext.Err() == nil && testContext.Err() != nil
	if runContext.Err() != nil {
		run.fail(protocol.PhaseRun, fmt.Errorf("the runner was stopped before the tests finished"))
		status = protocol.StatusBroken
	}
	for _, failed := range failedTests(out, len(job.Packages)) {
		run.say("failed " + failed)
	}
	if err := gatherParts(out, len(job.Packages)); err != nil {
		run.fail(protocol.PhaseRun, err)
		status = protocol.StatusBroken
	}
	if err := os.WriteFile(filepath.Join(out, "cpu.tsv"), cpu.Bytes(), 0o644); err != nil {
		run.fail(protocol.PhaseRun, err)
		status = protocol.StatusBroken
	}
	// A test that ran out of disk proved nothing about the change: broken, never failed.
	if status == protocol.StatusFailed && diskFilled(out, len(job.Packages)) {
		run.fail(protocol.PhaseRun, fmt.Errorf("the instance's disk filled during the tests (no space left on device): the instance's, not the change's"))
		status = protocol.StatusBroken
	}
	run.emitter.emit(protocol.Event{Type: "exit", Code: &code, TimedOut: timedOut, WallSeconds: seconds(time.Since(started)),
		UserSeconds: seconds(time.Duration(userSeconds * float64(time.Second))), SystemSeconds: seconds(time.Duration(systemSeconds * float64(time.Second)))})
	if timedOut {
		status = worse(status, protocol.StatusFailed)
	}
	return status
}

// runPhase runs a phase job: the box fast gate's run.py from the gate tools at job.Tools, checked out beside the tree
// from the same public repository, built here as separate arguments (--phase, and --unit when the line names one),
// never shell text. Its output streams as the unit's, under run.stream's heartbeat, and its exit decides the unit, as
// the box decides a stage (Judge's phase rule). Failed: run.py exited non-zero or ran out of the unit's time. Broken:
// the tools couldn't be readied or the runner was stopped.
func (run *unitRun) runPhase(runContext context.Context, job *protocol.TestJob, environment map[string]string, tree string, root string, out string, deadline time.Time) string {
	tools := filepath.Join(root, "adamic-gate-tools", job.Tools)
	if err := readyTools(runContext, tree, tools, job.Tools, packageEnvironment(environment, protocol.TestPackage{})); err != nil {
		run.fail(protocol.PhaseStart, fmt.Errorf("readying the gate tools at %s: %w (the instance's, never the change's)", job.Tools, err))
		return protocol.StatusBroken
	}
	fields := strings.Fields(job.Phase)
	argv := []string{"python3", filepath.Join(tools, "cloud", "fast-gate", "run.py"), "--phase", fields[0]}
	if len(fields) == 2 {
		argv = append(argv, "--unit", fields[1])
	}
	argv = append(argv, "--tree", tree, "--sha", job.Sha, "--base", job.Base, "--tools", tools, "--out", filepath.Join(out, "phase"))
	outcome, state, wall, err := run.stream(runContext, argv, packageEnvironment(environment, protocol.TestPackage{}), tree, time.Until(deadline))
	if err != nil {
		run.fail(protocol.PhaseStart, err)
		return protocol.StatusBroken
	}
	run.emitExit(outcome, state, wall)
	switch {
	case outcome.interrupted:
		run.fail(protocol.PhaseRun, fmt.Errorf("the runner was stopped before the phase finished"))
		return protocol.StatusBroken
	case outcome.code != nil && *outcome.code == 0 && !outcome.timedOut:
		return protocol.StatusPassed
	default:
		run.say(fmt.Sprintf("phase %s: %s", job.Phase, describeOutcome(outcome)))
		return protocol.StatusFailed
	}
}

// toolsRepository is where a phase job's gate tools are fetched from: the public repository (a test points it at a
// local one).
var toolsRepository = protocol.AdamicRepository

// readyTools checks out the gate tools' commit as a detached worktree of the tree's repository, fetched from the
// public repository by sha, and keeps it for later units of the same commit; a checkout at any other commit is redone.
func readyTools(runContext context.Context, tree string, tools string, commit string, environment []string) error {
	git := func(directory string, arguments ...string) (string, error) {
		command := exec.CommandContext(runContext, "git", append([]string{"-C", directory}, arguments...)...)
		command.Env = append(environment, "GIT_TERMINAL_PROMPT=0")
		output, err := command.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
		}
		return strings.TrimSpace(string(output)), nil
	}
	if head, err := git(tools, "rev-parse", "HEAD"); err == nil && head == commit {
		return nil
	}
	if err := os.RemoveAll(tools); err != nil {
		return err
	}
	git(tree, "worktree", "prune")
	if _, err := git(tree, "fetch", "--no-tags", "-q", toolsRepository, commit); err != nil {
		return err
	}
	if _, err := git(tree, "worktree", "add", "--detach", "--force", tools, commit); err != nil {
		return err
	}
	if head, err := git(tools, "rev-parse", "HEAD"); err != nil || head != commit {
		return fmt.Errorf("the checkout is at %q, not %s", head, commit)
	}
	return nil
}

// say emits one line on the runner's own stream.
func (run *unitRun) say(text string) {
	run.emitter.emit(protocol.Event{Type: "output", Stream: "runner", Text: "loom-runner: " + text})
}

func describeOutcome(outcome exitOutcome) string {
	switch {
	case outcome.timedOut:
		return "out of the unit's time"
	case outcome.code != nil:
		return fmt.Sprintf("exit %d", *outcome.code)
	default:
		return outcome.signal
	}
}

// testEnvironment is what every package's go test runs in: prepare.sh's environment (adamic's own env.sh and the gate
// inputs' variables on the runner's base), and the gate's switches, which the job's fields set and nothing else does.
func (run *unitRun) testEnvironment(environmentFile string, job *protocol.TestJob) (map[string]string, error) {
	content, err := os.ReadFile(environmentFile)
	if err != nil {
		return nil, fmt.Errorf("prepare.sh left no environment: %w", err)
	}
	environment := map[string]string{}
	for _, variable := range bytes.Split(content, []byte{0}) {
		if name, value, found := strings.Cut(string(variable), "="); found && name != "" {
			environment[name] = value
		}
	}
	for name, value := range map[string]string{"ADAMIC_GATE_UNCACHED": "1", "ADAMIC_TEST_WASI": "1", "ADAMIC_ORACLE_WASI": "1", "ADAMIC_GATE_COHERE": "1"} {
		environment[name] = value
	}
	if job.Sample != "" {
		environment["ADAMIC_GATE_SAMPLE"] = job.Sample
	}
	// Go's build cache is the unit's own, empty when it starts and gone with its directory, and no GOCACHEPROG serves
	// it. Go doesn't hash a header reached through #cgo -I outside the package, so a cache that outlives a unit (the
	// instance's ~/.cache/go-build, Oct 10: internal/buildcache read a stale header on two warm Codex instances) can
	// test an object built from the old header: a change to that header would read green. Slower, never looser (Loom,
	// 01:42Z), until the key covers out-of-package cgo inputs.
	goCache := filepath.Join(run.directory, "gocache")
	if err := os.MkdirAll(goCache, 0o755); err != nil {
		return nil, err
	}
	environment["GOCACHE"] = goCache
	delete(environment, "GOCACHEPROG")
	// adamic's own build cache likewise: its key misses a header reached by a relative #include, hand-rolled products'
	// undeclared files and headers outside the repository (Builder, Oct 10 02:27Z), so a warm one can serve a stale
	// product. Each unit's starts empty, to be filled only from the action store (Release's ruling).
	adamicCache := filepath.Join(run.directory, "adamic-build")
	if err := os.MkdirAll(adamicCache, 0o755); err != nil {
		return nil, err
	}
	environment["ADAMIC_BUILD_CACHE_DIR"] = adamicCache
	if len(job.ChangedPaths) > 0 {
		changed := filepath.Join(run.directory, "changed-paths.txt")
		if err := os.WriteFile(changed, []byte(strings.Join(job.ChangedPaths, "\n")+"\n"), 0o644); err != nil {
			return nil, err
		}
		environment["ADAMIC_GATE_CHANGED"] = changed
	}
	return environment, nil
}

// packageEnvironment is the environment one package's go test runs in, as KEY=value lines: a TestWASI spec puts the
// WASI SDK's clang first on PATH, as the gate's wasi phase does; every other test keeps the native clang.
func packageEnvironment(environment map[string]string, testPackage protocol.TestPackage) []string {
	result := make([]string, 0, len(environment))
	for name, value := range environment {
		if name == "PATH" && wasiPattern.MatchString(testPackage.Run) && environment["WASI_SYSROOT"] != "" {
			value = strings.TrimSuffix(environment["WASI_SYSROOT"], "/share/wasi-sysroot") + "/bin:" + value
		}
		result = append(result, name+"="+value)
	}
	return result
}

// wasiReady checks that the WASI SDK's clang names a compiler-rt builtins library for wasm32 that exists.
func wasiReady(environment map[string]string) error {
	sysroot := environment["WASI_SYSROOT"]
	if sysroot == "" {
		return fmt.Errorf("a WASI spec on an instance without the WASI SDK (WASI_SYSROOT unset): the instance's, not the change's")
	}
	clang := strings.TrimSuffix(sysroot, "/share/wasi-sysroot") + "/bin/clang"
	named, err := exec.Command(clang, "--target=wasm32-unknown-wasi", "-rtlib=compiler-rt", "-print-libgcc-file-name").Output()
	builtins := strings.TrimSpace(string(named))
	if _, statError := os.Stat(builtins); err != nil || builtins == "" || statError != nil {
		return fmt.Errorf("a WASI spec on an instance without the WASI SDK's builtins (%s names %q): the instance's, not the change's", clang, builtins)
	}
	return nil
}

// A packageResult is how one package's go test ended.
type packageResult struct {
	code                       int
	buildSeconds, testSeconds  float64
	userSeconds, systemSeconds float64
	err                        error // the runner couldn't run it at all
}

// runPackage compiles the package's tests, then runs them, go test's -json lines to <part>.jsonl and its stderr to
// <part>.stderr. Past the context's deadline the process group gets SIGTERM, then SIGKILL after KillGrace.
func (run *unitRun) runPackage(testContext context.Context, testPackage protocol.TestPackage, environment []string, tree string, part string) packageResult {
	var result packageResult
	build, err := run.goCommand(testContext, goBuildArguments(testPackage), environment, tree, io.Discard, io.Discard)
	if err != nil {
		result.err = err
		return result
	}
	result.buildSeconds = build.UserTime().Seconds() + build.SystemTime().Seconds()
	stdout, err := os.Create(part + ".jsonl")
	if err != nil {
		result.err = err
		return result
	}
	defer stdout.Close()
	stderr, err := os.Create(part + ".stderr")
	if err != nil {
		result.err = err
		return result
	}
	defer stderr.Close()
	tested, err := run.goCommand(testContext, goTestArguments(testPackage), environment, tree, stdout, stderr)
	if err != nil {
		result.err = err
		return result
	}
	result.testSeconds = tested.UserTime().Seconds() + tested.SystemTime().Seconds()
	result.userSeconds = build.UserTime().Seconds() + tested.UserTime().Seconds()
	result.systemSeconds = build.SystemTime().Seconds() + tested.SystemTime().Seconds()
	result.code = tested.ExitCode()
	if result.code < 0 {
		result.code = 1 // killed by a signal: at the deadline, or by the runner's stop
	}
	return result
}

// goCommand runs argv (go, from the environment's PATH) in its own process group in directory and waits for it.
func (run *unitRun) goCommand(commandContext context.Context, argv []string, environment []string, directory string, stdout io.Writer, stderr io.Writer) (*os.ProcessState, error) {
	path := ""
	for _, variable := range environment {
		if value, found := strings.CutPrefix(variable, "PATH="); found {
			path = value
		}
	}
	executable, err := lookPath(argv[0], path)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(commandContext, executable, argv[1:]...)
	command.Args[0] = argv[0]
	command.Env, command.Dir, command.Stdout, command.Stderr = environment, directory, stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGTERM) }
	command.WaitDelay = run.options.KillGrace
	if err := command.Start(); err != nil {
		return nil, err
	}
	command.Wait()
	signalGroup(command.Process.Pid, syscall.SIGKILL)
	return command.ProcessState, nil
}

// gatherParts writes every part's go test lines, in package order, to test.jsonl.gz.
func gatherParts(out string, parts int) error {
	file, err := os.Create(filepath.Join(out, "test.jsonl.gz"))
	if err != nil {
		return err
	}
	defer file.Close()
	compressed, _ := gzip.NewWriterLevel(file, gzip.BestCompression)
	for index := range parts {
		part, err := os.Open(filepath.Join(out, fmt.Sprintf("part-%d.jsonl", index)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		_, err = io.Copy(compressed, part)
		part.Close()
		if err != nil {
			return err
		}
	}
	return compressed.Close()
}

// failedTests names each top-level test the parts saw fail, as "<package> <test>".
func failedTests(out string, parts int) []string {
	var failed []string
	seen := map[string]bool{}
	for index := range parts {
		file, err := os.Open(filepath.Join(out, fmt.Sprintf("part-%d.jsonl", index)))
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1<<20), 16<<20)
		for scanner.Scan() {
			var event struct{ Action, Package, Test string }
			if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Action == "fail" && event.Test != "" && !strings.Contains(event.Test, "/") {
				if name := event.Package + " " + event.Test; !seen[name] {
					seen[name] = true
					failed = append(failed, name)
				}
			}
		}
		file.Close()
	}
	return failed
}

// diskFilled says whether a package ran out of disk: its output says so, or the disk is still nearly full.
func diskFilled(out string, parts int) bool {
	for index := range parts {
		for _, suffix := range []string{".jsonl", ".stderr"} {
			if content, err := os.ReadFile(filepath.Join(out, fmt.Sprintf("part-%d%s", index, suffix))); err == nil && bytes.Contains(content, []byte("no space left on device")) {
				return true
			}
		}
	}
	var stat syscall.Statfs_t
	return syscall.Statfs(out, &stat) == nil && stat.Bavail*uint64(stat.Bsize) < 512<<20
}

// lastLines is a file's last n lines.
func lastLines(path string, n int) []string {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(content), "\n"), "\n")
	return lines[max(0, len(lines)-n):]
}
