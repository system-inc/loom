package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/treebuilder"
)

// The shipped unit runs `loom build-trees` with flags it takes, under adamic's toolchain and GOTOOLCHAIN=local (the
// environment a tree's key is read in, the planner's too), and with the default KillMode, so a restart stops the build
// in flight with the builder rather than leaving it unrecorded. Mutants: GOTOOLCHAIN=local or env.sh taken out;
// KillMode=process.
func TestTheShippedTreeBuilderUnitBuildsUnderAdamicsToolchain(t *testing.T) {
	unit, err := os.ReadFile("../../treebuilder/systemd/loom-build-trees.service")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(unit, []byte("\nEnvironment=GOTOOLCHAIN=local\n")) || !bytes.Contains(unit, []byte("'source %h/adamic-tools/env.sh && exec ")) {
		t.Fatal("the unit doesn't build under adamic's toolchain with GOTOOLCHAIN=local")
	}
	if bytes.Contains(unit, []byte("\nKillMode=")) {
		t.Fatal("the unit sets a KillMode: a restart must take the build in flight with the builder")
	}
	arguments, err := buildTreesUnitCommand(unit, "/home/loom")
	if err != nil {
		t.Fatal(err)
	}
	settings, err := parseBuildTreesFlags(arguments, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("loom build-trees %v: %v", arguments, err)
	}
	if *settings.ledgerPath != "/home/loom/loom-trees/trees.jsonl" || *settings.clone != "/home/loom/loom-trees/adamic" || *settings.floorGB != 200 || *settings.once {
		t.Fatalf("the unit's builder is %v", arguments)
	}
}

// buildTreesUnitCommand is the shipped unit's `loom build-trees` arguments, from inside its bash -c, with %h made home.
func buildTreesUnitCommand(unit []byte, home string) ([]string, error) {
	for _, line := range strings.Split(string(unit), "\n") {
		command, found := strings.CutPrefix(line, "ExecStart=/bin/bash -c '")
		if !found {
			continue
		}
		_, command, _ = strings.Cut(strings.TrimSuffix(command, "'"), "&& exec ")
		words := strings.Fields(strings.ReplaceAll(command, "%h", home))
		if len(words) < 2 || filepath.Base(words[0]) != "loom" || words[1] != "build-trees" {
			return nil, fmt.Errorf("ExecStart %q isn't loom build-trees", command)
		}
		return words[2:], nil
	}
	return nil, fmt.Errorf("no ExecStart=/bin/bash -c")
}

// A build-tree is run with the plan's tree key and the future's commit, its output in a log of the builder's alone; one
// that ends badly says so with the log's last lines, and one past the bound is killed with everything it started.
// Mutants: no bound; only the child killed, not its group.
func TestABuildTreeChildIsBoundedAndSaysHowItEnded(t *testing.T) {
	directory := t.TempDir()
	settings, err := parseBuildTreesFlags([]string{"--queue", "https://queue", "--token-file", "token"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := treebuilder.Want{Tree: strings.Repeat("b", 64), Future: strings.Repeat("2", 40)}
	arguments := buildTreeArguments(settings, "/trees/adamic", want)
	if arguments[0] != "build-tree" || !slices.Contains(arguments, "--tree-key") || arguments[slices.Index(arguments, "--tree-key")+1] != want.Tree ||
		arguments[slices.Index(arguments, "--future")+1] != want.Future || arguments[slices.Index(arguments, "--tree")+1] != "/trees/adamic" {
		t.Fatalf("build-tree's arguments: %v", arguments)
	}

	failing := filepath.Join(directory, "failing")
	os.WriteFile(failing, []byte("#!/bin/bash\necho \"build-tree: the plan carries tree key $3\"\nexit 1\n"), 0o755)
	log := filepath.Join(directory, "b.log")
	err = runBuildTree(context.Background(), failing, []string{"--tree-key", want.Tree}, log, time.Minute)
	if err == nil || !strings.Contains(err.Error(), "exit status 1") || !strings.Contains(err.Error(), "the plan carries tree key") {
		t.Fatalf("a failing build-tree: %v", err)
	}
	if info, err := os.Stat(log); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("its log: %v %v", info.Mode(), err)
	}

	child := filepath.Join(directory, "child")
	hanging := filepath.Join(directory, "hanging")
	os.WriteFile(hanging, []byte("#!/bin/bash\nsleep 30 &\necho $! > \""+child+"\"\nwait\n"), 0o755)
	started := time.Now()
	err = runBuildTree(context.Background(), hanging, nil, log, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "past its 300ms bound") || time.Since(started) > 5*time.Second {
		t.Fatalf("a build-tree that never ends gave %v after %v", err, time.Since(started))
	}
	content, _ := os.ReadFile(child)
	pid, err := strconv.Atoi(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatalf("the stub's child: %q", content)
	}
	for deadline := time.Now().Add(3 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			syscall.Kill(pid, syscall.SIGKILL)
			t.Fatal("what build-tree started outlived the bound")
		}
	}
}

// The builder's clone is made when missing with the public repository as its origin, and one whose origin is anything
// else is refused: every tree comes from what that repository serves anyone. Mutant: the origin not checked.
func TestTheBuildersCloneHasOnlyThePublicOrigin(t *testing.T) {
	clone := filepath.Join(t.TempDir(), "adamic")
	if err := readyClone(clone); err != nil {
		t.Fatal(err)
	}
	if origin, err := exec.Command("git", "-C", clone, "config", "--get", "remote.origin.url").Output(); err != nil || strings.TrimSpace(string(origin)) != protocol.AdamicRepository {
		t.Fatalf("the clone's origin is %q (%v)", origin, err)
	}
	if err := readyClone(clone); err != nil {
		t.Fatalf("an existing clone: %v", err)
	}
	exec.Command("git", "-C", clone, "remote", "set-url", "origin", "https://github.com/someone/adamic").Run()
	if err := readyClone(clone); err == nil || !strings.Contains(err.Error(), "someone/adamic") {
		t.Fatalf("a clone with another origin: %v", err)
	}
}
