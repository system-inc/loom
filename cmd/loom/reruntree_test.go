package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/planner"
)

// A rerun runs a tree build (#v03v751): the plan's on the future, and on the base the base commit's, keyed from the
// clone as the planner keys a tree and run only once the store holds its index; a base whose tree isn't built is void,
// named, never red. Mutants: the base keyed from the clone's HEAD; the index left unread.
func TestARerunRunsItsCommitsTreeBuild(t *testing.T) {
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
	held := map[string]bool{}
	asked := []string{}
	indexed := func(tree string) (bool, error) {
		asked = append(asked, tree)
		return held[tree], nil
	}
	planned := strings.Repeat("7", 64)
	if tree, void, err := rerunTree(clone, indexed, strings.Repeat("c", 40), planned); tree != planned || void != "" || err != nil || len(asked) != 0 {
		t.Fatalf("the future's rerun: %q %q %v, asked %v", tree, void, err, asked)
	}
	if tree, void, err := rerunTree(clone, indexed, base, ""); tree != "" || err != nil || !strings.Contains(void, "base tree not built: trees/"+want+".json, base "+base+"'s, isn't in the store") {
		t.Fatalf("a base whose tree isn't up: %q %q %v", tree, void, err)
	}
	held[want] = true
	if tree, void, err := rerunTree(clone, indexed, base, ""); tree != want || void != "" || err != nil {
		t.Fatalf("a base whose tree is up: %q %q %v, want %s", tree, void, err, want)
	}
	for name, test := range map[string]struct {
		clone, sha string
		says       string
	}{
		"no --tree-git":            {"", base, "base tree not built: the judge has no --tree-git"},
		"a commit the clone lacks": {clone, strings.Repeat("d", 40), "base tree not built: keying base " + strings.Repeat("d", 40)},
	} {
		if tree, void, err := rerunTree(test.clone, indexed, test.sha, ""); tree != "" || err != nil || !strings.Contains(void, test.says) {
			t.Errorf("%s: %q %q %v", name, tree, void, err)
		}
	}
	failing := func(string) (bool, error) { return false, errors.New("R2 answered 500") }
	if _, void, err := rerunTree(clone, failing, base, ""); void != "" || err == nil || !strings.Contains(err.Error(), "R2 answered 500") {
		t.Errorf("a store that can't be read is an error for the pass, never a void: %q %v", void, err)
	}
}
