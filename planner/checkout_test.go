package planner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// GitCheckout moves one tree between futures with its submodule at each future's recorded commit, and leaves
// nothing a previous plan made.
func TestGitCheckoutBringsEachFuturesSubmodule(t *testing.T) {
	// Local submodules are fetched over file://, which git refuses unless allowed; this is the test's own config.
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	root := t.TempDir()
	git := func(directory string, arguments ...string) string {
		t.Helper()
		command := exec.Command("git", append([]string{"-C", directory, "-c", "user.name=t", "-c", "user.email=t@t"}, arguments...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	sub, origin := filepath.Join(root, "sub"), filepath.Join(root, "origin")
	writeFiles(t, sub, map[string]string{"version.txt": "one\n"})
	git(root, "init", "-q", sub)
	git(sub, "add", ".")
	git(sub, "commit", "-q", "-m", "one")
	git(root, "init", "-q", origin)
	writeFiles(t, origin, map[string]string{"go.mod": "module example.com/origin\n"})
	git(origin, "submodule", "add", "-q", sub, "sub")
	git(origin, "add", ".")
	git(origin, "commit", "-q", "-m", "first")
	first := git(origin, "rev-parse", "HEAD")
	writeFiles(t, sub, map[string]string{"version.txt": "two\n"})
	git(sub, "commit", "-q", "-am", "two")
	git(filepath.Join(origin, "sub"), "pull", "-q", "origin", "HEAD")
	git(origin, "commit", "-q", "-am", "second")
	second := git(origin, "rev-parse", "HEAD")
	clone := filepath.Join(root, "clone")
	git(root, "clone", "-q", origin, clone)

	checkout := GitCheckout(clone)
	for _, step := range []struct{ sha, version string }{{first, "one\n"}, {second, "two\n"}, {first, "one\n"}} {
		tree, cleanup, err := checkout(step.sha)
		if err != nil {
			t.Fatal(err)
		}
		if head := git(tree, "rev-parse", "HEAD"); head != step.sha {
			t.Errorf("the tree is at %s, want %s", head, step.sha)
		}
		if version, _ := os.ReadFile(filepath.Join(tree, "sub/version.txt")); string(version) != step.version {
			t.Errorf("at %s the submodule holds %q, want %q", step.sha[:8], version, step.version)
		}
		if _, err := os.Stat(filepath.Join(tree, "leftover.txt")); err == nil {
			t.Errorf("at %s a file the previous plan left is still there", step.sha[:8])
		}
		os.WriteFile(filepath.Join(tree, "leftover.txt"), []byte("left\n"), 0o644)
		cleanup()
	}
}
