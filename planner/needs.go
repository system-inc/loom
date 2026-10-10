package planner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/system-inc/loom/protocol"
)

// UnitNeeds is the gate tools' cloud/fast-gate/unit-needs.json (Loom, Oct 10 01:10Z): what a unit needs from the
// machine it's placed on, measured. A need is placement only, like Requires, never part of the key: it doesn't
// change what a passing run proves. What each pool has lives in Fabric's pool table (Pools), never here.
type UnitNeeds struct {
	Version int        `json:"version"`
	Units   []UnitNeed `json:"units"`
}

// A Pool is one pool of Fabric's table (workshop ~/.loom/pools.json, Oct 10 02:04Z), which the placer and the
// judge's reruns read too: its name on the wire, its tier, the runner its workers serve, and each worker's memory
// and cpus.
type Pool struct {
	Name            string `json:"name"`
	Tier            string `json:"tier"`
	Runner          string `json:"runner"`
	MemoryMegabytes int    `json:"memoryMegabytes"`
	Cpus            int    `json:"cpus"`
	// Kinds are the unit kinds the pool takes when it is limited to some (a box pool that runs phases): empty takes
	// tests and products.
	Kinds []string `json:"kinds,omitempty"`
	// Cold marks a pool whose workers give each test unit a Go cache private to it and empty at its start. Only a
	// cold pool decides a test unit (Release, Oct 10 02:48Z), so only a cold pool counts for a declared need.
	Cold bool `json:"cold,omitempty"`
}

// PhaseRunner is the runner a phase unit keys on: the runner of the pool that takes kind phase, never a test pool's
// (Loom, Oct 10 02:09Z). No such pool, or two serving different runners, is an error: a phase keyed on a runner no
// phase pool serves would wait unplaced.
func PhaseRunner(pools []Pool) (string, error) {
	runner := ""
	for _, pool := range pools {
		for _, kind := range pool.Kinds {
			if kind != "phase" {
				continue
			}
			if runner != "" && runner != pool.Runner {
				return "", fmt.Errorf("two phase pools serve different runners, %s and %s", short(runner), short(pool.Runner))
			}
			runner = pool.Runner
		}
	}
	if runner == "" {
		return "", fmt.Errorf("no pool in the pool table takes kind phase")
	}
	return runner, nil
}

// PoolsFile is the pool table the planner reads, ~/.loom/pools.json unless a command names another (--pools).
var PoolsFile = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".loom", "pools.json")
}()

// LoadPools reads a pool table and checks every pool names its tier and runner and has room.
func LoadPools(path string) ([]Pool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("the pool table: %w", err)
	}
	var table struct {
		Pools []Pool `json:"pools"`
	}
	if err := json.Unmarshal(content, &table); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, pool := range table.Pools {
		if pool.Name == "" || pool.Tier == "" || !Sha256Hex(pool.Runner) || pool.MemoryMegabytes <= 0 || pool.Cpus <= 0 {
			return nil, fmt.Errorf("%s: pool %+v needs a name, a tier, its runner's sha256 and positive memoryMegabytes and cpus", path, pool)
		}
	}
	return table.Pools, nil
}

// A UnitNeed is one package's measured need, with the record that measured it. Run, when set, narrows it to the unit
// whose run pattern is exactly that.
type UnitNeed struct {
	Package         string `json:"package"` // directory, repo-relative
	Run             string `json:"run,omitempty"`
	MemoryMegabytes int    `json:"memoryMegabytes"`
	Cpus            int    `json:"cpus"`
	Record          string `json:"record"`
}

// LoadUnitNeeds reads the gate tools' unit-needs.json and checks it: every need positive and measured by a named
// record. Tools without the file declare no needs.
func LoadUnitNeeds(gateTools string) (UnitNeeds, error) {
	content, err := os.ReadFile(filepath.Join(gateTools, "cloud/fast-gate/unit-needs.json"))
	if os.IsNotExist(err) {
		return UnitNeeds{}, nil
	}
	if err != nil {
		return UnitNeeds{}, err
	}
	var needs UnitNeeds
	if err := json.Unmarshal(content, &needs); err != nil {
		return UnitNeeds{}, fmt.Errorf("unit-needs.json: %w", err)
	}
	for _, need := range needs.Units {
		// Queue takes a unit's resources only as both keys, each a positive whole number (18e6bb6).
		if need.Package == "" || need.Record == "" || need.MemoryMegabytes <= 0 || need.Cpus <= 0 {
			return UnitNeeds{}, fmt.Errorf("unit-needs.json: unit %+v needs a package, positive memoryMegabytes and cpus, and the record that measured them", need)
		}
	}
	return needs, nil
}

// For is a unit's resources (nil: no declared need), refused when no cold pool serving the unit's runner can hold it,
// so the unit is never planned to wait unplaced: a test unit places only on cold pools. An empty runner (a product's
// key) may go to any cold pool.
func (needs UnitNeeds) For(directory, run string, pools []Pool, runner string) (*protocol.Resources, error) {
	resources := needs.Need(directory, run)
	if resources == nil {
		return nil, nil
	}
	for _, pool := range pools {
		if pool.Cold && (runner == "" || pool.Runner == runner) && pool.MemoryMegabytes >= resources.MemoryMegabytes && pool.Cpus >= resources.Cpus {
			return resources, nil
		}
	}
	return nil, fmt.Errorf("%s needs %d MB and %d cpus, which no cold pool serving runner %s holds", directory, resources.MemoryMegabytes, resources.Cpus, short(runner))
}

// Need is a unit's declared need (nil: none), the largest of the entries for its directory and run pattern, with no
// check of what any pool has: the judge's reruns place it against the pool table themselves.
func (needs UnitNeeds) Need(directory, run string) *protocol.Resources {
	var resources *protocol.Resources
	for _, need := range needs.Units {
		if need.Package != directory || need.Run != "" && need.Run != run {
			continue
		}
		if resources == nil {
			resources = &protocol.Resources{}
		}
		resources.MemoryMegabytes = max(resources.MemoryMegabytes, need.MemoryMegabytes)
		resources.Cpus = max(resources.Cpus, need.Cpus)
	}
	return resources
}
