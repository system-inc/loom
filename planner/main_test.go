package planner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// PhasePoolRunner is the runner the tests' pool table serves phases on.
var PhasePoolRunner = strings.Repeat("9", 64)

// TestMain points PoolsFile at a pool table of the tests' own, holding a test pool and a phase pool, and NoReuseFile and
// ReadSetsDirectory at paths that don't exist, so no test reads the machine's ~/.loom/pools.json, ~/.loom/no-reuse or
// ~/.loom/read-sets. A test that needs another table or its own read sets sets them itself, and isn't parallel.
func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "planner-pools-")
	if err != nil {
		panic(err)
	}
	PoolsFile = filepath.Join(directory, "pools.json")
	NoReuseFile = filepath.Join(directory, "no-reuse")
	ReadSetsDirectory = filepath.Join(directory, "read-sets")
	table := `{"pools": [{"name": "codex-strict", "tier": "codex-strict", "runner": "` + strings.Repeat("d", 64) + `", "memoryMegabytes": 16384, "cpus": 4, "cold": true},
		{"name": "box-phase", "tier": "box-strict", "runner": "` + PhasePoolRunner + `", "memoryMegabytes": 65536, "cpus": 8, "kinds": ["phase"], "cold": true}]}`
	if err := os.WriteFile(PoolsFile, []byte(table), 0o644); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(directory)
	os.Exit(code)
}
