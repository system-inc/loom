package planner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The tools part's runner is the pinned sha from its file, never a guess: a file with anything but a sha256 refuses,
// and a missing file refuses, so no key holds an empty or wrong runner.
func TestProbeToolsReadsThePinnedRunnerFromItsFile(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	pin := filepath.Join(directory, "runner-pin")
	sha := strings.Repeat("8a", 32)
	os.WriteFile(pin, []byte(sha+"\n"), 0o644)
	tools, err := ProbeTools(pin)
	if err != nil || tools.Runner != sha {
		t.Fatalf("the pinned runner read as %q (%v), want %s", tools.Runner, err, sha)
	}
	os.WriteFile(pin, []byte("8a70ebce\n"), 0o644)
	if _, err := ProbeTools(pin); err == nil {
		t.Error("a short sha was keyed as the runner")
	}
	if _, err := ProbeTools(filepath.Join(directory, "missing")); err == nil {
		t.Error("a missing pin file left the runner empty instead of refusing")
	}
}
