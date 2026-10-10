package planner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A declared need is stamped on its unit for placement and never moves the key; a need no pool serving the key's
// runner holds is refused at plan time, so the unit can't wait unplaced; and a need without its record is refused.
// Not parallel: it points the package's PoolsFile at its own table.
func TestADeclaredNeedIsStampedNeverKeyedAndRefusedWhenNoPoolHoldsIt(t *testing.T) {
	tree, gateTools := planFixture(t)
	runner, other := strings.Repeat("d", 64), strings.Repeat("e", 64)
	tools := Tools{Runner: runner, Go: "go1.27.0"}
	poolsFile := filepath.Join(t.TempDir(), "pools.json")
	// The big pool serves another runner, so only the small one counts for this key.
	os.WriteFile(poolsFile, []byte(`{"pools": [{"name": "codex-strict", "tier": "codex-strict", "runner": "`+runner+`", "memoryMegabytes": 16384, "cpus": 4},
		{"name": "box-strict", "tier": "box-strict", "runner": "`+runner+`", "memoryMegabytes": 65536, "cpus": 8},
		{"name": "box-other", "tier": "box-strict", "runner": "`+other+`", "memoryMegabytes": 262144, "cpus": 64}]}`), 0o644)
	saved := PoolsFile
	PoolsFile = poolsFile
	defer func() { PoolsFile = saved }()
	plan := func(needs string) (map[string]PlannedResult, error) {
		writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/unit-needs.json": needs})
		results, err := PlanTree(tree, gateTools, tools, MemoryIndex{}, false)
		byName := map[string]PlannedResult{}
		for _, result := range results {
			byName[result.Name] = result
		}
		return byName, err
	}
	none, err := plan(`{"version": 1, "units": []}`)
	if err != nil {
		t.Fatal(err)
	}
	declared, err := plan(`{"version": 1, "units": [{"package": "a", "memoryMegabytes": 20480, "cpus": 4, "record": "gate-logs/x/fast"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	a, b := declared["example.com/plan/a"], declared["example.com/plan/b"]
	if a.Resources == nil || a.Resources.MemoryMegabytes != 20480 || a.Resources.Cpus != 4 || b.Resources != nil {
		t.Fatalf("a's resources %+v and b's %+v: want a's declared need on a alone", a.Resources, b.Resources)
	}
	if a.UnitKey != none["example.com/plan/a"].UnitKey {
		t.Error("declaring a's need moved its key")
	}
	// 128 GB: only the pool of another runner could hold it, and this key's runner isn't served there.
	if _, err := plan(`{"version": 1, "units": [{"package": "a", "memoryMegabytes": 131072, "cpus": 4, "record": "gate-logs/x/fast"}]}`); err == nil {
		t.Error("a need only another runner's pool holds was planned")
	}
	if _, err := plan(`{"version": 1, "units": [{"package": "a", "memoryMegabytes": 20480}]}`); err == nil {
		t.Error("a need without the record that measured it was planned")
	}
	// Queue refuses a unit's resources without both keys, so a need without cpus would refuse the whole plan there.
	if _, err := plan(`{"version": 1, "units": [{"package": "a", "memoryMegabytes": 20480, "record": "gate-logs/x/fast"}]}`); err == nil {
		t.Error("a need without cpus was planned")
	}
	encoded, _ := json.Marshal(a.Resources)
	if string(encoded) != `{"cpus":4,"memoryMegabytes":20480}` {
		t.Errorf("a's resources encode as %s, not Queue's two keys", encoded)
	}
}
