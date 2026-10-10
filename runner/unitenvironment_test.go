package runner

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

// A unit's tests run under planner.UnitEnvironment whatever prepare.sh's environment held, the environment Workshop
// ran the tree's product tests under, so a test asks for each product by the key it was built under (#nm31pcn: verify
// run 4's units keyed products under GOFLAGS= GOTOOLCHAIN=auto, Workshop under GOFLAGS=-p=7 GOTOOLCHAIN=local, and every
// unit that read one missed it and built it).
func TestAUnitRunsUnderTheEnvironmentWorkshopBuiltItsProductsUnder(t *testing.T) {
	directory := t.TempDir()
	prepared := filepath.Join(directory, "environment")
	// What a box's env.sh might leave: another toolchain, its own flags, no platform.
	if err := os.WriteFile(prepared, []byte("PATH=/usr/bin\x00GOTOOLCHAIN=auto\x00GOFLAGS=-mod=mod -p=3\x00ADAMIC_GATE_UNCACHED=0\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
	run := &unitRun{directory: directory}
	environment, err := run.testEnvironment(prepared, &protocol.TestJob{})
	if err != nil {
		t.Fatal(err)
	}
	for _, variable := range planner.UnitEnvironment(runtime.GOOS, runtime.GOARCH) {
		name, value, _ := strings.Cut(variable, "=")
		if got, found := environment[name]; !found || got != value {
			t.Errorf("a unit's %s is %q (set %v), and Workshop built its products under %q", name, got, found, value)
		}
	}
	if environment["PATH"] != "/usr/bin" {
		t.Errorf("PATH is %q: the unit's environment keeps what prepare.sh set outside the unit environment", environment["PATH"])
	}
}
