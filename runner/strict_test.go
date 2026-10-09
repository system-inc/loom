package runner

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// The strict runner's refusals (#1pe3ndh). Each mutant below must make a test here fail:
//
//	checkStrict's argv refusal dropped (`if unit.Test == nil` -> `if false`): TestAStrictRunnerRefusesArgv
//	CheckTestJob's repository check dropped: TestABadTestJobIsRefusedBeforeAnythingRuns
//	goTestArguments joining the pattern into a shell line: TestShellTextInAPatternStaysOneArgument
//	prepare.sh fetching the commit into the checkout, not an empty store (a planted local commit then passes): TestPrepareRefusesACommitGitHubDoesntHave
//	a trim reaching the host's /tmp (as a mutant, only /tmp/go-buildloom-canary-*): TestPrepareNamesNoHostPath, TestPrepareTouchesNothingOutsideItsRoot
//	a strict runner's root anything but /tmp: TestAStrictRunnersRootIsTmpAndOthersKeepTheirOwn

const testSha = "0123456789abcdef0123456789abcdef01234567"

// strictFixture stands in for the instance: prepare.sh replaced by a stub that writes an environment whose PATH
// leads to a stub go, which records each invocation's argv (NUL separated) and prints a passing or failing test.
type strictFixture struct {
	directory string // the stubs, and markers a refused unit must never create
	tree      string
	calls     string
}

func newStrictFixture(t *testing.T, prepareExit int) *strictFixture {
	t.Helper()
	fixture := &strictFixture{directory: t.TempDir()}
	fixture.tree = filepath.Join(fixture.directory, "tree")
	fixture.calls = filepath.Join(fixture.directory, "calls")
	bin := filepath.Join(fixture.directory, "bin")
	os.MkdirAll(bin, 0o755)
	os.MkdirAll(fixture.calls, 0o755)
	stubGo := `#!/bin/bash
printf '%s\0' "$0" "$@" > "` + fixture.calls + `/call-$$"
case "$*" in *-exec*) exit 0 ;; esac
for argument in "$@"; do case "${argument}" in -run=*Fail*) printf '{"Action":"fail","Package":"%s","Test":"TestFail"}\n' "${!#}"; exit 1 ;; esac; done
printf '{"Action":"pass","Package":"%s","Test":"TestA"}\n' "${!#}"
`
	os.WriteFile(filepath.Join(bin, "go"), []byte(stubGo), 0o755)
	original := prepareScript
	t.Cleanup(func() { prepareScript = original })
	prepareScript = []byte(`#!/bin/bash
touch "` + fixture.directory + `/prepared"
echo "$6" > "` + fixture.directory + `/trim"
echo "$7" > "` + fixture.directory + `/root"
env > "` + fixture.directory + `/prepare-environment"
[ ` + strconv.Itoa(prepareExit) + ` = 0 ] || exit ` + strconv.Itoa(prepareExit) + `
mkdir -p "$1"
printf 'PATH=%s\0HOME=%s\0' "` + bin + `:/usr/bin:/bin" "${HOME}" > "$5"
`)
	return fixture
}

func (fixture *strictFixture) exists(name string) bool {
	_, err := os.Stat(filepath.Join(fixture.directory, name))
	return err == nil
}

// goCalls is every argv the stub go was run with.
func (fixture *strictFixture) goCalls(t *testing.T) [][]string {
	t.Helper()
	entries, _ := os.ReadDir(fixture.calls)
	var calls [][]string
	for _, entry := range entries {
		content, _ := os.ReadFile(filepath.Join(fixture.calls, entry.Name()))
		fields := strings.Split(strings.TrimSuffix(string(content), "\x00"), "\x00")
		fields[0] = filepath.Base(fields[0])
		calls = append(calls, fields)
	}
	return calls
}

func testJobUnit(job protocol.TestJob) protocol.Unit {
	return protocol.Unit{Run: "r-test", Unit: "unit", Test: &job, TimeoutSeconds: 60, Token: testToken}
}

func goodTestJob() protocol.TestJob {
	return protocol.TestJob{Repository: protocol.AdamicRepository, Sha: testSha,
		Packages: []protocol.TestPackage{{Package: protocol.AdamicModule + "/internal/lower", Run: "^(TestA)$"}}}
}

func (fixture *strictFixture) options(t *testing.T) Options {
	options := testOptions(t)
	options.Strict = true
	options.Root = filepath.Join(fixture.directory, "root")
	options.Tree = fixture.tree
	return options
}

