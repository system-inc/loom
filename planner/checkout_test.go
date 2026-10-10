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
		"git@github.com:system-inc/cohere.git": "https://github.com/system-inc/cohere.git", "ssh://git@github.com/system-inc/tsc.git": "https://github.com/system-inc/tsc.git"} {
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

// An ssh URL off GitHub, the repository's or a submodule's from the change's own .gitmodules, is refused as a transport
// a keyless git doesn't take, and ssh never runs: an agent or a key on the machine is never offered, and nothing only
// a key can read reaches the published source (review of tree-wiring, finding 3). Mutant: ssh among the transports.
func TestAKeylessCheckoutNeverRunsSsh(t *testing.T) {
	root := t.TempDir()
	bin, marker := filepath.Join(root, "bin"), filepath.Join(root, "ssh-ran")
	writeFiles(t, bin, map[string]string{"ssh": "#!/bin/sh\necho \"$@\" > " + marker + "\nexit 1\n"})
	os.Chmod(filepath.Join(bin, "ssh"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("GIT_SSH_COMMAND", "ssh")
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(root, "agent"))
	for _, url := range []string{"ssh://git@example.invalid/x.git", "git@example.invalid:y.git"} {
		output, err := KeylessGit("ls-remote", url).CombinedOutput()
		if err == nil || !strings.Contains(string(output), "transport 'ssh' not allowed") {
			t.Errorf("ls-remote %s: %v %s", url, err, output)
		}
	}
	git := func(directory string, arguments ...string) string {
		t.Helper()
		output, err := exec.Command("git", append([]string{"-C", directory, "-c", "user.name=t", "-c", "user.email=t@t"}, arguments...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	origin, clone := filepath.Join(root, "origin"), filepath.Join(root, "clone")
	writeFiles(t, origin, map[string]string{"go.mod": "module example.com/origin\n"})
	git(root, "init", "-q", origin)
	git(origin, "add", ".")
	git(origin, "commit", "-q", "-m", "first")
	first := git(origin, "rev-parse", "HEAD")
	for name, url := range map[string]string{"x": "ssh://git@example.invalid/x.git", "y": "git@example.invalid:y.git"} {
		git(origin, "config", "-f", ".gitmodules", "submodule."+name+".path", name)
		git(origin, "config", "-f", ".gitmodules", "submodule."+name+".url", url)
		git(origin, "update-index", "--add", "--cacheinfo", "160000,"+first+","+name)
	}
	git(origin, "add", ".gitmodules")
	git(origin, "commit", "-q", "-m", "ssh submodules")
	second := git(origin, "rev-parse", "HEAD")
	git(root, "clone", "-q", origin, clone)
	saved := keylessProtocols
	keylessProtocols = "https:file"
	defer func() { keylessProtocols = saved }()
	if _, _, err := GitCheckout(clone)(second); err == nil || !strings.Contains(err.Error(), "transport 'ssh' not allowed") {
		t.Fatalf("a checkout with ssh submodules: %v", err)
	}
	if ran, err := os.ReadFile(marker); err == nil {
		t.Fatalf("ssh ran: %s", ran)
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

	// A keyless git takes https alone; this test's origin and submodule are local paths.
	saved := keylessProtocols
	keylessProtocols = "https:file"
	defer func() { keylessProtocols = saved }()
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
