package planner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A tree's identity is read where its builds run: go env inside the tree, under the process's own environment (so
// Workshop's GOTOOLCHAIN=local decides the release, as it does for build-tree's go test -c), and the commit's tree
// hash; a tree with changes to tracked files has none. Mutants: go asked outside the tree; GOTOOLCHAIN forced to auto
// as the gofmt key's release is; the dirty check dropped.
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
		"echo \"go1.99.9-$(basename \"$(/bin/pwd -P)\")-${GOTOOLCHAIN}\"\necho linux\necho amd64\n"), 0o755)
	t.Setenv("PATH", bin+":"+filepath.Dir(git)+":/usr/bin:/bin")
	t.Setenv("GOTOOLCHAIN", "local")
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