func TestAStrictRunnerRefusesArgv(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	marker := filepath.Join(fixture.directory, "ran")
	for name, argv := range map[string][]string{
		"a command":      {"touch", marker},
		"a shell string": {"bash", "-c", "touch " + marker},
	} {
		result, events, _ := runUnit(t, testUnit(argv...), fixture.options(t))
		if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "refused: a strict runner runs only a structured test job") {
			t.Errorf("%s: %s, errors %q", name, result.Status, errorPhases(events))
		}
		if fixture.exists("ran") || fixture.exists("prepared") || len(eventsOfType(events, "exit")) > 0 {
			t.Errorf("%s: something ran before the refusal", name)
		}
	}
	// The same unit runs on a runner that isn't strict, so the refusal is strict mode's alone.
	result, _, _ := runUnit(t, testUnit("touch", marker), testOptions(t))
	if result.Status != protocol.StatusPassed || !fixture.exists("ran") {
		t.Errorf("a box runner refused argv: %s", result.Status)
	}
}

func TestAStrictRunnerRefusesWhatATestJobDoesntTake(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	for name, change := range map[string]func(unit *protocol.Unit){
		"an environment": func(unit *protocol.Unit) { unit.Environment = map[string]string{"LD_PRELOAD": "/tmp/x.so"} },
		"an input": func(unit *protocol.Unit) {
			unit.Inputs = []protocol.Input{{Path: "x", Sha256: strings.Repeat("a", 64)}}
			unit.Store = &protocol.Endpoint{Url: "https://store"}
		},
		"a directory":    func(unit *protocol.Unit) { unit.Directory = "sub" },
		"another output": func(unit *protocol.Unit) { unit.Outputs = []protocol.Output{{Glob: "../../*"}} },
	} {
		unit := testJobUnit(goodTestJob())
		change(&unit)
		result, events, _ := runUnit(t, unit, fixture.options(t))
		if result.Status != protocol.StatusBroken || errorPhases(events) == "" || fixture.exists("prepared") {
			t.Errorf("%s: %s, errors %q, prepared %v", name, result.Status, errorPhases(events), fixture.exists("prepared"))
		}
	}
}

func TestABadTestJobIsRefusedBeforeAnythingRuns(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	for name, change := range map[string]func(job *protocol.TestJob){
		"a private repository":           func(job *protocol.TestJob) { job.Repository = "https://github.com/system-inc/private" },
		"another repository":             func(job *protocol.TestJob) { job.Repository = "https://github.com/attacker/adamic" },
		"a malformed sha":                func(job *protocol.TestJob) { job.Sha = "main; touch " + fixture.directory + "/ran" },
		"a package holding a command":    func(job *protocol.TestJob) { job.Packages[0].Package = protocol.AdamicModule + "/$(touch ran)" },
		"a package holding a backtick":   func(job *protocol.TestJob) { job.Packages[0].Package = protocol.AdamicModule + "/`touch ran`" },
		"a pattern holding a line break": func(job *protocol.TestJob) { job.Packages[0].Run = "x\ntouch ran" },
	} {
		job := goodTestJob()
		job.Packages = append([]protocol.TestPackage(nil), job.Packages...)
		change(&job)
		result, events, _ := runUnit(t, testJobUnit(job), fixture.options(t))
		if result.Status != protocol.StatusBroken || errorPhases(events) == "" {
			t.Errorf("%s: %s, errors %q", name, result.Status, errorPhases(events))
		}
		if fixture.exists("prepared") || fixture.exists("ran") || len(fixture.goCalls(t)) > 0 {
			t.Errorf("%s: something ran before the refusal", name)
		}
	}
}

func TestShellTextInAPatternStaysOneArgument(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	marker := filepath.Join(fixture.directory, "ran")
	run := "x; touch " + marker + " $(touch " + marker + ") `touch " + marker + "`"
	skip := "' ; touch " + marker + " #"
	job := goodTestJob()
	job.Packages[0].Run, job.Packages[0].Skip = run, skip
	result, events, _ := runUnit(t, testJobUnit(job), fixture.options(t))
	if result.Status != protocol.StatusPassed {
		t.Fatalf("%s, errors %q", result.Status, errorPhases(events))
	}
	if fixture.exists("ran") {
		t.Fatalf("a pattern ran as a command")
	}
	want := []string{"go", "test", "-count=1", "-json", "-timeout", "3h", "-run=" + run, "-skip=" + skip, protocol.AdamicModule + "/internal/lower"}
	found := false
	for _, call := range fixture.goCalls(t) {
		if reflect.DeepEqual(call, want) {
			found = true
		}
	}
	if !found {
		t.Fatalf("go test wasn't run as %q; calls %q", want, fixture.goCalls(t))
	}
}

