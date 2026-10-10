package planner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/system-inc/loom/protocol"
)

// UnitNeeds is the gate tools' cloud/fast-gate/unit-needs.json (Loom, Oct 10 01:10Z): what a unit needs from the
// machine it's placed on, measured, and the live tiers a placer can put it on. A need is placement only, like
// Requires, never part of the key: it doesn't change what a passing run proves.
type UnitNeeds struct {
	Version int        `json:"version"`
	Tiers   []NeedTier `json:"tiers"`
	Units   []UnitNeed `json:"units"`
}

// A NeedTier is one live placement tier and the most a unit placed on it can use.
type NeedTier struct {
	Name            string `json:"name"`
	MemoryMegabytes int    `json:"memoryMegabytes"`
	Cpus            int    `json:"cpus"`
}

// A UnitNeed is one package's measured need, with the record that measured it. Run, when set, narrows it to the unit
// whose run pattern is exactly that.
type UnitNeed struct {
	Package         string `json:"package"` // directory, repo-relative
	Run             string `json:"run,omitempty"`
	MemoryMegabytes int    `json:"memoryMegabytes,omitempty"`
	Cpus            int    `json:"cpus,omitempty"`
	Record          string `json:"record"`
}

// LoadUnitNeeds reads the gate tools' unit-needs.json and checks it: every need positive and measured by a named
// record, and at least one tier when any unit declares a need. Tools without the file declare no needs.
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
	for _, tier := range needs.Tiers {
		if tier.Name == "" || tier.MemoryMegabytes <= 0 || tier.Cpus <= 0 {
			return UnitNeeds{}, fmt.Errorf("unit-needs.json: tier %+v needs a name and positive memoryMegabytes and cpus", tier)
		}
	}
	for _, need := range needs.Units {
		if need.Package == "" || need.Record == "" || need.MemoryMegabytes < 0 || need.Cpus < 0 || need.MemoryMegabytes+need.Cpus == 0 {
			return UnitNeeds{}, fmt.Errorf("unit-needs.json: unit %+v needs a package, a positive need and the record that measured it", need)
		}
	}
	if len(needs.Units) > 0 && len(needs.Tiers) == 0 {
		return UnitNeeds{}, fmt.Errorf("unit-needs.json declares needs but no tiers to meet them")
	}
	return needs, nil
}

// For is a unit's resources (nil: no declared need), refused when no live tier can meet it, so the unit is never
// planned to wait unplaced.
func (needs UnitNeeds) For(directory, run string) (*protocol.Resources, error) {
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
	if resources == nil {
		return nil, nil
	}
	for _, tier := range needs.Tiers {
		if tier.MemoryMegabytes >= resources.MemoryMegabytes && tier.Cpus >= resources.Cpus {
			return resources, nil
		}
	}
	return nil, fmt.Errorf("%s needs %d MB and %d cpus, which no live tier meets", directory, resources.MemoryMegabytes, resources.Cpus)
}
