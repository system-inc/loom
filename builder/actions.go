package builder

import (
	"sort"

	"github.com/system-inc/loom/planner"
)

// GateEnvironment is the environment a product test runs with on Workshop: the gate's switches, as a unit's, so
// what buildcache builds here under each key is what a gate unit would ask for.
func GateEnvironment() []string {
	environment := []string{}
	for name, value := range planner.GateEnvironment {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return environment
}
