package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

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
	// The job is read as plan writes it: three units of the opening outgrow a pipe's buffer (Oct 9 18:3xZ: the root
	// trim made the opening longer, and plan blocked on a full pipe with nothing reading it).
	var printed bytes.Buffer
	done := make(chan struct{})
	go func() {
		printed.ReadFrom(read)
		close(done)
	}()
	// No package setup here: the arithmetic below is the unit count's alone.
	err := plan([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--budget", "60", "--unit-setup", "10", "--package-setup", "0"})
	write.Close()
	<-done
	os.Stdout = stdout
	if err != nil {
		t.Fatal(err)
	}
	var job protocol.Job
	if err := protocol.Decode(&printed, &job); err != nil {
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

// A unit killed in its opening, before any test began (a cold instance's setup), is broken, Loom's to place again;
// a later run's attempt at it stands in, so the run reads green. Without that run it is void, never red.
func TestRedsReadsAKillInTheOpeningAsLoomsAndTakesItsRerun(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", directory)
	if err := os.MkdirAll(filepath.Join(directory, ".loom"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".loom", "token-secret"), []byte(strings.Repeat("s", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	write := func(name string, content string) string {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	job := write("job.json", `{"name": "j", "units": [{"id": "tests-0", "argv": ["true"], "timeoutSeconds": 90}]}`)
	record := write("record.jsonl", strings.Join([]string{
		`{"run": "r-1", "verdict": {"status": "red"}}`,
		`{"run": "r-1", "unit": "tests-0", "sequence": 0, "type": "started", "time": "2026-10-09T04:00:00Z"}`,
		`{"run": "r-1", "unit": "tests-0", "sequence": 1, "type": "output", "time": "2026-10-09T04:01:00Z", "stream": "stdout", "text": "loom-pilot: go lacks its standard library\n"}`,
		`{"run": "r-1", "unit": "tests-0", "sequence": 2, "type": "exit", "time": "2026-10-09T04:01:30Z", "timedOut": true}`,
	}, "\n")+"\n")
	// The again-run's tests-0 began and passed with no go test lines, an empty unit, which reads as green.
	againJob := write("again.json", `{"name": "j-again", "units": [{"id": "tests-0", "argv": ["true"], "timeoutSeconds": 90}]}`)
	againRecord := write("again.jsonl", strings.Join([]string{
		`{"run": "r-2", "verdict": {"status": "red"}}`,
		`{"run": "r-2", "unit": "tests-0", "sequence": 0, "type": "started", "time": "2026-10-09T04:05:00Z"}`,
		`{"run": "r-2", "unit": "tests-0", "sequence": 1, "type": "output", "time": "2026-10-09T04:05:01Z", "stream": "stdout", "text": "loom-pilot: box slot 1 cpus 4 tree x setup 1 s, 2 packages at a time\n"}`,
		`{"run": "r-2", "unit": "tests-0", "sequence": 2, "type": "exit", "time": "2026-10-09T04:05:30Z", "timedOut": true}`,
	}, "\n")+"\n")
	run := func(arguments ...string) (string, string) {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout := os.Stdout
		os.Stdout = writer
		verdict, err := reds(arguments)
		os.Stdout = stdout
		writer.Close()
		var printed bytes.Buffer
		printed.ReadFrom(reader)
		if err != nil {
			t.Fatal(err)
		}
		return verdict, printed.String()
	}
	verdict, printed := run("--job", job, "--record", record)
	if verdict != "void" || !strings.Contains(printed, "BROKEN tests-0: killed in its opening, before any test began: Loom's fault") || strings.Contains(printed, "KILLED") {
		t.Fatalf("a kill in the opening: verdict %q, printed:\n%s", verdict, printed)
	}
	// Placed again, the unit began its tests and was killed there: that is the change's red, read from the later run.
	verdict, printed = run("--job", job, "--record", record, "--rerun", againJob+":"+againRecord)
	if verdict != "red" || !strings.Contains(printed, "AGAIN tests-0: from run r-2") || !strings.Contains(printed, "KILLED tests-0: over budget, P0") {
		t.Fatalf("the again-run's kill after its tests began: verdict %q, printed:\n%s", verdict, printed)
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

// Under a budget a test no record has timed runs in an untimed unit of its own, killed at the budget's kill and needing
// its package's products, never packed by a guess (#h0xnmmf): the timed tests pack exactly as they would without it,
// and no remainder rides with it. Unbudgeted, it packs by the guess as it always has.
func TestATestNoRecordTimedRunsInAnUntimedUnitOfItsOwn(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"TestProduct_P","Elapsed":20}`+"\n", module)
	for test := range 6 {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"TestA%02d","Elapsed":7}`+"\n", module, test)
	}
	for test := range 3 {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sb","Test":"TestB%02d","Elapsed":12}`+"\n", module, test)
	}
	writer.Close()
	directory := t.TempDir()
	path := filepath.Join(directory, "reference.jsonl.gz")
	os.WriteFile(path, reference.Bytes(), 0o644)
	times := filepath.Join(directory, "times.tsv")
	os.WriteFile(times, []byte("loom_seconds\twhole_gate_seconds\tunit\tpackage\ttest\n9\t0\ttests-01\t"+module+"a\tTestNewTimed\n"), 0o644)
	timedTree := module + "a TestProduct_P\n" + module + "a TestNewTimed\n"
	for test := range 6 {
		timedTree += fmt.Sprintf("%sa TestA%02d\n", module, test)
	}
	for test := range 3 {
		timedTree += fmt.Sprintf("%sb TestB%02d\n", module, test)
	}
	plain := filepath.Join(directory, "timed.txt")
	os.WriteFile(plain, []byte(timedTree), 0o644)
	withNew := filepath.Join(directory, "tree.txt")
	os.WriteFile(withNew, []byte(timedTree+module+"a TestNewOne\n"+module+"a TestNewTwo\n"+module+"c TestC\n"), 0o644)
	planned := func(extra ...string) protocol.Job {
		read, write, _ := os.Pipe()
		stdout := os.Stdout
		os.Stdout = write
		var printed bytes.Buffer
		done := make(chan struct{})
		go func() {
			printed.ReadFrom(read)
			close(done)
		}()
		err := plan(append([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--loom-times", times, "--only", "^" + module + "(a|b|c)$"}, extra...))
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
		return job
	}
	budget := []string{"--budget", "60", "--unit-setup", "10", "--package-setup", "5"}
	job := planned(append(budget, "--tree-tests", withNew)...)
	var packed []protocol.JobUnit
	untimed := map[string]protocol.JobUnit{}
	for _, unit := range job.Units {
		if !strings.HasPrefix(unit.Id, "tests-untimed-") {
			packed = append(packed, unit)
			continue
		}
		if len(unit.Argv) != 6 {
			t.Fatalf("%s holds more than its test: %v", unit.Id, unit.Argv[5:])
		}
		if unit.TimeoutSeconds != 90 || unit.ExpectedSeconds != 60 {
			t.Fatalf("%s is killed at %d s and expected at %g s, want 90 and 60", unit.Id, unit.TimeoutSeconds, unit.ExpectedSeconds)
		}
		untimed[unit.Argv[5]] = unit
	}
	want := map[string][]string{module + "a=^(TestNewOne)$": {"product-00"}, module + "a=^(TestNewTwo)$": {"product-00"}, module + "c=^(TestC)$": nil}
	if len(untimed) != len(want) {
		t.Fatalf("%d untimed units, want %d: %v", len(untimed), len(want), untimed)
	}
	for spec, needs := range want {
		if unit, held := untimed[spec]; !held || !slices.Equal(unit.Needs, needs) {
			t.Fatalf("%s: unit %q needs %v, want %v", spec, unit.Id, unit.Needs, needs)
		}
	}
	// TestNewTimed is new too, but Loom timed it: it packs with the rest, which pack as if no test were untimed.
	if alone := planned(append(budget, "--tree-tests", plain)...); !reflect.DeepEqual(packed, alone.Units) {
		t.Fatalf("the timed tests packed differently beside the untimed ones:\n%v\nwithout them:\n%v", packed, alone.Units)
	}
	// With the remainder, package c's skip spec finds no packed unit of its package and goes to the remainder unit.
	for _, unit := range planned(append(budget, "--tree-tests", withNew, "--remainder")...).Units {
		if strings.HasPrefix(unit.Id, "tests-untimed-") && len(unit.Argv) != 6 {
			t.Fatalf("%s carries a remainder: %v", unit.Id, unit.Argv[5:])
		}
	}
	// Unbudgeted, the plan is today's: three units, the untimed tests in them by their guess.
	unbudgeted := planned("--units", "3", "--tree-tests", withNew)
	specs := ""
	for _, unit := range unbudgeted.Units {
		if strings.Contains(unit.Id, "untimed") || (strings.HasPrefix(unit.Id, "tests-") && unit.TimeoutSeconds != 3*3600+600) {
			t.Fatalf("unbudgeted, %s is killed at %d s", unit.Id, unit.TimeoutSeconds)
		}
		specs += strings.Join(unit.Argv[5:], " ") + " "
	}
	if len(unbudgeted.Units) != 4 || !strings.Contains(specs, "TestNewOne") || !strings.Contains(specs, module+"c=^(TestC)$") {
		t.Fatalf("unbudgeted, %d units: %s", len(unbudgeted.Units), specs)
	}
}

// A unit holding any of a setup family's tests (X_000, X_001, ..., XPlantedFailure) runs X_Setup too, once: its shards
// read what X_Setup made in their own process. Without it they failed "shared setup is absent" (proof 3, Oct 9).
func TestEveryUnitHoldingAShardRunsItsSetup(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	family := []string{"TestHooks_Setup", "TestHooksPlantedFailure"}
	for shard := range 8 {
		family = append(family, fmt.Sprintf("TestHooks_%03d", shard))
	}
	// Shards at 6 s between other tests at 7 s and 5 s: packed longest first without the family, they fall into
	// several chunks of the package.
	for _, test := range family {
		elapsed := 6
		if !strings.Contains(test, "_0") {
			elapsed = 1
		}
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"%s","Elapsed":%d}`+"\n", module, test, elapsed)
	}
	for test := range 20 {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"TestOther%02d","Elapsed":%d}`+"\n", module, test, 5+2*(test%2))
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
	planned, err := plannedTests(job)
	if err != nil {
		t.Fatal(err)
	}
	holding, shards := 0, 0
	for _, unit := range job.Units {
		held, setups := 0, 0
		for _, key := range planned.tests[unit.Id] {
			_, test, _ := strings.Cut(key, " ")
			switch {
			case test == "TestHooks_Setup":
				setups++
			case slices.Contains(family[1:], test):
				held++
			}
		}
		if held == 0 {
			continue
		}
		holding++
		shards += held
		if setups != 1 {
			t.Fatalf("%s holds %d of the family without its setup once: %v", unit.Id, held, unit.Argv[5:])
		}
	}
	if holding < 2 || shards != len(family)-1 {
		t.Fatalf("the family's %d tests over %d units: %d placed", len(family)-1, holding, shards)
	}
}

// A package's TestProduct_ tests are units of their own, planned first, and every test unit holding that package's
// tests needs them; a package with none adds no needs, and no test unit runs a product (#8gw478y). A product runs to
// completion under a 10-minute ceiling, never the test units' 90 s kill (@system_adamic's ruling, Oct 9).
func TestProductsAreUnitsTheirPackagesTestUnitsNeed(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"TestProduct_Corpus","Elapsed":20}`+"\n", module)
	for test := range 20 {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"TestA%02d","Elapsed":5}`+"\n", module, test)
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sb","Test":"TestB%02d","Elapsed":5}`+"\n", module, test)
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
	if job.Units[0].Id != "product-00" || job.Units[0].Argv[5] != module+"a=^(TestProduct_Corpus)$" || job.Units[0].TimeoutSeconds != 600 {
		t.Fatalf("the first unit isn't the product: %s %v %d", job.Units[0].Id, job.Units[0].Argv[5:], job.Units[0].TimeoutSeconds)
	}
	if _, err := protocol.Expand(job); err != nil {
		t.Fatal(err)
	}
	needing, free := 0, 0
	for _, unit := range job.Units[1:] {
		specs := strings.Join(unit.Argv[5:], " ")
		if strings.Contains(specs, "TestProduct_") {
			t.Fatalf("%s runs a product: %s", unit.Id, specs)
		}
		holdsA := strings.Contains(specs, module+"a=")
		switch {
		case holdsA && len(unit.Needs) == 1 && unit.Needs[0] == "product-00":
			needing++
		case !holdsA && len(unit.Needs) == 0:
			free++
		default:
			t.Fatalf("%s (package a: %t) needs %v", unit.Id, holdsA, unit.Needs)
		}
	}
	if needing == 0 || free == 0 {
		t.Fatalf("%d units need the product, %d don't", needing, free)
	}
}

// productFixture plans a reference of five packages' products and plain tests: a's four products sum to 260 s, b's
// three to 270 s, c's two to 240 s beside c's 400 s TestProduct_Huge, d's and e's one each, 20 s and 10 s. The tree
// also holds c's TestProduct_Fresh, which no record timed. a and d each have 20 s of tests, so under a 60 s budget
// they share a test unit, and b and c 40 s each.
func productFixture(t *testing.T, arguments ...string) protocol.Job {
	t.Helper()
	products := map[string]map[string]float64{
		"a": {"TestProduct_Alpha": 100, "TestProduct_Apex": 80, "TestProduct_Atlas": 50, "TestProduct_Axle": 30},
		"b": {"TestProduct_Badger": 120, "TestProduct_Basil": 90, "TestProduct_Birch": 60},
		"c": {"TestProduct_Cedar": 200, "TestProduct_Cobalt": 40, "TestProduct_Huge": 400},
		"d": {"TestProduct_Delta": 20},
		"e": {"TestProduct_Echo": 10},
	}
	var reference bytes.Buffer
	var tree []string
	writer := gzip.NewWriter(&reference)
	for packageName, names := range products {
		for name, seconds := range names {
			fmt.Fprintf(writer, `{"Action":"pass","Package":"%s%s","Test":"%s","Elapsed":%g}`+"\n", module, packageName, name, seconds)
			tree = append(tree, module+packageName+" "+name)
		}
	}
	plain := map[string][]float64{"a": {5, 5, 5, 5}, "b": {20, 20}, "c": {20, 20}, "d": {5, 5, 5, 5}}
	for packageName, tests := range plain {
		for test, seconds := range tests {
			fmt.Fprintf(writer, `{"Action":"pass","Package":"%s%s","Test":"TestPlain%02d","Elapsed":%g}`+"\n", module, packageName, test, seconds)
			tree = append(tree, fmt.Sprintf("%s%s TestPlain%02d", module, packageName, test))
		}
	}
	writer.Close()
	tree = append(tree, module+"c TestProduct_Fresh")
	directory := t.TempDir()
	referencePath, treePath := filepath.Join(directory, "reference.jsonl.gz"), filepath.Join(directory, "tree-tests.txt")
	os.WriteFile(referencePath, reference.Bytes(), 0o644)
	os.WriteFile(treePath, []byte(strings.Join(tree, "\n")+"\n"), 0o644)
	return planJob(t, append([]string{"--reference", referencePath, "--sha", testSha, "--target", "codex", "--tree-tests", treePath}, arguments...))
}

// planJob runs plan with these arguments and reads back the job it prints, checked as Loom would expand it.
func planJob(t *testing.T, arguments []string) protocol.Job {
	t.Helper()
	read, write, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = write
	var printed bytes.Buffer
	done := make(chan struct{})
	go func() {
		printed.ReadFrom(read)
		close(done)
	}()
	err := plan(arguments)
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
	if _, err := protocol.Expand(job); err != nil {
		t.Fatal(err)
	}
	return job
}

// productPlans are the two ways products pack: under a budget, and unbudgeted with --product-budget given.
var productPlans = [][]string{
	{"--budget", "60", "--unit-setup", "10", "--package-setup", "5", "--product-budget", "300"},
	{"--units", "2", "--product-budget", "300"},
}

// Under a budget, or unbudgeted with --product-budget given, products pack by their times into as few units of the
// product budget as fit (#fysfvrx): 800 s of timed products in 290 s of room are three units, never one each. A
// package's products in a unit are one spec, an alternation, so no unit holds two specs of one package (#4zwdxa3); a
// product no record timed and one over the budget run alone.
func TestAProductBudgetPacksProductsIntoUnitsOfIt(t *testing.T) {
	for _, arguments := range productPlans {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			job := productFixture(t, arguments...)
			var products []protocol.JobUnit
			for _, unit := range job.Units {
				if strings.HasPrefix(unit.Id, "product-") {
					products = append(products, unit)
				}
			}
			got := map[string][]string{}
			expected := map[string]float64{}
			for index, unit := range products {
				if unit.Id != fmt.Sprintf("product-%02d", index) || unit.TimeoutSeconds != 600 {
					t.Fatalf("product unit %d is %s, timeout %d", index, unit.Id, unit.TimeoutSeconds)
				}
				got[unit.Id] = unit.Argv[5:]
				expected[unit.Id] = unit.ExpectedSeconds
			}
			want := map[string][]string{
				"product-00": {module + "b=^(TestProduct_Badger|TestProduct_Basil|TestProduct_Birch)$", module + "e=^(TestProduct_Echo)$"},
				"product-01": {module + "a=^(TestProduct_Alpha|TestProduct_Apex|TestProduct_Atlas|TestProduct_Axle)$", module + "d=^(TestProduct_Delta)$"},
				"product-02": {module + "c=^(TestProduct_Cedar|TestProduct_Cobalt)$"},
				"product-03": {module + "c=^(TestProduct_Fresh)$"},
				"product-04": {module + "c=^(TestProduct_Huge)$"},
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("product units:\n%v\nwant:\n%v", got, want)
			}
			// A packed unit expects its products' seconds and each package's setup; one alone, its product's own.
			if expected["product-00"] != 270+10+2*5 || expected["product-01"] != 260+20+2*5 || expected["product-02"] != 240+5 || expected["product-04"] != 400 {
				t.Fatalf("expected seconds %v", expected)
			}
			for _, unit := range products {
				packages := map[string]bool{}
				for _, spec := range unit.Argv[5:] {
					packageName, _, _ := strings.Cut(spec, "=")
					if packages[packageName] {
						t.Fatalf("%s holds two specs of %s: %v", unit.Id, packageName, unit.Argv[5:])
					}
					packages[packageName] = true
				}
			}
		})
	}
}

// A test unit needs exactly the product units holding its packages' products, each once.
func TestTestUnitsNeedThePackedProductUnitsOfTheirPackages(t *testing.T) {
	for _, arguments := range productPlans {
		t.Run(strings.Join(arguments, " "), func(t *testing.T) {
			job := productFixture(t, arguments...)
			holding := map[string][]string{} // package to the product units holding its products
			for _, unit := range job.Units {
				if !strings.HasPrefix(unit.Id, "product-") {
					continue
				}
				for _, spec := range unit.Argv[5:] {
					packageName, _, _ := strings.Cut(spec, "=")
					holding[packageName] = append(holding[packageName], unit.Id)
				}
			}
			shared := 0
			for _, unit := range job.Units {
				if strings.HasPrefix(unit.Id, "product-") {
					continue
				}
				want := map[string]bool{}
				for _, spec := range unit.Argv[5:] {
					packageName, _, _ := strings.Cut(spec, "=")
					for _, id := range holding[packageName] {
						want[id] = true
					}
				}
				got := map[string]bool{}
				for _, need := range unit.Needs {
					if got[need] {
						t.Fatalf("%s needs %s twice: %v", unit.Id, need, unit.Needs)
					}
					got[need] = true
				}
				if !reflect.DeepEqual(got, want) || len(want) == 0 {
					t.Fatalf("%s (%v) needs %v, want %v", unit.Id, unit.Argv[5:], unit.Needs, want)
				}
				if strings.Contains(strings.Join(unit.Argv[5:], " "), module+"a=") && strings.Contains(strings.Join(unit.Argv[5:], " "), module+"d=") {
					shared++
				}
			}
			// a's and d's products share product-01, and their tests a unit: it needs product-01 once.
			if shared == 0 {
				t.Fatalf("no test unit holds both a and d")
			}
		})
	}
}

// Without a budget the products plan as before: one unit each, in package and name order, its one name the spec.
func TestWithoutABudgetEachProductIsAUnitOfItsOwn(t *testing.T) {
	job := productFixture(t, "--units", "2")
	var got []string
	for _, unit := range job.Units {
		if !strings.HasPrefix(unit.Id, "product-") {
			continue
		}
		if len(unit.Argv) != 6 || unit.TimeoutSeconds != 3*3600+600 {
			t.Fatalf("%s: %v, timeout %d", unit.Id, unit.Argv[5:], unit.TimeoutSeconds)
		}
		got = append(got, unit.Id+" "+unit.Argv[5])
	}
	var want []string
	for _, product := range []string{"a Alpha", "a Apex", "a Atlas", "a Axle", "b Badger", "b Basil", "b Birch", "c Cedar", "c Cobalt", "c Fresh", "c Huge", "d Delta", "e Echo"} {
		packageName, name, _ := strings.Cut(product, " ")
		want = append(want, fmt.Sprintf("product-%02d %s%s=^(TestProduct_%s)$", len(want), module, packageName, name))
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("product units:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// --product-times sizes the products alone by Loom's times: products the reference never ran (it holds none) pack by
// them, and the tests keep the reference's times, here 5 s each though the file says 500 s for one.
func TestProductTimesSizeOnlyTheProducts(t *testing.T) {
	directory := t.TempDir()
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	tree := []string{module + "a TestProduct_One", module + "a TestProduct_Two"}
	for test := range 4 {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"TestPlain%02d","Elapsed":5}`+"\n", module, test)
		tree = append(tree, fmt.Sprintf("%sa TestPlain%02d", module, test))
	}
	writer.Close()
	referencePath, treePath, timesPath := filepath.Join(directory, "reference.jsonl.gz"), filepath.Join(directory, "tree-tests.txt"), filepath.Join(directory, "loom-times.tsv")
	os.WriteFile(referencePath, reference.Bytes(), 0o644)
	os.WriteFile(treePath, []byte(strings.Join(tree, "\n")+"\n"), 0o644)
	os.WriteFile(timesPath, []byte("loom_seconds\twhole_gate_seconds\tunit\tpackage\ttest\n"+
		"100\t1\tproduct-00\t"+module+"a\tTestProduct_One\n80\t1\tproduct-01\t"+module+"a\tTestProduct_Two\n500\t5\ttests-00\t"+module+"a\tTestPlain00\n"), 0o644)
	arguments := []string{"--reference", referencePath, "--sha", testSha, "--target", "codex", "--tree-tests", treePath, "--units", "1", "--product-budget", "300"}
	summary := func(job protocol.Job) []string {
		var units []string
		for _, unit := range job.Units {
			units = append(units, fmt.Sprintf("%s %v %.0f s", unit.Id, unit.Argv[5:], unit.ExpectedSeconds))
		}
		return units
	}
	got := summary(planJob(t, append(arguments, "--product-times", timesPath)))
	want := []string{"product-00 [" + module + "a=^(TestProduct_One|TestProduct_Two)$] 185 s", "tests-00 [" + module + "a=^(TestPlain00|TestPlain01|TestPlain02|TestPlain03)$] 20 s"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("with --product-times:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// Without it the products are guesses, each a unit of its own.
	if got := summary(planJob(t, arguments)); len(got) != 3 || !strings.HasPrefix(got[1], "product-01 ["+module+"a=^(TestProduct_Two)$]") {
		t.Fatalf("without --product-times:\n%s", strings.Join(got, "\n"))
	}
}

// A packed product unit's slow products are each listed by their own seconds, never by the unit's wall.
func TestRedsListsEachSlowProductOfAPackedUnit(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", directory)
	os.MkdirAll(filepath.Join(directory, ".loom"), 0o700)
	os.WriteFile(filepath.Join(directory, ".loom", "token-secret"), []byte(strings.Repeat("s", 64)), 0o600)
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	for _, line := range []string{
		`{"Action":"pass","Package":"github.com/x/b","Test":"TestProduct_Badger","Elapsed":75}`,
		`{"Action":"pass","Package":"github.com/x/b","Test":"TestProduct_Basil","Elapsed":30}`,
		`{"Action":"pass","Package":"github.com/x/e","Test":"TestProduct_Echo","Elapsed":61}`,
	} {
		fmt.Fprintln(writer, line)
	}
	writer.Close()
	wire := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Write(output.Bytes())
	}))
	defer wire.Close()
	job := filepath.Join(directory, "job.json")
	os.WriteFile(job, []byte(`{"name": "j", "units": [{"id": "product-00", "argv": ["bash", "-c", "true", "adamic-gate-unit", "`+testSha+`", "github.com/x/b=^(TestProduct_Badger|TestProduct_Basil)$", "github.com/x/e=^(TestProduct_Echo)$"], "timeoutSeconds": 600}]}`), 0o644)
	record := filepath.Join(directory, "record.jsonl")
	os.WriteFile(record, []byte(strings.Join([]string{
		`{"run": "r-1", "verdict": {"status": "green"}}`,
		`{"run": "r-1", "unit": "product-00", "sequence": 0, "type": "started", "time": "2026-10-09T04:00:00Z"}`,
		`{"run": "r-1", "unit": "product-00", "sequence": 1, "type": "output", "time": "2026-10-09T04:00:01Z", "stream": "stdout", "text": "loom-pilot: setup 1 s, 2 packages at a time\n"}`,
		`{"run": "r-1", "unit": "product-00", "sequence": 2, "type": "exit", "time": "2026-10-09T04:03:00Z", "code": 0, "wallSeconds": 180}`,
		`{"run": "r-1", "unit": "product-00", "sequence": 3, "type": "uploaded", "time": "2026-10-09T04:03:01Z", "path": "loom-out/test.jsonl.gz", "sha256": "` + strings.Repeat("a", 64) + `"}`,
	}, "\n")+"\n"), 0o644)
	read, write, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = write
	verdict, err := reds([]string{"--job", job, "--record", record, "--wire", wire.URL})
	os.Stdout = stdout
	write.Close()
	var printed bytes.Buffer
	printed.ReadFrom(read)
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, line := range strings.Split(printed.String(), "\n") {
		if strings.HasPrefix(line, "PRODUCT OVER 60 S ") {
			listed = append(listed, line)
		}
	}
	want := []string{"PRODUCT OVER 60 S product-00 github.com/x/b TestProduct_Badger: 75 s", "PRODUCT OVER 60 S product-00 github.com/x/e TestProduct_Echo: 61 s"}
	if verdict != "green" || !reflect.DeepEqual(listed, want) {
		t.Fatalf("verdict %q, printed:\n%s", verdict, printed.String())
	}
}

// A test unit that never ran because its product failed (here, killed at its budget) is that product's red, not a unit
// Loom broke: the run reads red, never void.
func TestRedsReadsAFailedProductsDependentsAsItsRed(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", directory)
	os.MkdirAll(filepath.Join(directory, ".loom"), 0o700)
	os.WriteFile(filepath.Join(directory, ".loom", "token-secret"), []byte(strings.Repeat("s", 64)), 0o600)
	job := filepath.Join(directory, "job.json")
	os.WriteFile(job, []byte(`{"name": "j", "units": [{"id": "product-00", "argv": ["true"], "timeoutSeconds": 90}, {"id": "tests-01", "needs": ["product-00"], "argv": ["true"], "timeoutSeconds": 90}]}`), 0o644)
	record := filepath.Join(directory, "record.jsonl")
	os.WriteFile(record, []byte(strings.Join([]string{
		`{"run": "r-1", "verdict": {"status": "void"}}`,
		`{"run": "r-1", "unit": "product-00", "sequence": 0, "type": "started", "time": "2026-10-09T04:00:00Z"}`,
		`{"run": "r-1", "unit": "product-00", "sequence": 1, "type": "output", "time": "2026-10-09T04:00:01Z", "stream": "stdout", "text": "loom-pilot: box setup 1 s, 2 packages at a time\n"}`,
		`{"run": "r-1", "unit": "product-00", "sequence": 2, "type": "exit", "time": "2026-10-09T04:01:30Z", "timedOut": true}`,
		`{"run": "r-1", "unit": "product-00", "sequence": 3, "type": "finished", "time": "2026-10-09T04:01:30Z", "status": "failed"}`,
		`{"run": "r-1", "unit": "tests-01", "sequence": 0, "type": "error", "time": "2026-10-09T04:00:31Z", "phase": "place", "message": "not placed: it needs product-00, which ended failed"}`,
	}, "\n")+"\n"), 0o644)
	read, write, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = write
	verdict, err := reds([]string{"--job", job, "--record", record})
	os.Stdout = stdout
	write.Close()
	var printed bytes.Buffer
	printed.ReadFrom(read)
	if err != nil {
		t.Fatal(err)
	}
	if verdict != "red" || !strings.Contains(printed.String(), "NOT RUN tests-01: not run, its product product-00 failed") || strings.Contains(printed.String(), "BROKEN tests-01") {
		t.Fatalf("verdict %q, printed:\n%s", verdict, printed.String())
	}
}

// alternation writes a stem's numbered shards once, and unquoteAlternation reads every name back, quoted
// characters and groups alike.
func TestAnAlternationOfShardsReadsBackWhole(t *testing.T) {
	names := []string{"TestPlain", "TestX_0002", "TestX_0001", "TestX_0010", "TestOne_1", "TestMeta(a|b).c", "TestY_", "TestZ_007", "TestZ_008"}
	pattern := alternation(names)
	if !strings.Contains(pattern, "TestX_(?:0001|0002|0010)") || !strings.Contains(pattern, `TestMeta\(a\|b\)\.c`) || strings.Contains(pattern, "TestOne_(?:") {
		t.Fatalf("pattern %s", pattern)
	}
	read := unquoteAlternation(pattern)
	sort.Strings(read)
	want := append([]string(nil), names...)
	sort.Strings(want)
	if !reflect.DeepEqual(read, want) {
		t.Fatalf("read back %v, want %v", read, want)
	}
	compiled := regexp.MustCompile("^(" + pattern + ")$")
	for _, name := range names {
		if !compiled.MatchString(name) {
			t.Fatalf("%s doesn't match %s", name, pattern)
		}
	}
	if compiled.MatchString("TestX_0003") || compiled.MatchString("TestMetaXa") {
		t.Fatalf("%s matches too much", pattern)
	}
}

// A package of 4,000 numbered shards plans into arguments a Linux exec takes: main c869cea9's json shards, listed
// whole, made one argument of 325 KB and two units broke on "argument list too long".
func TestThousandsOfShardsPlanUnderTheArgumentLimit(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	for shard := range 4000 {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sjson","Test":"TestUpstreamRepositoryCorpusParity_%04d","Elapsed":0.01}`+"\n", module, shard)
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
	err := plan([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--budget", "60", "--unit-setup", "10", "--package-setup", "5", "--remainder"})
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
	planned, err := plannedTests(job)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, unit := range job.Units {
		for _, argument := range unit.Argv {
			if len(argument) > 120*1024 {
				t.Fatalf("%s has an argument of %d bytes", unit.Id, len(argument))
			}
		}
		count += len(planned.tests[unit.Id])
	}
	if count != 4000 {
		t.Fatalf("the plan reads back %d tests", count)
	}
}

// A fast gate's selection plans only the tests it names in its packages, plus their products and the setups the kept
// shards need; other packages plan as ever.
func TestOnlyTestsPlansTheSelectionWithItsProductsAndSetups(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	for _, test := range []string{"TestA", "TestB", "TestX_Setup", "TestX_000", "TestX_001", "TestProduct_P"} {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sa","Test":"%s","Elapsed":5}`+"\n", module, test)
	}
	fmt.Fprintf(writer, `{"Action":"pass","Package":"%sb","Test":"TestOther","Elapsed":5}`+"\n", module)
	writer.Close()
	directory := t.TempDir()
	path := filepath.Join(directory, "reference.jsonl.gz")
	os.WriteFile(path, reference.Bytes(), 0o644)
	only := filepath.Join(directory, "only.json")
	os.WriteFile(only, []byte(`{"`+module+`a": ["TestA", "TestX_000"]}`), 0o644)
	read, write, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = write
	var printed bytes.Buffer
	done := make(chan struct{})
	go func() {
		printed.ReadFrom(read)
		close(done)
	}()
	err := plan([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--budget", "60", "--unit-setup", "10", "--only-tests", only})
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
	planned, err := plannedTests(job)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, keys := range planned.tests {
		got = append(got, keys...)
	}
	sort.Strings(got)
	want := []string{module + "a TestA", module + "a TestProduct_P", module + "a TestX_000", module + "a TestX_Setup", module + "b TestOther"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("planned %v, want %v", got, want)
	}
}

// TestWASI's shards run in a spec of their own, and that spec, only that one, gets the WASI SDK's clang first on PATH
// (Oct 9: once TestWASI split into TestWASIUnit00 and on, the shards rode with native tests on native clang and all 36
// skipped on every pool record). Its mutant, the shards left in the native spec, fails here.
func TestWASIShardsRunInASpecOfTheirOwnWithTheWASIClang(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	for _, test := range []string{"TestWASIUnit00", "TestWASIUnit01", "TestWASITargetFlags", "TestOther"} {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%sinternal/native","Test":"%s","Elapsed":5}`+"\n", module, test)
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
	err := plan([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--units", "1"})
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
	var specs []string
	for _, unit := range job.Units {
		for _, spec := range unit.Argv {
			if strings.HasPrefix(spec, module+"internal/native=") {
				specs = append(specs, strings.TrimPrefix(spec, module+"internal/native="))
			}
		}
	}
	wasiSpecs := 0
	for _, pattern := range specs {
		if pattern == "^(TestWASIUnit00|TestWASIUnit01)$" {
			wasiSpecs++
		} else if strings.Contains(pattern, "TestWASIUnit") && !strings.Contains(pattern, "skip=") {
			t.Fatalf("a WASI shard rides with native tests: %q", pattern)
		}
	}
	if wasiSpecs != 1 {
		t.Fatalf("specs %q: want one of exactly the WASI shards", specs)
	}
	// The unit body's own PATH line, run as the unit runs it, on each kind of spec.
	var line string
	for _, candidate := range strings.Split(unitBody, "\n") {
		if strings.Contains(candidate, "${WASI_SYSROOT%/share/wasi-sysroot}/bin") {
			line = strings.TrimSpace(candidate)
		}
	}
	if line == "" {
		t.Fatal("no WASI PATH line in the unit body")
	}
	for pattern, wantWASI := range map[string]bool{
		"^(TestWASI)$": true, "^(TestWASIUnit00|TestWASIUnit01)$": true, "^(TestWASIUnit00)": true,
		"^(TestWASIUnit00|TestOther)$": false, "^(TestWASITargetFlags)$": false, ".": false,
	} {
		script := "pattern='" + pattern + "' WASI_SYSROOT=/sdk/share/wasi-sysroot PATH=/native\n" + line + "\necho \"${PATH}\""
		output, err := exec.Command("bash", "-c", script).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v %s", pattern, err, output)
		}
		if got := strings.TrimSpace(string(output)) == "/sdk/bin:/native"; got != wantWASI {
			t.Fatalf("%s: PATH %q, want the WASI clang first: %v", pattern, strings.TrimSpace(string(output)), wantWASI)
		}
	}
}

// A unit holding a WASI spec refuses, exit 2, on a runner whose WASI SDK can't name its wasm32 builtins, and runs
// everywhere else (Oct 9: main 20d538c0's whole gate skipped all 36 TestWASIUnit shards on the native clang).
func TestAWASISpecNeedsTheSDKsBuiltins(t *testing.T) {
	start := strings.Index(unitBody, "# A WASI spec needs the WASI SDK's clang")
	end := strings.Index(unitBody[start:], "\ndone\n")
	if start < 0 || end < 0 {
		t.Fatal("the WASI check isn't in unitBody")
	}
	check := unitBody[start : start+end+len("\ndone\n")]
	sdk := t.TempDir()
	os.MkdirAll(filepath.Join(sdk, "bin"), 0o755)
	os.MkdirAll(filepath.Join(sdk, "share", "wasi-sysroot"), 0o755)
	builtins := filepath.Join(sdk, "libclang_rt.builtins.a")
	os.WriteFile(filepath.Join(sdk, "bin", "clang"), []byte("#!/bin/bash\necho "+builtins+"\n"), 0o755)
	run := func(sysroot string, specs ...string) (int, string) {
		script := "specs=(" + strings.Join(specs, " ") + ")\n" + check + "echo ran\n"
		command := exec.Command("bash", "-c", script)
		command.Env = append(os.Environ(), "WASI_SYSROOT="+sysroot)
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	wasi := `'github.com/system-inc/adamic/internal/native=^(TestWASIUnit00|TestWASIUnit01)$'`
	other := `'github.com/system-inc/adamic/internal/native=^(TestWASITargetFlags)$'`
	if code, output := run(filepath.Join(sdk, "share", "wasi-sysroot"), other, wasi); code != 2 || !strings.Contains(output, "missing") {
		t.Fatalf("builtins missing: exit %d %q", code, output)
	}
	os.WriteFile(builtins, []byte("!<arch>\n"), 0o644)
	if code, output := run(filepath.Join(sdk, "share", "wasi-sysroot"), other, wasi); code != 0 || !strings.Contains(output, "ran") {
		t.Fatalf("builtins present: exit %d %q", code, output)
	}
	if code, output := run("", wasi); code != 2 || !strings.Contains(output, "WASI_SYSROOT=unset") {
		t.Fatalf("no SDK: exit %d %q", code, output)
	}
	if code, output := run("", other); code != 0 {
		t.Fatalf("no WASI spec, no SDK: exit %d %q", code, output)
	}
}

// A tree whose submodule a killed unit left on a revision it doesn't have fails every submodule update the same way; the
// opening makes the submodules again from nothing, once, and the checkout goes on (Oct 9: "Unable to find current
// revision in submodule path 'cohere/TypeScript'" broke units of canary 7, lint-alone and gocacheprog).
func TestABrokenSubmoduleIsMadeAgain(t *testing.T) {
	start := strings.Index(codexOpening, "# A submodule left mid-update")
	end := strings.Index(codexOpening[start:], "\nfi\n")
	if start < 0 || end < 0 {
		t.Fatal("the submodule repair isn't in codexOpening")
	}
	repair := codexOpening[start : start+end+len("\nfi\n")]
	root := t.TempDir()
	environment := append(os.Environ(), "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=protocol.file.allow", "GIT_CONFIG_VALUE_0=always",
		"GIT_AUTHOR_NAME=loom", "GIT_AUTHOR_EMAIL=loom@test", "GIT_COMMITTER_NAME=loom", "GIT_COMMITTER_EMAIL=loom@test")
	git := func(arguments ...string) {
		command := exec.Command("git", arguments...)
		command.Env = environment
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	sub, super, tree := filepath.Join(root, "sub"), filepath.Join(root, "super"), filepath.Join(root, "tree")
	git("init", "-q", sub)
	git("-C", sub, "commit", "-q", "--allow-empty", "-m", "sub")
	git("init", "-q", super)
	git("-C", super, "submodule", "add", "-q", sub, "sub")
	git("-C", super, "commit", "-q", "-m", "super")
	git("clone", "-q", super, tree)
	git("-C", tree, "submodule", "update", "-q", "--init")
	run := func() (int, string) {
		command := exec.Command("bash", "-c", "retry() { \"$@\"; }\n"+repair+"echo checked-out\n")
		command.Env = append(environment, "tree="+tree, "sha=HEAD")
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	if code, output := run(); code != 0 || strings.Contains(output, "making the submodules again") {
		t.Fatalf("a sound tree: exit %d %q", code, output)
	}
	os.WriteFile(filepath.Join(tree, ".git", "modules", "sub", "HEAD"), []byte(strings.Repeat("1", 40)+"\n"), 0o644)
	if code, output := run(); code != 0 || !strings.Contains(output, "making the submodules again") || !strings.Contains(output, "checked-out") {
		t.Fatalf("a broken submodule: exit %d %q", code, output)
	}
}

func TestAFullDiskEarlyInALargeLogIsLoomsNotARed(t *testing.T) {
	start := strings.Index(unitBody, "# A test that ran out of disk proved nothing")
	end := strings.Index(unitBody[start:], "\nfi\n")
	if start < 0 || end < 0 {
		t.Fatal("the full-disk check isn't in unitBody")
	}
	check := unitBody[start : start+end+len("\nfi\n")]
	out := t.TempDir()
	// The error first, then 26 MB of output: grep -q matches long before a pipe's writer could finish.
	log := "{\"Action\":\"output\",\"Output\":\"compile: writing output: no space left on device\\n\"}\n" +
		strings.Repeat("{\"Action\":\"output\",\"Output\":\"filler line to make the file large\"}\n", 400000)
	if err := os.WriteFile(filepath.Join(out, "part-1.jsonl"), []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "set -uo pipefail\nfreeMegabytes() { echo 9999; }\nstatus=1\n" + check + "exit \"${status}\"\n"
	command := exec.Command("bash", "-c", script)
	command.Env = append(os.Environ(), "out="+out, "HOME="+out)
	output, _ := command.CombinedOutput()
	if code := command.ProcessState.ExitCode(); code != 2 || !strings.Contains(string(output), "the instance's disk filled") {
		t.Fatalf("exit %d, want 2 naming the full disk: %q", code, output)
	}
}

// A test in a package the change touched is packed at 1.5x its predicted seconds (#6pekqxy), and only there: a and b
// each hold one 40 s test in 50 s of room; with a changed, a's test is 60 s and runs alone, b's still packs. A changed
// list naming no package the plan holds leaves the plan byte for byte as it was.
func TestAChangedPackagesTestsArePackedWithHeadroom(t *testing.T) {
	var reference bytes.Buffer
	writer := gzip.NewWriter(&reference)
	for _, packageName := range []string{"a", "b"} {
		fmt.Fprintf(writer, `{"Action":"pass","Package":"%s%s","Test":"TestX","Elapsed":40}`+"\n", module, packageName)
	}
	writer.Close()
	path := filepath.Join(t.TempDir(), "reference.jsonl.gz")
	os.WriteFile(path, reference.Bytes(), 0o644)
	planned := func(extra ...string) []byte {
		read, write, _ := os.Pipe()
		stdout := os.Stdout
		os.Stdout = write
		var printed bytes.Buffer
		done := make(chan struct{})
		go func() {
			printed.ReadFrom(read)
			close(done)
		}()
		err := plan(append([]string{"--reference", path, "--sha", testSha, "--target", "codex", "--budget", "60", "--unit-setup", "10", "--package-setup", "0"}, extra...))
		write.Close()
		os.Stdout = stdout
		<-done
		if err != nil {
			t.Fatal(err)
		}
		return printed.Bytes()
	}
	unchanged := planned()
	if other := planned("--changed-packages", module+"elsewhere"); !bytes.Equal(other, unchanged) {
		t.Fatalf("a changed package the plan doesn't hold changed the plan:\n%s\nwant\n%s", other, unchanged)
	}
	var job protocol.Job
	if err := protocol.Decode(bytes.NewReader(planned("--changed-packages", module+"a")), &job); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, unit := range job.Units {
		got[strings.Join(unit.Argv[5:], " ")] = fmt.Sprintf("%g s, killed at %d", unit.ExpectedSeconds, unit.TimeoutSeconds)
	}
	want := map[string]string{
		module + "b=^(TestX)$": "40 s, killed at 90",
		module + "a=^(TestX)$": "60 s, killed at 11400",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("units %v, want %v", got, want)
	}
}

func TestTheOpeningTrimsTheRootToItsFloor(t *testing.T) {
	start := strings.Index(codexOpening, "rootFree() {")
	end := strings.Index(codexOpening[start:], "[ \"$(freeMegabytes)\" -ge 3000 ]")
	if start < 0 || end < 0 {
		t.Fatal("the root trim isn't in codexOpening")
	}
	trim := codexOpening[start : start+end]
	run := func(floor string) (string, string) {
		home := t.TempDir()
		old, fresh := filepath.Join(home, ".cache", "adamic-build", "old"), filepath.Join(home, ".cache", "adamic-build", "fresh")
		goBuild := filepath.Join(home, ".cache", "go-build")
		for _, directory := range []string{old, fresh, goBuild} {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		hourAndMore := time.Now().Add(-2 * time.Hour)
		os.Chtimes(old, hourAndMore, hourAndMore)
		command := exec.Command("bash", "-c", "set -uo pipefail\n"+trim)
		command.Env = append(os.Environ(), "HOME="+home, "LOOM_ROOT_FLOOR_MB="+floor)
		output, _ := command.CombinedOutput()
		var left []string
		for _, directory := range []string{old, fresh, goBuild} {
			if _, err := os.Stat(directory); err == nil {
				left = append(left, filepath.Base(directory))
			}
		}
		return strings.Join(left, " "), string(output)
	}
	// Below the floor: the product cache's old entry goes, its fresh one stays, and go's build cache goes too.
	if left, output := run("999999999"); left != "fresh" || !strings.Contains(output, "trimmed the product cache") || !strings.Contains(output, "dropped go's build cache") {
		t.Fatalf("below the floor: left %q, %q", left, output)
	}
	// Above the floor: nothing is touched and nothing is said.
	if left, output := run("1"); left != "old fresh go-build" || output != "" {
		t.Fatalf("above the floor: left %q, %q", left, output)
	}
}
