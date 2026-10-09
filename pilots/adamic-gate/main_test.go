package main

import (
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
