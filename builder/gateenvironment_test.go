package builder

import (
	"testing"

	"github.com/system-inc/loom/planner"
)

func TestWorkshopRunsProductTestsUnderTheGatesEnvironment(t *testing.T) {
	_, store := serve(t)
	var seen []string
	builder := Builder{
		Store: store, Scratch: t.TempDir(), Cache: t.TempDir(), Key: func(Action) (string, error) { return keyOf("k"), nil },
		Run: func(_ Action, environment []string) ([]byte, error) {
			seen = environment
			return nil, nil
		},
	}
	builder.Build([]Action{{Directory: "x", Test: "TestProduct_X"}})
	for name, value := range planner.GateEnvironment {
		found := false
		for _, entry := range seen {
			found = found || entry == name+"="+value
		}
		if !found {
			t.Errorf("a product test ran without %s=%s: %v", name, value, seen)
		}
	}
}
