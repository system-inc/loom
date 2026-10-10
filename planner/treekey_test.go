package planner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A tree's identity is read where its builds run: go env inside the tree, under GOTOOLCHAIN=local and the runners'
// GOOS/GOARCH whatever the process's environment says (review of tree-wiring, finding 4), and the commit's tree hash; a
// tree with changes to tracked files has none. Mutants: go asked outside the tree; GOTOOLCHAIN forced to auto as the
// gofmt key's release is; the pinned environment dropped; the dirty check dropped.
func TestATreesIdentityIsReadInsideItUnderItsBuildsEnvironment(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	tree := filepath.Join(t.TempDir(), "tree-x")
	writeFiles(t, tree, map[string]string{"go.mod": "module example.com/x\n"})
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	hash, err := exec.Command("git", "-C", tree, "rev-parse", "HEAD^{tree}").Output()
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\n[ \"$*\" = \"env GOVERSION GOOS GOARCH\" ] || exit 2\n"+
		"echo \"go1.99.9-$(basename \"$(/bin/pwd -P)\")-${GOTOOLCHAIN}\"\necho ${GOOS:-host}\necho ${GOARCH:-host}\n"), 0o755)
	t.Setenv("PATH", bin+":"+filepath.Dir(git)+":/usr/bin:/bin")
	// Whatever the process's environment says, the key reads the local go building for the runners' target.
	t.Setenv("GOTOOLCHAIN", "auto")
	t.Setenv("GOOS", "darwin")
	t.Setenv("GOARCH", "arm64")
	identity, err := ReadTreeIdentity(tree)
	if err != nil {
		t.Fatal(err)
	}
	want := TreeIdentity{Tree: strings.TrimSpace(string(hash)), Go: "go1.99.9-tree-x-local", Goos: "linux", Goarch: "amd64"}
	if identity != want {
		t.Fatalf("the identity is %+v, want %+v", identity, want)
	}
	if identity.Key() != TreeKey(want.Tree, want.Go, "linux", "amd64") || identity.Key() == TreeKey(want.Tree, want.Go, "darwin", "arm64") {
		t.Fatal("the key isn't the identity's, platform included")
	}
	os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module example.com/changed\n"), 0o644)
	if _, err := ReadTreeIdentity(tree); err == nil || !strings.Contains(err.Error(), "changes to tracked files") {
		t.Fatalf("a tree with a tracked change keyed (%v)", err)
	}
}

// The Go release is the tree's: a tree naming a toolchain other than the local go's release is refused at plan time,
// naming both and the file; go.work's line rules a workspace; a tree naming none is the local go's. Mutants: the
// toolchain not compared; go.mod read before go.work.
func TestATreeNamingAnotherToolchainIsRefused(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\necho go1.27.1\necho linux\necho amd64\n"), 0o755)
	t.Setenv("PATH", bin+":"+filepath.Dir(git)+":/usr/bin:/bin")
	for name, test := range map[string]struct {
		files map[string]string
		want  string
	}{
		"go.mod's own":            {map[string]string{"go.mod": "module m\n\ngo 1.27\n\ntoolchain go1.27.1\n"}, ""},
		"none":                    {map[string]string{"go.mod": "module m\n\ngo 1.27\n"}, ""},
		"go.mod's another":        {map[string]string{"go.mod": "module m\n\ngo 1.27\n\ntoolchain go1.27.2 // pinned\n"}, "go.mod names toolchain go1.27.2, and this go is go1.27.1"},
		"go.work's over go.mod's": {map[string]string{"go.mod": "module m\n\ntoolchain go1.27.1\n", "go.work": "go 1.27\n\ntoolchain go1.28.0\n\nuse .\n"}, "go.work names toolchain go1.28.0"},
	} {
		tree := filepath.Join(t.TempDir(), "tree")
		writeFiles(t, tree, test.files)
		for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "x"}} {
			if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", arguments, err, output)
			}
		}
		identity, err := ReadTreeIdentity(tree)
		switch {
		case test.want == "" && (err != nil || identity.Go != "go1.27.1"):
			t.Errorf("%s: %+v %v", name, identity, err)
		case test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)):
			t.Errorf("%s: %v, want %q", name, err, test.want)
		}
	}
}

// A commit is keyed as its checkout is, from the repository's objects alone (Judge's rerun on a unit's base,
// #v03v751): the same identity for a commit behind HEAD as ReadTreeIdentity reads in a checkout of it, the same
// toolchain refusals, and a commit the repository doesn't hold named. Mutants: go.work not carried (a workspace's
// toolchain line unread); HEAD's tree hash read for every commit.
func TestACommitIsKeyedAsItsCheckoutIs(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\necho go1.27.1\necho linux\necho amd64\n"), 0o755)
	t.Setenv("PATH", bin+":"+filepath.Dir(git)+":/usr/bin:/bin")
	commit := func(tree string, files map[string]string) string {
		writeFiles(t, tree, files)
		for _, arguments := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "x"}} {
			if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v %s", arguments, err, output)
			}
		}
		sha, _ := exec.Command("git", "-C", tree, "rev-parse", "HEAD").Output()
		return strings.TrimSpace(string(sha))
	}
	for name, files := range map[string]map[string]string{
		"none":                    {"go.mod": "module m\n\ngo 1.27\n", "a.go": "package m\n"},
		"go.mod's another":        {"go.mod": "module m\n\ngo 1.27\n\ntoolchain go1.27.2 // pinned\n"},
		"go.work's over go.mod's": {"go.mod": "module m\n\ntoolchain go1.27.1\n", "go.work": "go 1.27\n\ntoolchain go1.28.0\n\nuse .\n"},
		"no module at all":        {"README": "x\n"},
	} {
		repository := filepath.Join(t.TempDir(), "repository")
		os.MkdirAll(repository, 0o755)
		exec.Command("git", "-C", repository, "init", "-q").Run()
		base := commit(repository, files)
		// HEAD moves on: what's keyed is the commit named, not the checkout's.
		commit(repository, map[string]string{"later.txt": "later\n"})
		checkout := filepath.Join(t.TempDir(), "checkout")
		if output, err := exec.Command("git", "clone", "-q", repository, checkout).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", name, err, output)
		}
		exec.Command("git", "-C", checkout, "checkout", "-q", base).Run()
		want, wantErr := ReadTreeIdentity(checkout)
		got, err := ReadCommitIdentity(repository, base)
		if got != want || (err == nil) != (wantErr == nil) || (err != nil && err.Error() != wantErr.Error()) {
			t.Errorf("%s: the commit keys %+v (%v), and its checkout %+v (%v)", name, got, err, want, wantErr)
		}
	}
	repository := filepath.Join(t.TempDir(), "repository")
	os.MkdirAll(repository, 0o755)
	exec.Command("git", "-C", repository, "init", "-q").Run()
	commit(repository, map[string]string{"go.mod": "module m\n"})
	if _, err := ReadCommitIdentity(repository, strings.Repeat("d", 40)); err == nil || !strings.Contains(err.Error(), "isn't in "+repository) {
		t.Errorf("a commit the repository doesn't hold: %v", err)
	}
}
