package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/treebuilder"
)

// A rerun runs a tree build (#v03v751, #6ygdzat): the plan's on the future, and on the base the base commit's, keyed
// from the clone as the planner keys a tree. A base tree the store lacks is asked of the tree builder once, and a
// rerun on it waits, nothing rerun, until its index is up; it's void, named, never red, when the builder's build of it
// stands failed, when the index isn't up within the wait of the first request (counted across a judge's restart), or
// when the judge can't key it or asks the builder for nothing. Mutants: the index left unread; no wait; the wait
// counted from each ask (a request not kept); a failed build waited on.
func TestARerunOnABaseWaitsForItsTreeToBeBuilt(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\necho go1.27.2\necho linux\necho amd64\n"), 0o755)
	t.Setenv("PATH", bin+":"+filepath.Dir(git)+":/usr/bin:/bin")
	clone := t.TempDir()
	commit := func(content string) string {
		os.WriteFile(filepath.Join(clone, "go.mod"), []byte(content), 0o644)
		for _, arguments := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
			if output, err := exec.Command("git", append([]string{"-C", clone}, arguments...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", arguments, err, output)
			}
		}
		sha, _ := exec.Command("git", "-C", clone, "rev-parse", "HEAD").Output()
		return strings.TrimSpace(string(sha))
	}
	exec.Command("git", "-C", clone, "init", "-q").Run()
	base := commit("module m\n")
	commit("module m // later\n")
	hash, _ := exec.Command("git", "-C", clone, "rev-parse", base+"^{tree}").Output()
	want := planner.TreeKey(strings.TrimSpace(string(hash)), "go1.27.2", "linux", "amd64")

	held, ledger := map[string]bool{}, map[string]treebuilder.Record{}
	now := time.Date(2026, 10, 10, 20, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "requests.jsonl")
	open := func() *baseTrees {
		requests, err := treebuilder.OpenRequests(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { requests.Close() })
		return &baseTrees{git: clone, requests: requests, wait: 30 * time.Minute, now: func() time.Time { return now }, keyed: map[string]planner.TreeIdentity{},
			indexed: func(tree string) (bool, error) { return held[tree], nil },
			newest: func(tree string) (treebuilder.Record, bool, error) {
				record, found := ledger[tree]
				return record, found, nil
			}}
	}
	trees := open()
	asked := func() []treebuilder.Request {
		requests, err := treebuilder.ReadRequests(path, 0, now)
		if err != nil {
			t.Fatal(err)
		}
		return requests
	}

	planned := strings.Repeat("7", 64)
	if tree, void, err := trees.rerunTree(strings.Repeat("c", 40), planned); tree != planned || void != "" || err != nil || len(asked()) != 0 {
		t.Fatalf("the future's rerun: %q %q %v, asked %v", tree, void, err, asked())
	}
	// Not built: asked for once, and waited on.
	if err := trees.ready(base); !errors.Is(err, judge.ErrWaiting) || !strings.Contains(err.Error(), want) {
		t.Fatalf("a base whose tree isn't up: %v", err)
	}
	now = now.Add(20 * time.Minute)
	if _, void, err := trees.rerunTree(base, ""); !errors.Is(err, judge.ErrWaiting) || void != "" {
		t.Fatalf("a base whose tree is being built, 20 minutes on: %q %v", void, err)
	}
	if requests := asked(); len(requests) != 1 || requests[0] != (treebuilder.Request{Tree: want, Commit: base, Go: "go1.27.2", At: "2026-10-10T20:00:00Z"}) {
		t.Fatalf("asked %+v, want one request for base %s's tree %s", requests, base, want)
	}
	// A judge restarted counts the wait from the first request.
	trees.requests.Close()
	trees = open()
	now = now.Add(11 * time.Minute)
	if err := trees.ready(base); err != nil {
		t.Fatalf("31 minutes after the request, across a restart, the rerun still waits: %v", err)
	}
	if tree, void, err := trees.rerunTree(base, ""); tree != "" || err != nil || !strings.Contains(void, "base tree not built: trees/"+want+".json, base "+base+"'s, isn't in the store 30m0s after the judge asked for it (the tree builder has no record of it)") {
		t.Fatalf("past the wait: %q %q %v", tree, void, err)
	}
	// Built: the rerun runs it.
	held[want] = true
	if err := trees.ready(base); err != nil {
		t.Fatal(err)
	}
	if tree, void, err := trees.rerunTree(base, ""); tree != want || void != "" || err != nil {
		t.Fatalf("a base whose tree is up: %q %q %v, want %s", tree, void, err, want)
	}
	// The builder's build of it failed, and that stands: void at once, never waited on.
	held[want] = false
	ledger[want] = treebuilder.Record{Tree: want, Future: base, Event: treebuilder.Failed, At: "2026-10-10T20:05:00Z", Retry: "2026-10-10T23:00:00Z", Cause: "build-tree: exit status 1"}
	now = time.Date(2026, 10, 10, 20, 10, 0, 0, time.UTC)
	if _, void, err := trees.rerunTree(base, ""); err != nil || !strings.Contains(void, "wasn't built on Workshop (the tree builder's newest record: failed at 2026-10-10T20:05:00Z, tried again at 2026-10-10T23:00:00Z: build-tree: exit status 1)") {
		t.Fatalf("a failed build: %q %v", void, err)
	}
	for name, test := range map[string]struct {
		trees *baseTrees
		sha   string
		says  string
	}{
		"no --tree-git":            {&baseTrees{}, base, "base tree not built: the judge has no --tree-git"},
		"a commit the clone lacks": {trees, strings.Repeat("d", 40), "base tree not built: keying base " + strings.Repeat("d", 40)},
		"no --tree-requests": {&baseTrees{git: clone, wait: time.Hour, now: time.Now, keyed: map[string]planner.TreeIdentity{},
			indexed: func(string) (bool, error) { return false, nil }, newest: func(string) (treebuilder.Record, bool, error) { return treebuilder.Record{}, false, nil }},
			base, "and the judge asks the builder for nothing (no --tree-requests)"},
	} {
		if tree, void, err := test.trees.rerunTree(test.sha, ""); tree != "" || err != nil || !strings.Contains(void, test.says) {
			t.Errorf("%s: %q %q %v", name, tree, void, err)
		}
	}
	trees.indexed = func(string) (bool, error) { return false, errors.New("R2 answered 500") }
	if err := trees.ready(base); err == nil || errors.Is(err, judge.ErrWaiting) || !strings.Contains(err.Error(), "R2 answered 500") {
		t.Errorf("a store that can't be read is an error for the pass, never a wait or a void: %v", err)
	}
}
