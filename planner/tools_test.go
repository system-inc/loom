package planner

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

// The Go release a gofmt key holds is the one go resolves inside the tree under GOTOOLCHAIN=auto, as adamic's setup
// leaves it: asked in the tree, with auto outranking the planner's own GOTOOLCHAIN. A go that can't say fails the plan.
// Mutants: go asked outside the tree, or under the planner's GOTOOLCHAIN.
func TestTreeGoVersionIsAskedInTheTree(t *testing.T) {
	bin, tree := t.TempDir(), filepath.Join(t.TempDir(), "tree-x")
	os.MkdirAll(tree, 0o755)
	os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\n[ \"$*\" = \"env GOVERSION\" ] || exit 2\necho \"go1.99.9-$(basename \"$(/bin/pwd -P)\")-${GOTOOLCHAIN}\"\n"), 0o755)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	t.Setenv("GOTOOLCHAIN", "local")
	if version, err := TreeGoVersion(tree); err != nil || version != "go1.99.9-tree-x-auto" {
		t.Fatalf("the tree's Go release is %q (%v), want go1.99.9-tree-x-auto", version, err)
	}
	os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\necho 'go: no toolchain' >&2\nexit 1\n"), 0o755)
	if _, err := TreeGoVersion(tree); err == nil || !strings.Contains(err.Error(), "no toolchain") {
		t.Fatalf("a go that can't say its release gave %v", err)
	}
}

// A go that doesn't answer (a toolchain fetch from a proxy it can't reach) is killed at the bound, with everything it
// started, and the tree is refused naming itself and the bound, so planning never stalls on it. Mutants: no bound, and
// only go killed, not its group.
func TestTreeGoVersionIsBounded(t *testing.T) {
	bin, tree := t.TempDir(), t.TempDir()
	child := filepath.Join(bin, "child")
	os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\nsleep 30 &\necho $! > \""+child+"\"\nwait\n"), 0o755)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	started := time.Now()
	_, err := treeGoVersion(tree, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), tree) || !strings.Contains(err.Error(), "within 300ms") || time.Since(started) > 4*time.Second {
		t.Fatalf("a go that never answers gave %v after %v, want the tree and the bound named at once", err, time.Since(started))
	}
	content, _ := os.ReadFile(child)
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatalf("the stub go's child: %q", content)
	}
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("what go started outlived the bound")
		}
	}
	if TreeGoVersionBound < time.Minute || TreeGoVersionBound > 10*time.Minute {
		t.Errorf("the bound is %v, want a few minutes", TreeGoVersionBound)
	}
}
