package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/planner"
)

// Mutant (Loom, Oct 10 02:5xZ): the runner switch rewrites the pin while the steady planner runs. Read only at start,
// every key would stay on the old runner. Reread before each pull, the next plan keys the new one, and a pin that
// isn't a sha256 refuses the pull rather than keying anything.
func TestRepinMovesTheRunnerOnTheNextPull(t *testing.T) {
	t.Parallel()
	pin := filepath.Join(t.TempDir(), "runner-pin")
	old, switched := strings.Repeat("8a", 32), strings.Repeat("ed", 32)
	os.WriteFile(pin, []byte(old+"\n"), 0o644)
	tools, err := planner.ProbeTools(pin)
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := repin(&tools, pin, &stdout); err != nil || tools.Runner != old || stdout.Len() != 0 {
		t.Fatalf("an unchanged pin: runner %.12s, %v, %q", tools.Runner, err, stdout.String())
	}
	os.WriteFile(pin, []byte(switched+"\n"), 0o644)
	if err := repin(&tools, pin, &stdout); err != nil || tools.Runner != switched {
		t.Fatalf("after the switch the runner is %.12s (%v), want %.12s", tools.Runner, err, switched)
	}
	if !strings.Contains(stdout.String(), "runner pin moved: 8a8a8a8a8a8a to edededededed") {
		t.Errorf("the switch wasn't logged: %q", stdout.String())
	}
	os.WriteFile(pin, []byte("ed74b11d\n"), 0o644)
	if err := repin(&tools, pin, &stdout); err == nil || tools.Runner != switched {
		t.Errorf("a short pin was accepted, or moved the runner to %.12s", tools.Runner)
	}
}
