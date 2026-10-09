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
