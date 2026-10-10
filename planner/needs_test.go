package planner

import (
	"encoding/json"
	"strings"
	"testing"
)

// A declared need is stamped on its unit for placement and never moves the key; a need no live tier meets is refused
// at plan time, so the unit can't wait unplaced; and a need without the record that measured it is refused.
func TestADeclaredNeedIsStampedNeverKeyedAndRefusedWhenNoTierMeetsIt(t *testing.T) {
	t.Parallel()
	tree, gateTools := planFixture(t)
	tools := Tools{Runner: strings.Repeat("d", 64), Go: "go1.27.0"}
	plan := func(needs string) (map[string]PlannedResult, error) {
		writeFiles(t, gateTools, map[string]string{"cloud/fast-gate/unit-needs.json": needs})
		results, err := PlanTree(tree, gateTools, tools, MemoryIndex{}, false)
		byName := map[string]PlannedResult{}
		for _, result := range results {
			byName[result.Name] = result
		}
		return byName, err
	}
	tiers := `"tiers": [{"name": "codex-strict", "memoryMegabytes": 16384, "cpus": 4}, {"name": "box", "memoryMegabytes": 65536, "cpus": 32}]`
	none, err := plan(`{"version": 1, ` + tiers + `, "units": []}`)
	if err != nil {
		t.Fatal(err)
	}
	declared, err := plan(`{"version": 1, ` + tiers + `, "units": [{"package": "a", "memoryMegabytes": 20480, "cpus": 4, "record": "gate-logs/x/fast"}]}`)
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
	if _, err := plan(`{"version": 1, ` + tiers + `, "units": [{"package": "a", "memoryMegabytes": 131072, "record": "gate-logs/x/fast"}]}`); err == nil {
		t.Error("a need no tier meets was planned")
	}
	if _, err := plan(`{"version": 1, ` + tiers + `, "units": [{"package": "a", "memoryMegabytes": 20480}]}`); err == nil {
		t.Error("a need without the record that measured it was planned")
	}
	// Queue refuses a unit's resources without both keys, so a need without cpus would refuse the whole plan there.
	if _, err := plan(`{"version": 1, ` + tiers + `, "units": [{"package": "a", "memoryMegabytes": 20480, "record": "gate-logs/x/fast"}]}`); err == nil {
		t.Error("a need without cpus was planned")
	}
	encoded, _ := json.Marshal(a.Resources)
	if string(encoded) != `{"cpus":4,"memoryMegabytes":20480}` {
		t.Errorf("a's resources encode as %s, not Queue's two keys", encoded)
	}
}
