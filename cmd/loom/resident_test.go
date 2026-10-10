package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/resident"
	"github.com/system-inc/loom/treebuilder"
)

// With a resident, the tree builder keys each checked-out tree before its build and hands build-tree the keys in a
// file beside the build's log, for that tree; a tree the resident can't key builds cold, with no keys, rather than
// failing. Mutant: the keys left off the child's command line.
func TestABuildWithAResidentHandsTheChildItsTreesKeys(t *testing.T) {
	repository := t.TempDir()
	for name, text := range map[string]string{
		"go.mod":      "module example.com/warm\n\ngo 1.22\n",
		"a/a.go":      "package a\n\nconst A = 1\n",
		"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(test *testing.T) {}\n",
	} {
		file := filepath.Join(repository, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=loom", "-c", "user.email=loom@example.com", "commit", "-q", "-m", "warm"}} {
		if output, err := exec.Command("git", append([]string{"-C", repository}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	head, err := planner.LocalGit(repository, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.TrimSpace(string(head))
	hash, err := planner.TreeHash(repository)
	if err != nil {
		t.Fatal(err)
	}
	logs := t.TempDir()
	settings, err := parseBuildTreesFlags([]string{"--queue", "https://queue", "--token-file", "token", "--logs", logs, "--resident"}, &bytes.Buffer{})
	if err != nil || !*settings.resident {
		t.Fatalf("--resident: %v", err)
	}
	// The child records its command line.
	child := filepath.Join(t.TempDir(), "build-tree")
	recorded := filepath.Join(t.TempDir(), "arguments")
	if err := os.WriteFile(child, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+recorded+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := treebuilder.Want{Tree: strings.Repeat("c", 64), Future: commit, Go: "go1.27.1"}
	checkout := func(string) (string, func(), error) { return repository, func() {}, nil }
	if err := buildWant(context.Background(), checkout, child, settings, want, resident.New(), func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	arguments, err := os.ReadFile(recorded)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(arguments)), "\n")
	at := slices.Index(lines, "--keys")
	if at < 0 || at+1 >= len(lines) || lines[at+1] != filepath.Join(logs, want.Tree+".keys.json") {
		t.Fatalf("the child ran with %q, no keys beside its log", lines)
	}
	keys, err := resident.ReadKeys(lines[at+1], hash)
	if err != nil {
		t.Fatal(err)
	}
	if keys.Commit != commit || len(keys.Packages) != 1 || keys.Closures["example.com/warm/a"].Key == "" {
		t.Fatalf("the keys handed over: %+v", keys)
	}
	// A commit the checkout isn't at can't be keyed: the build goes on cold, without keys.
	want.Future = strings.Repeat("3", 40)
	if err := buildWant(context.Background(), checkout, child, settings, want, resident.New(), func(int) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if arguments, err = os.ReadFile(recorded); err != nil || slices.Contains(strings.Split(string(arguments), "\n"), "--keys") {
		t.Fatalf("a tree the resident couldn't key ran with %q (%v)", arguments, err)
	}
}
