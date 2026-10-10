package planner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A keyless git reads none of the user's configuration: a global rewrite that would send a fetch from GitHub over ssh
// (with the user's key) and a credential helper are both unseen, and an ssh URL is fetched over https. Mutants: the
// global configuration read; the ssh rewrite dropped.
func TestKeylessGitReadsNoUserConfiguration(t *testing.T) {
	home := t.TempDir()
	writeFiles(t, home, map[string]string{".gitconfig": "[url \"git@github.com:\"]\n\tinsteadOf = https://github.com/\n[credential]\n\thelper = store\n"})
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	clone := filepath.Join(t.TempDir(), "clone")
	if output, err := exec.Command("git", "init", "-q", clone).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, output)
	}
	for url, want := range map[string]string{"https://github.com/system-inc/adamic": "https://github.com/system-inc/adamic",
		"git@github.com:system-inc/cohere.git": "https://github.com/system-inc/cohere.git"} {
		exec.Command("git", "-C", clone, "remote", "remove", "origin").Run()
		exec.Command("git", "-C", clone, "remote", "add", "origin", url).Run()
		if got, err := KeylessGit("-C", clone, "ls-remote", "--get-url", "origin").Output(); err != nil || strings.TrimSpace(string(got)) != want {
			t.Errorf("origin %s is fetched from %q (%v), want %s", url, strings.TrimSpace(string(got)), err, want)
		}
	}
	if helper, _ := KeylessGit("-C", clone, "config", "--get-all", "credential.helper").Output(); strings.TrimSpace(string(helper)) != "" {
		t.Errorf("a keyless git sees the credential helper %q", helper)
	}
}

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