func TestATestJobRunsItsPackagesAndUploadsTheirLines(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	store := newTestStore(t)
	job := goodTestJob()
	job.Packages = append(job.Packages, protocol.TestPackage{Package: protocol.AdamicModule + "/internal/native", Run: "^(TestFail)$"})
	job.ChangedPaths = []string{"internal/lower/lower.go"}
	unit := testJobUnit(job)
	unit.Outputs = []protocol.Output{{Glob: "loom-out/test.jsonl.gz"}, {Glob: "loom-out/cpu.tsv"}}
	unit.Store = &protocol.Endpoint{Url: store.server.URL}
	result, events, _ := runUnit(t, unit, fixture.options(t))
	if result.Status != protocol.StatusFailed {
		t.Fatalf("a failing package left the unit %s; errors %q", result.Status, errorPhases(events))
	}
	if lines := strings.Join(outputLines(events, "runner"), "\n"); !strings.Contains(lines, "failed "+protocol.AdamicModule+"/internal/native TestFail") {
		t.Errorf("the failed test isn't named: %s", lines)
	}
	exits := eventsOfType(events, "exit")
	if len(exits) != 1 || exits[0].Code == nil || *exits[0].Code != 1 {
		t.Errorf("one exit event with code 1, got %+v", exits)
	}
	uploaded := map[string]string{}
	for _, event := range eventsOfType(events, "uploaded") {
		uploaded[event.Path] = event.Sha256
	}
	reader, err := gzip.NewReader(bytes.NewReader(store.blobs[uploaded["loom-out/test.jsonl.gz"]]))
	if err != nil {
		t.Fatalf("test.jsonl.gz: %v (uploaded %v)", err, uploaded)
	}
	lines, _ := io.ReadAll(reader)
	if !bytes.Contains(lines, []byte(`"Test":"TestA"`)) || !bytes.Contains(lines, []byte(`"Test":"TestFail"`)) {
		t.Errorf("test.jsonl.gz lacks a package's lines: %s", lines)
	}
	if cpu := string(store.blobs[uploaded["loom-out/cpu.tsv"]]); strings.Count(cpu, "\n") != 2 || !strings.HasPrefix(cpu, protocol.AdamicModule+"/internal/lower\t") {
		t.Errorf("cpu.tsv: %q", cpu)
	}
	// Strict: the instance runs one unit at a time, so the preparation trims what earlier units left.
	if trim, _ := os.ReadFile(filepath.Join(fixture.directory, "trim")); string(trim) != "trim\n" {
		t.Errorf("a strict runner prepared with %q, not trim", trim)
	}
	// The preparation got the runner's base environment and nothing of the unit's: no token reaches it.
	environment, _ := os.ReadFile(filepath.Join(fixture.directory, "prepare-environment"))
	if bytes.Contains(environment, []byte(testToken)) {
		t.Errorf("the run token reached the preparation's environment")
	}
}

func TestARefusedCommitBreaksTheUnit(t *testing.T) {
	fixture := newStrictFixture(t, 3)
	result, events, _ := runUnit(t, testJobUnit(goodTestJob()), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "refused: the test job's commit") || len(fixture.goCalls(t)) > 0 {
		t.Fatalf("%s, errors %q, go calls %d", result.Status, errorPhases(events), len(fixture.goCalls(t)))
	}
	fixture = newStrictFixture(t, 2)
	result, events, _ = runUnit(t, testJobUnit(goodTestJob()), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "preparing the checkout failed (exit 2)") {
		t.Fatalf("%s, errors %q", result.Status, errorPhases(events))
	}
}

