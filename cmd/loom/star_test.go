package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/system-inc/loom/coordinator"
)

func TestLoomTakesNothingOnABoxWhereTheStarRuns(t *testing.T) {
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "running"), 0o755)
	os.WriteFile(filepath.Join(state, "front"), []byte("cloud/land-*views-slice* # the star\n"), 0o644)
	os.WriteFile(filepath.Join(state, "slots"), []byte("server B\n"), 0o644)
	slots := []coordinator.Machine{coordinator.SSHMachine{Box: "workshop", Class: "S"}, coordinator.SSHMachine{Box: "server", Class: "S"}}
	limit := yieldLimit(slots, filepath.Join(state, "slots"))
	if limit("workshop") != 1 || limit("server") != 1 {
		t.Fatalf("before the star: workshop %d, server %d", limit("workshop"), limit("server"))
	}
	os.WriteFile(filepath.Join(state, "running", "42"), []byte("cloud/land-train-1-views-slice1-64c59784 9f16421c S workshop B tools log\n"), 0o644)
	os.WriteFile(filepath.Join(state, "running", "43"), []byte("codex/other abcdef S server S tools log\n"), 0o644)
	if limit("workshop") != 0 || limit("server") != 1 {
		t.Fatalf("with the star on workshop: workshop %d, server %d", limit("workshop"), limit("server"))
	}
}
