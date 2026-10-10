package gateinputs

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// What Pack refuses and what it reads past, from the review of 5d30af8. Each mutant below must make a test here fail:
//
//	a link's target checked only as written, not resolved through the links on its path:
//	TestPackRefusesALinkThatResolvesOutside
//	only a checkout's own .git/index normalized: TestPackNormalizesASubmodulesAndAWorktreesIndex
//	a file's own permissions packed, beyond the executable bit: TestPackIsBlindToUmaskOrderHardLinksAndAttributes

// review builds a small directory under umask, its files written in order, dup2 a hard link to dup1 or a copy, and
// extended attributes set where the system's xattr can set them.
func review(t *testing.T, umask int, order []string, hardLink, attributes bool) string {
	t.Helper()
	previous := syscall.Umask(umask)
	defer syscall.Umask(previous)
	directory := filepath.Join(t.TempDir(), "gate-inputs")
	files := map[string]string{"a/x.js": "x\n", "a-b/y.js": "y\n", "b/z.js": "z\n", "dup1": "same\n"}
	for _, name := range order {
		path := filepath.Join(directory, name)
		os.MkdirAll(filepath.Dir(path), 0o777)
		os.WriteFile(path, []byte(files[name]), 0o666)
	}
	if hardLink {
		if err := os.Link(filepath.Join(directory, "dup1"), filepath.Join(directory, "dup2")); err != nil {
			t.Fatal(err)
		}
	} else {
		os.WriteFile(filepath.Join(directory, "dup2"), []byte("same\n"), 0o666)
	}
	if attributes {
		exec.Command("xattr", "-w", "com.example.junk", "1", filepath.Join(directory, "a/x.js")).Run()
		exec.Command("setfattr", "-n", "user.junk", "-v", "1", filepath.Join(directory, "b/z.js")).Run()
	}
	return directory
}

// The name doesn't hear the umask the files were written under, the order they were written in, a hard link in place
// of a copy, or an extended attribute.
func TestPackIsBlindToUmaskOrderHardLinksAndAttributes(t *testing.T) {
	one, _ := pack(t, review(t, 0o022, []string{"a/x.js", "a-b/y.js", "b/z.js", "dup1"}, false, false))
	two, _ := pack(t, review(t, 0o077, []string{"dup1", "b/z.js", "a-b/y.js", "a/x.js"}, true, true))
	if one.Name != two.Name {
		t.Fatalf("the same files packed to %s and %s", one.Name, two.Name)
	}
}

// A socket is neither file, directory nor link, and is refused.
func TestPackRefusesASocket(t *testing.T) {
	directory := review(t, 0o022, []string{"a/x.js"}, false, false)
	listener, err := net.Listen("unix", filepath.Join(directory, "s.sock"))
	if err != nil {
		t.Skipf("no socket here: %v", err)
	}
	defer listener.Close()
	if _, err := Pack(directory, 4096, func(string, []byte) error { return nil }); err == nil {
		t.Fatal("a socket was packed")
	}
}

// A link whose written target stays inside, but which resolves through another link to outside, is refused; a link
// through a link that stays inside is packed.
func TestPackRefusesALinkThatResolvesOutside(t *testing.T) {
	directory := review(t, 0o022, []string{"a/x.js"}, false, false)
	os.WriteFile(filepath.Join(filepath.Dir(directory), "outside.txt"), []byte("secret\n"), 0o644)
	os.Mkdir(filepath.Join(directory, "sub"), 0o755)
	os.Symlink("..", filepath.Join(directory, "sub", "up"))
	os.Symlink("../a", filepath.Join(directory, "sub", "inside"))
	os.Symlink("sub/inside/x.js", filepath.Join(directory, "through"))
	if _, err := Pack(directory, 4096, func(string, []byte) error { return nil }); err != nil {
		t.Fatalf("links that stay inside: %v", err)
	}
	// Written, it reads as sub/outside.txt; followed, sub/up is the directory itself, and .. leaves it.
	os.Symlink("sub/up/../outside.txt", filepath.Join(directory, "escape"))
	if _, err := Pack(directory, 4096, func(string, []byte) error { return nil }); err == nil {
		t.Fatal("a link resolving outside through another link was packed")
	}
}

// git status in a submodule rewrites .git/modules/<name>/index, and in a worktree .git/worktrees/<name>/index; neither
// moves the name.
func TestPackNormalizesASubmodulesAndAWorktreesIndex(t *testing.T) {
	directory := review(t, 0o022, []string{"a/x.js"}, false, false)
	upstream := filepath.Join(t.TempDir(), "upstream")
	write(t, filepath.Join(upstream, "f.txt"), "f\n", 0o644)
	git(t, upstream, "init", "-q", "-b", "main")
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-q", "-m", "upstream")
	checkout := filepath.Join(directory, "typescript")
	write(t, filepath.Join(checkout, "g.txt"), "g\n", 0o644)
	git(t, checkout, "init", "-q", "-b", "main")
	git(t, checkout, "-c", "protocol.file.allow=always", "submodule", "add", "-q", upstream, "sub")
	git(t, checkout, "add", "-A")
	git(t, checkout, "commit", "-q", "-m", "checkout")
	git(t, checkout, "worktree", "add", "-q", "--detach", filepath.Join(directory, "worktree"))
	before, _ := pack(t, directory)
	for _, place := range []struct{ tree, index string }{
		{filepath.Join(checkout, "sub"), filepath.Join(checkout, ".git", "modules", "sub", "index")},
		{filepath.Join(directory, "worktree"), filepath.Join(checkout, ".git", "worktrees", "worktree", "index")},
	} {
		raw, _ := os.ReadFile(place.index)
		later := Epoch.AddDate(1, 0, 0)
		entries, _ := os.ReadDir(place.tree)
		for _, entry := range entries {
			os.Chtimes(filepath.Join(place.tree, entry.Name()), later, later)
		}
		git(t, place.tree, "update-index", "-q", "--refresh")
		if rewritten, _ := os.ReadFile(place.index); string(rewritten) == string(raw) {
			t.Fatalf("the refresh didn't rewrite %s, so this test proves nothing about it", place.index)
		}
	}
	if after, _ := pack(t, directory); after.Name != before.Name {
		t.Fatalf("git status in a submodule and a worktree moved the name: %.12s to %.12s", before.Name, after.Name)
	}
}