// The real prepare.sh against GitHub: a commit the public repository doesn't have is refused (exit 3) before any
// checkout, setup or test, even when the kept checkout's own origin holds it (a private remote, a planted ref): the
// commit is fetched from the public URL by its sha, never from what the checkout already knows. Skipped with -short or
// where GitHub can't be reached.
func TestPrepareRefusesACommitGitHubDoesntHave(t *testing.T) {
	if testing.Short() {
		t.Skip("reaches GitHub")
	}
	if exec.Command("git", "ls-remote", "--exit-code", protocol.AdamicRepository, "HEAD").Run() != nil {
		t.Skip("GitHub isn't reachable from here")
	}
	directory := t.TempDir()
	private := filepath.Join(directory, "private")
	git := func(arguments ...string) string {
		output, err := exec.Command("git", arguments...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	git("init", "-q", private)
	git("-C", private, "-c", "user.email=loom@test", "-c", "user.name=loom", "commit", "-q", "--allow-empty", "-m", "planted")
	planted := git("-C", private, "rev-parse", "HEAD")
	tree := filepath.Join(directory, "tree")
	git("clone", "-q", private, tree)
	script := filepath.Join(directory, "prepare.sh")
	os.WriteFile(script, prepareScript, 0o700)
	// keep, a root and a HOME of its own: this test runs on a developer's machine, whose /tmp and caches are other work's.
	command := exec.Command("bash", script, tree, planted, "", "", filepath.Join(directory, "environment"), "keep", filepath.Join(directory, "root"))
	command.Env = append(os.Environ(), "LOOM_PREPARE_ATTEMPTS=1", "HOME="+filepath.Join(directory, "home"))
	started := time.Now()
	output, _ := command.CombinedOutput()
	if command.ProcessState.ExitCode() != 3 || !strings.Contains(string(output), "refused: "+planted) {
		t.Fatalf("exit %d after %v: %s", command.ProcessState.ExitCode(), time.Since(started), output)
	}
	if _, err := os.Stat(filepath.Join(directory, "environment")); err == nil {
		t.Fatalf("a refused commit left an environment")
	}
}

// Every path prepare.sh keeps or clears beside the tree is under its root: no line of its code names /tmp.
func TestPrepareNamesNoHostPath(t *testing.T) {
	for number, line := range strings.Split(string(prepareScript), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, "/tmp") {
			t.Errorf("prepare.sh line %d names /tmp: %s", number+1, line)
		}
	}
}

// prepare.sh with trim clears its root and its HOME, and nothing else: a canary in the host's /tmp named like each
// thing it trims survives. The run ends at the refusal (a commit GitHub doesn't hold, or GitHub unreachable), after
// the trims.
func TestPrepareTouchesNothingOutsideItsRoot(t *testing.T) {
	directory := t.TempDir()
	root, home, tree := filepath.Join(directory, "root"), filepath.Join(directory, "home"), filepath.Join(directory, "root", "adamic")
	planted := []string{filepath.Join(root, "go-build1"), filepath.Join(root, "TestX"), filepath.Join(root, "adamic-stage3-lane-1"),
		filepath.Join(root, "adamic-gate", "left"), filepath.Join(home, ".cache", "adamic", "runtime", ".build-1")}
	for _, path := range planted {
		os.MkdirAll(filepath.Dir(path), 0o755)
		os.WriteFile(path, []byte("x"), 0o644)
	}
	suffix := strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	var canaries []string
	for _, prefix := range []string{"go-build", "Test", "adamic-stage3-lane-"} {
		canary := filepath.Join("/tmp", prefix+"loom-canary-"+suffix)
		if err := os.WriteFile(canary, []byte("x"), 0o644); err != nil {
			t.Skipf("can't place a canary in /tmp: %v", err)
		}
		canaries = append(canaries, canary)
		t.Cleanup(func() { os.Remove(canary) })
	}
	exec.Command("git", "init", "-q", tree).Run()
	script := filepath.Join(directory, "prepare.sh")
	os.WriteFile(script, prepareScript, 0o700)
	command := exec.Command("bash", script, tree, strings.Repeat("e", 40), "", "", filepath.Join(directory, "environment"), "trim", root)
	command.Env = append(os.Environ(), "LOOM_PREPARE_ATTEMPTS=1", "HOME="+home)
	output, _ := command.CombinedOutput()
	if command.ProcessState.ExitCode() != 3 {
		t.Fatalf("exit %d: %s", command.ProcessState.ExitCode(), output)
	}
	for _, path := range planted {
		if _, err := os.Stat(path); err == nil {
			t.Errorf("trim left %s", path)
		}
	}
	for _, canary := range canaries {
		if _, err := os.Stat(canary); err != nil {
			t.Errorf("prepare.sh touched %s, outside its root", canary)
		}
	}
}

func TestAStrictRunnersRootIsTmpAndOthersKeepTheirOwn(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	options := fixture.options(t)
	options.Root = ""
	runUnit(t, testJobUnit(goodTestJob()), options)
	if root, _ := os.ReadFile(filepath.Join(fixture.directory, "root")); string(root) != "/tmp\n" {
		t.Errorf("a strict runner's root is %q", root)
	}
	options.Strict = false
	runUnit(t, testJobUnit(goodTestJob()), options)
	if root, _ := os.ReadFile(filepath.Join(fixture.directory, "root")); string(root) != filepath.Join(options.WorkspaceParent, "loom-test-root")+"\n" {
		t.Errorf("a box runner's root is %q", root)
	}
	if trim, _ := os.ReadFile(filepath.Join(fixture.directory, "trim")); string(trim) != "keep\n" {
		t.Errorf("a box runner prepared with %q, not keep", trim)
	}
}
