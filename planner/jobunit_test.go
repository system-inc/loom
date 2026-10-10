package planner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// A rerun's job is the planned unit's package and selection at the commit, valid as a job, and requiring the toolchains
// its key names; a unit whose env a test job can't carry is refused, never run without it.
func TestJobUnitForRunsThePlannedUnitAtACommit(t *testing.T) {
	t.Parallel()
	sha := strings.Repeat("c", 40)
	parts := KeyParts{Kind: "test", Package: protocol.AdamicModule + "/internal/oracle", Select: Select{Run: "^(TestA|TestB)$"},
		Tools: Tools{Go: "go1.27.1", Clang: "clang 19"}, Env: map[string]string{"ADAMIC_GATE_UNCACHED": "1", "ADAMIC_GATE_SAMPLE": sha}}
	unit, err := JobUnitFor(parts, sha)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.Expand(protocol.Job{Name: "rerun", Units: []protocol.JobUnit{unit}}); err != nil {
		t.Fatalf("the rerun isn't a valid job: %v", err)
	}
	if unit.Test.Sha != sha || unit.Test.Packages[0].Package != parts.Package || unit.Test.Packages[0].Run != parts.Select.Run || unit.Test.Sample != sha {
		t.Errorf("the job %+v isn't the planned unit at %s", unit.Test, sha)
	}
	if strings.Join(unit.Requires, ",") != "go,clang" || unit.TimeoutSeconds != protocol.KindCeilings["test"] || unit.Cache {
		t.Errorf("requires %v, timeout %d, cache %v", unit.Requires, unit.TimeoutSeconds, unit.Cache)
	}
	parts.Env["ADAMIC_GATE_CHANGED"] = strings.Repeat("d", 64)
	if _, err := JobUnitFor(parts, sha); err == nil {
		t.Error("a unit keyed on a changed-paths file it can't carry got a job")
	}
}

