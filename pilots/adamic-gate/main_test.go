package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/system-inc/loom/protocol"
)

const testSha = "8161285ad449cc3a8cce6746120de2133c4a738a"
const testTools = "d785e9c2270000000000000000000000000000aa"

func writeList(t *testing.T, content string) string {
	path := filepath.Join(t.TempDir(), "units.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Each line of run.py's unit list is one pool unit, its phase and unit passed to the phase body as they were listed.
func TestPhaseUnitsAreOnePerLineWithThePhaseAndUnitAsArguments(t *testing.T) {
	units, err := phaseJobUnits(codexOpening, testSha, testTools, writeList(t, "build\nwasi internal/load/testdata/0.1/compile/01_hello.ts\n\ncatalog 08\nstage3 stage3-lane\n"))
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, unit := range units {
		ids = append(ids, unit.Id)
	}
	if _, err := protocol.Expand(protocol.Job{Name: "j", Units: units}); err != nil {
		t.Fatalf("not a job Loom takes: %v", err)
	}
	if !reflect.DeepEqual(ids, []string{"phase-build", "phase-wasi-internal-load-testdata-0-1-compile-01-hello-ts", "phase-catalog-08", "phase-stage3-stage3-lane"}) {
		t.Fatalf("ids %v", ids)
	}
	tail := units[1].Argv[3:]
	if !reflect.DeepEqual(tail, []string{"adamic-gate-phase", testSha, testTools, "wasi", "internal/load/testdata/0.1/compile/01_hello.ts"}) {
		t.Fatalf("argv after the script %v", tail)
	}
	if units[0].Outputs[0].Glob != "loom-out/phase.tar.gz" || units[0].Resources.Cpus != 4 {
		t.Fatalf("unit %+v", units[0])
	}
}

func TestAPhaseListThatIsNotRunPysShapeIsRefused(t *testing.T) {
	for _, content := range []string{"", "build vet extra\n", "build;rm\n", "build\nbuild\n", "wasi -dash\n", "wasi a/../../etc\n", "build/x\n"} {
		if _, err := phaseJobUnits(codexOpening, testSha, testTools, writeList(t, content)); err == nil {
			t.Errorf("%q accepted", content)
		}
	}
	if _, err := phaseJobUnits(codexOpening, testSha, "d785e9c", writeList(t, "build\n")); err == nil || !strings.Contains(err.Error(), "40-character") {
		t.Errorf("a short tools sha read as %v", err)
	}
}

// The scripts a unit runs parse as bash, each opening with each body.
func TestEveryUnitScriptParsesAsBash(t *testing.T) {
	for name, script := range map[string]string{
		"codex phase": codexPreamble("") + codexOpening + phaseBody, "box phase": boxOpening + phaseBody,
		"codex tests": codexPreamble("") + codexOpening + unitBody, "codex build-vet": codexPreamble("") + codexOpening + buildVetBody,
	} {
		if output, err := exec.Command("bash", "-n", "-c", script).CombinedOutput(); err != nil {
			t.Errorf("%s: %v: %s", name, err, output)
		}
	}
}

// With --remainder, one unit asks go list for packages the reference never ran, and compare counts what they ran
// as that unit's own.
func TestTheUnplannedSpecNamesTheKnownPackagesAndCompareReadsIt(t *testing.T) {
	job := protocol.Job{Name: "j", Units: []protocol.JobUnit{
		{Id: "tests-00", Argv: []string{"bash", "-c", "body", "adamic-gate-unit", testSha, module + "a=^(TestA)$", "@unplanned=" + module + "a," + module + "b"}},
	}}
	planned, err := plannedTests(job)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(planned.unplanned["tests-00"], map[string]bool{module + "a": true, module + "b": true}) {
		t.Fatalf("unplanned %v", planned.unplanned)
	}
	if !reflect.DeepEqual(planned.tests["tests-00"], []string{module + "a TestA"}) {
		t.Fatalf("tests %v", planned.tests)
	}
	if output, err := exec.Command("bash", "-n", "-c", unitBody).CombinedOutput(); err != nil {
		t.Fatalf("unitBody: %v %s", err, output)
	}
}

func TestLoomTimesAreReadByPackageAndTest(t *testing.T) {
	path := writeList(t, "loom_seconds\twhole_gate_seconds\tunit\tpackage\ttest\n962.80\t303.73\ttests-38\t"+module+"stage1/cohere/tsprinter\tTestMutants\n")
	timed, err := readLoomTimes(path)
	if err != nil || timed[module+"stage1/cohere/tsprinter TestMutants"] != 962.80 || len(timed) != 1 {
		t.Fatalf("%v %v", timed, err)
	}
	if _, err := readLoomTimes(writeList(t, "loom_seconds\nx\ty\n")); err == nil {
		t.Fatal("a malformed line was read")
	}
}

// Under a budget, the unit count comes out of the times: an item over the budget runs alone with the long timeout,
// and the rest pack into as few units as fit, each killed at budget plus a half.
func TestABudgetSetsTheUnitCountAndAnOverBudgetTestRunsAlone(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	for name, seconds := range map[string]float64{"TestHuge": 100, "TestA": 40, "TestB": 30, "TestC": 20, "TestD": 10} {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"%s","Elapsed":%g}`+"\n", module, name, seconds)
	}
	writer.Close()
	path := filepath.Join(t.TempDir(), "reference.jsonl.gz")
	os.WriteFile(path, reference.Bytes(), 0o644)
	read, write, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = write
	// No package setup here: the arithmetic below is the unit count's alone.
	err := plan([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--budget", "60", "--unit-setup", "10", "--package-setup", "0"})
	write.Close()
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	var job protocol.Job
	if err := protocol.Decode(read, &job); err != nil {
		t.Fatal(err)
	}
	timeouts, huge := []int{}, ""
	for _, unit := range job.Units {
		timeouts = append(timeouts, unit.TimeoutSeconds)
		if strings.Contains(strings.Join(unit.Argv[5:], " "), "TestHuge") {
			huge = unit.Id
			if len(unit.Argv) != 6 {
				t.Fatalf("the over-budget test shares its unit: %v", unit.Argv[5:])
			}
		}
	}
	// 100 s of packable tests in 50 s of room: two units killed at 90 s, then TestHuge alone with the long timeout.
	if !reflect.DeepEqual(timeouts, []int{90, 90, 3*3600 + 600}) || huge != "tests-02" {
		t.Fatalf("timeouts %v, TestHuge in %q", timeouts, huge)
	}
}

// A unit killed at its budget is a red that names each leaf its kill trap found still running, one killed line per
// leaf for its owner's P0 (#2en3b4t (f)); a killed unit whose trap named none still reads killed.
func TestRedsNamesTheLeavesRunningAtAKill(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", directory)
	if err := os.MkdirAll(filepath.Join(directory, ".loom"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".loom", "token-secret"), []byte(strings.Repeat("s", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	job := `{"name": "j", "units": [{"id": "tests-0", "argv": ["true"], "timeoutSeconds": 90}, {"id": "tests-1", "argv": ["true"], "timeoutSeconds": 90}]}`
	record := strings.Join([]string{
		`{"run": "r-1", "verdict": {"status": "red"}}`,
		`{"run": "r-1", "unit": "tests-0", "sequence": 0, "type": "started", "time": "2026-10-09T04:00:00Z"}`,
		`{"run": "r-1", "unit": "tests-0", "sequence": 1, "type": "output", "time": "2026-10-09T04:01:30Z", "stream": "stdout", "text": "loom-pilot: running at the kill: github.com/x/p TestA/two\nloom-pilot: running at the kill: github.com/x/q TestB\n"}`,
		`{"run": "r-1", "unit": "tests-0", "sequence": 2, "type": "exit", "time": "2026-10-09T04:01:30Z", "timedOut": true}`,
		`{"run": "r-1", "unit": "tests-1", "sequence": 0, "type": "started", "time": "2026-10-09T04:00:00Z"}`,
		`{"run": "r-1", "unit": "tests-1", "sequence": 1, "type": "output", "time": "2026-10-09T04:01:30Z", "stream": "stdout", "text": "loom-pilot: setup 3 s\n"}`,
		`{"run": "r-1", "unit": "tests-1", "sequence": 2, "type": "exit", "time": "2026-10-09T04:01:30Z", "timedOut": true}`,
	}, "\n") + "\n"
	jobPath, recordPath := filepath.Join(directory, "job.json"), filepath.Join(directory, "record.jsonl")
	if err := os.WriteFile(jobPath, []byte(job), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recordPath, []byte(record), 0o644); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := os.Stdout
	os.Stdout = writer
	verdict, err := reds([]string{"--job", jobPath, "--record", recordPath})
	os.Stdout = stdout
	writer.Close()
	var printed bytes.Buffer
	printed.ReadFrom(reader)
	if err != nil {
		t.Fatal(err)
	}
	if verdict != "red" {
		t.Fatalf("verdict %q, printed:\n%s", verdict, printed.String())
	}
	for _, want := range []string{
		"KILLED tests-0 github.com/x/p TestA/two: over budget, P0",
		"KILLED tests-0 github.com/x/q TestB: over budget, P0",
		"KILLED tests-1: over budget, P0",
		"running at the kill: github.com/x/p TestA/two; github.com/x/q TestB",
		"2 killed over budget",
	} {
		if !strings.Contains(printed.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, printed.String())
		}
	}
}

// The tree's own tests are the plan's list: a test gone from the tree is dropped with its subtests, and a new one is
// sized by Loom's time, a share of what its package lost, its package's median doubled, or 30 s. A package outside
// --only stays out (the reference arrives filtered the same way).
func TestThePlansTestListIsTheTrees(t *testing.T) {
	reference := map[string]result{
		"p TestOld":    {action: "pass", seconds: 100},
		"p TestOld/x":  {action: "pass", seconds: 90},
		"p TestKeep":   {action: "pass", seconds: 10},
		"q TestQOne":   {action: "pass", seconds: 4},
		"q TestQTwo":   {action: "pass", seconds: 6},
		"q TestQThree": {action: "pass", seconds: 8},
	}
	path := filepath.Join(t.TempDir(), "tree.txt")
	tree := "p TestKeep\np TestNewOne\np TestNewTwo\nq TestQOne\nq TestQTwo\nq TestQThree\nq TestQFour\nr TestR\nskip TestOther\n"
	if err := os.WriteFile(path, []byte(tree), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := fromTheTree(reference, path, "^(p|q|r)$", map[string]float64{"p TestNewOne": 12}); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"p TestKeep": 10, "p TestNewOne": 12, "p TestNewTwo": 75, "q TestQOne": 4, "q TestQTwo": 6, "q TestQThree": 8, "q TestQFour": 12, "r TestR": 30}
	got := map[string]float64{}
	for key, outcome := range reference {
		got[key] = outcome.seconds
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if err := os.WriteFile(path, []byte("p TestKeep/sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if fromTheTree(map[string]result{}, path, "", nil) == nil {
		t.Fatal("a subtest line was taken as a top-level test")
	}
}

// A subtest Loom has timed under a test the plan holds joins the plan, so a test split into subtests is packed child
// by child: tsprinter's TestExpressionsAgainstGoAndPrettier was planned whole at 486 s after its split into shards.
func TestLoomsTimedSubtestsSplitATestTheReferenceHeldWhole(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"TestBig","Elapsed":480}`+"\n", module)
	fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"TestSmall","Elapsed":1}`+"\n", module)
	writer.Close()
	directory := t.TempDir()
	path := filepath.Join(directory, "reference.jsonl.gz")
	os.WriteFile(path, reference.Bytes(), 0o644)
	var table strings.Builder
	table.WriteString("loom_seconds\twhole_gate_seconds\tunit\tpackage\ttest\n")
	fmt.Fprintf(&table, "480\t0\ttimes\t%sa\tTestBig\n", module)
	for shard := range 12 {
		fmt.Fprintf(&table, "40\t0\ttimes\t%sa\tTestBig/shard-%03d\n", module, shard)
	}
	times := filepath.Join(directory, "times.tsv")
	os.WriteFile(times, []byte(table.String()), 0o644)
	read, write, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = write
	// The job is larger than a pipe holds, so it's read as plan writes it.
	var printed bytes.Buffer
	done := make(chan struct{})
	go func() {
		printed.ReadFrom(read)
		close(done)
	}()
	err := plan([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--budget", "60", "--unit-setup", "10", "--split-all", "--loom-times", times})
	write.Close()
	os.Stdout = stdout
	<-done
	if err != nil {
		t.Fatal(err)
	}
	var job protocol.Job
	if err := protocol.Decode(&printed, &job); err != nil {
		t.Fatal(err)
	}
	// Twelve 40 s shards and a 1 s test in 50 s of room: thirteen units, every one packed and killed at 90 s, none of
	// them TestBig whole at 480 s with the long timeout.
	for _, unit := range job.Units {
		if unit.TimeoutSeconds != 90 {
			t.Fatalf("%s runs long (%d s): %v", unit.Id, unit.TimeoutSeconds, unit.Argv[5:])
		}
	}
	if len(job.Units) != 12 && len(job.Units) != 13 {
		t.Fatalf("%d units", len(job.Units))
	}
}

// Under a budget a package's tests pack together (#wa8exgw): three packages of many small tests make units that each
// run one package, not every unit a slice of all three.
func TestABudgetPacksAPackagesTestsTogether(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	for _, packageName := range []string{"a", "b", "c"} {
		for test := range 30 {
			fmt.Fprintf(writer, `{"Action":"pass","Package":"%s%s","Test":"Test%02d","Elapsed":3}`+"\n", module, packageName, test)
		}
	}
	writer.Close()
	path := filepath.Join(t.TempDir(), "reference.jsonl.gz")
	os.WriteFile(path, reference.Bytes(), 0o644)
	read, write, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = write
	var printed bytes.Buffer
	done := make(chan struct{})
	go func() {
		printed.ReadFrom(read)
		close(done)
	}()
	err := plan([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--budget", "60", "--unit-setup", "10", "--package-setup", "5"})
	write.Close()
	os.Stdout = stdout
	<-done
	if err != nil {
		t.Fatal(err)
	}
	var job protocol.Job
	if err := protocol.Decode(&printed, &job); err != nil {
		t.Fatal(err)
	}
	// 90 s of each package in 45 s of room after its setup: two units a package, six in all, each one package.
	if len(job.Units) != 6 {
		t.Fatalf("%d units", len(job.Units))
	}
	for _, unit := range job.Units {
		packages := map[string]bool{}
		for _, spec := range unit.Argv[5:] {
			name, _, _ := strings.Cut(spec, "=")
			packages[name] = true
		}
		if len(packages) != 1 {
			t.Fatalf("%s runs %d packages: %v", unit.Id, len(packages), unit.Argv[5:])
		}
	}
}