// A future's job unit is every kind under its key's id: a product as its package's go test under the product ceiling,
// a phase as run.py's line at the key's tools merged onto the base, and changed paths carried only when they hash to
// the key. gofmt's phase is the one phase that carries them (TestTheGofmtPhaseCarriesTheChangesPathsOnlyWhenTheyMatch).
func TestFutureJobUnitSaysEveryKindExactly(t *testing.T) {
	t.Parallel()
	sha, base, key := strings.Repeat("c", 40), strings.Repeat("b", 40), strings.Repeat("1", 64)
	product := KeyParts{Kind: "product", Package: protocol.AdamicModule + "/internal/lower", Select: Select{Run: "^TestProduct_Lower$"}, Tools: Tools{Go: "go1.27.1"}}
	unit, err := FutureJobUnit(key, product, sha, base, nil)
	if err != nil || unit.Id != key || unit.Kind != "product" || unit.TimeoutSeconds != protocol.KindCeilings["product"] || unit.Test.Packages[0].Run != product.Select.Run {
		t.Fatalf("the product's job is %+v (%v)", unit, err)
	}
	phase := KeyParts{Kind: "phase", Package: protocol.AdamicModule, Select: Select{Run: "wasi fixture-07"}, Tools: Tools{Go: "go1.27.1"}, GateTools: strings.Repeat("d", 40), Env: GateEnvironment}
	unit, err = FutureJobUnit(key, phase, sha, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := protocol.Expand(protocol.Job{Name: "future", Units: []protocol.JobUnit{unit}}); err != nil || unit.Test.Phase != "wasi fixture-07" || unit.Test.Base != base || unit.Test.Tools != phase.GateTools {
		t.Fatalf("the phase's job is %+v (%v)", unit.Test, err)
	}
	test := KeyParts{Kind: "test", Package: protocol.AdamicModule + "/internal/oracle", Env: map[string]string{"ADAMIC_GATE_CHANGED": ChangedPathsSum([]string{"b.go", "a.go"})}}
	if unit, err := FutureJobUnit(key, test, sha, base, []string{"b.go", "a.go"}); err != nil || strings.Join(unit.Test.ChangedPaths, ",") != "a.go,b.go" {
		t.Errorf("the changed paths came out %v (%v)", unit.Test.ChangedPaths, err)
	}
	if _, err := FutureJobUnit(key, test, sha, base, []string{"a.go"}); err == nil {
		t.Error("paths that don't hash to the key were carried")
	}
}

// gofmt's phase unit, which the runner runs itself, is a phase job carrying the change's paths, sorted, when they hash
// to its key's ADAMIC_GATE_CHANGED; paths that don't, or none for a key that holds them, are refused, and no other
// phase carries paths at all. Mutant: phaseJob accepting mismatched paths.
func TestTheGofmtPhaseCarriesTheChangesPathsOnlyWhenTheyMatch(t *testing.T) {
	t.Parallel()
	sha, base, key := strings.Repeat("c", 40), strings.Repeat("b", 40), strings.Repeat("1", 64)
	env := map[string]string{"ADAMIC_GATE_CHANGED": ChangedPathsSum([]string{"b.go", "a.go"})}
	for name, value := range GateEnvironment {
		env[name] = value
	}
	gofmt := KeyParts{Kind: "phase", Package: protocol.AdamicModule, Select: Select{Run: protocol.GofmtPhase}, Tools: Tools{Go: "go1.27.1"}, GateTools: strings.Repeat("d", 40), Env: env}
	unit, err := FutureJobUnit(key, gofmt, sha, base, []string{"b.go", "a.go"})
	if err != nil {
		t.Fatalf("gofmt keyed on its change's paths got no job: %v", err)
	}
	if _, err := protocol.Expand(protocol.Job{Name: "future", Units: []protocol.JobUnit{unit}}); err != nil || unit.Kind != "phase" || unit.Test.Phase != protocol.GofmtPhase ||
		strings.Join(unit.Test.ChangedPaths, ",") != "a.go,b.go" || unit.Test.Base != base || unit.Test.Tools != gofmt.GateTools || unit.Test.Go != "go1.27.1" {
		t.Fatalf("gofmt's job is %+v (%v)", unit.Test, err)
	}
	for name, changed := range map[string][]string{"a path short": {"a.go"}, "another path": {"a.go", "c.go"}, "no paths": {}, "no record": nil} {
		if _, err := FutureJobUnit(key, gofmt, sha, base, changed); err == nil {
			t.Errorf("%s: gofmt was carried paths that don't hash to its key", name)
		}
	}
	vet := gofmt
	vet.Select.Run = "vet"
	if _, err := FutureJobUnit(key, vet, sha, base, []string{"b.go", "a.go"}); err == nil {
		t.Error("a run.py phase keyed on changed paths got a job carrying them")
	}
	// Keyed on no paths, gofmt would check nothing: refused, but for a witness, whose base is its sha.
	gofmt.Env = GateEnvironment
	if _, err := FutureJobUnit(key, gofmt, sha, base, nil); err == nil {
		t.Error("gofmt keyed on no paths, on a change, got a job that checks nothing")
	}
	if unit, err := FutureJobUnit(key, gofmt, sha, sha, nil); err != nil || len(unit.Test.ChangedPaths) != 0 {
		t.Errorf("a witness's gofmt came out %+v (%v)", unit.Test, err)
	}
	// A key with no Go release can't name the gofmt to run.
	gofmt.Tools.Go = ""
	if _, err := FutureJobUnit(key, gofmt, sha, sha, nil); err == nil {
		t.Error("gofmt keyed on no Go release got a job")
	}
}

// ChangedPathsSum is exactly the env part KeyEnv makes of ChangedPathsFile's file, or no carried path would match.
func TestChangedPathsSumIsTheKeysEnvPart(t *testing.T) {
	t.Parallel()
	paths := []string{"z/z.go", "a.go"}
	file, err := ChangedPathsFile(t.TempDir(), paths)
	if err != nil {
		t.Fatal(err)
	}
	env, err := KeyEnv(map[string]string{"ADAMIC_GATE_CHANGED": file})
	if err != nil || env["ADAMIC_GATE_CHANGED"] != ChangedPathsSum(paths) {
		t.Fatalf("the key holds %s, the sum is %s (%v)", env["ADAMIC_GATE_CHANGED"], ChangedPathsSum(paths), err)
	}
}

// KeyAt keys a planned unit's identity on another tree as PlanTree keys it there: the same tree gives the planned key,
// and a tree where the unit's closure moved gives a new one.
func TestKeyAtKeysTheUnitAsThePlanWould(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	planned := planOf(t, tree, gateTools, MemoryIndex{})["example.com/plan/a"]
	parts, key, err := KeyAt(tree, gateTools, planned.KeyParts)
	if err != nil {
		t.Fatal(err)
	}
	if key != planned.UnitKey {
		t.Fatalf("KeyAt on the planned tree gave %s, the plan %s (parts %+v)", key, planned.UnitKey, parts)
	}
	os.WriteFile(filepath.Join(tree, "a/a.go"), []byte("package a\n\nconst Moved = 1\n"), 0o644)
	if _, moved, err := KeyAt(tree, gateTools, planned.KeyParts); err != nil || moved == planned.UnitKey {
		t.Fatalf("KeyAt on a tree where a's closure moved gave the old key (%v)", err)
	}
}
