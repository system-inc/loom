package builder

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A tree's source carries its manifest (#cyasrr4). Each mutant below must make a test here fail:
//
//	files listed without -z: TestTheManifestIsGitsOwnOutput
//	HEAD's newline trimmed: TestTheManifestIsGitsOwnOutput
//	commit left out: TestTheManifestIsGitsOwnOutput
//	submodules not recursed into: TestTheManifestIsGitsOwnOutput
//	a submodule away from its parent's pin let through: TestASubmoduleAwayFromItsPinFailsNamed
//	a submodule that isn't checked out skipped: TestAMissingSubmoduleFailsNamed
//	git run with the environment's GIT_DIR: TestTheManifestIsTheSameForTheSameTree
//	the manifest's files chunked with their neighbors: TestTheManifestsChunksMoveOnlyWithTheirRepository
//	a tracked path under .tracked let through: TestATreeTrackingTheManifestsDirectoryIsRefused

// gitIn runs git in directory and returns what it printed.
func gitIn(t *testing.T, directory string, arguments ...string) []byte {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "protocol.file.allow=always", "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, arguments...)...)
	command.Dir = directory
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			t.Fatalf("git %v in %s: %v %s", arguments, directory, err, exit.Stderr)
		}
		t.Fatalf("git %v in %s: %v", arguments, directory, err)
	}
	return output
}

// manifestRepositories are the repositories of trackedTree, as the manifest names them: the tree, its submodule, and
// the submodule's own, as adamic holds cohere and cohere holds TypeScript.
var manifestRepositories = []string{"", "sub", "sub/deep/inner"}

// trackedTree is a tree with a submodule that has a submodule of its own, every one checked out.
func trackedTree(t *testing.T) string {
	t.Helper()
	inner := gitTree(t, map[string]string{"inner.txt": "inner\n", "b/c.txt": "c\n"})
	sub := gitTree(t, map[string]string{"sub.txt": "sub\n", "x/y.txt": "y\n"})
	gitIn(t, sub, "submodule", "add", "-q", inner, "deep/inner")
	gitCommit(t, sub)
	top := gitTree(t, map[string]string{"top.txt": "top\n", "a/b.txt": "b\n", "z.txt": "z\n"})
	os.Symlink("top.txt", filepath.Join(top, "link"))
	gitIn(t, top, "submodule", "add", "-q", sub, "sub")
	gitCommit(t, top)
	gitIn(t, top, "submodule", "update", "-q", "--init", "--recursive")
	return top
}

// unpackSource unpacks every chunk of source into a new directory.
func unpackSource(t *testing.T, source Source) string {
	t.Helper()
	directory := t.TempDir()
	for _, chunk := range source.Chunks {
		if err := UnpackChunk(bytes.NewReader(source.Blobs[chunk.Blob]), directory, chunk); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// The manifest is git's own output, byte for byte, for the tree and each submodule, recursively: rev-parse HEAD,
// cat-file commit HEAD and ls-tree -r -z --full-tree HEAD, run in that repository. Each submodule's HEAD is the commit
// its parent's files pin. It is in the source, each file a chunk of its own, and never in the checkout.
func TestTheManifestIsGitsOwnOutput(t *testing.T) {
	top := trackedTree(t)
	source, err := SourceChunks(top)
	if err != nil {
		t.Fatal(err)
	}
	unpacked := unpackSource(t, source)
	commands := map[string][]string{"HEAD": {"rev-parse", "HEAD"}, "commit": {"cat-file", "commit", "HEAD"}, "files": {"ls-tree", "-r", "-z", "--full-tree", "HEAD"}}
	want := []string{}
	for _, repository := range manifestRepositories {
		for file, arguments := range commands {
			name := path.Join(TrackedDirectory, repository, file)
			want = append(want, name)
			got, err := os.ReadFile(filepath.Join(unpacked, filepath.FromSlash(name)))
			if err != nil {
				t.Fatal(err)
			}
			if expected := gitIn(t, filepath.Join(top, filepath.FromSlash(repository)), arguments...); len(expected) == 0 || !bytes.Equal(got, expected) {
				t.Errorf("%s is %q, and git %s prints %q", name, got[:min(len(got), 200)], strings.Join(arguments, " "), expected[:min(len(expected), 200)])
			}
		}
		if repository == "" {
			continue
		}
		// The submodule's HEAD is its parent's gitlink.
		parent, inside := path.Dir(repository), path.Base(repository)
		if repository == "sub/deep/inner" {
			parent, inside = "sub", "deep/inner"
		}
		if parent == "." {
			parent = ""
		}
		listing, _ := os.ReadFile(filepath.Join(unpacked, filepath.FromSlash(path.Join(TrackedDirectory, parent, "files"))))
		head, _ := os.ReadFile(filepath.Join(unpacked, filepath.FromSlash(path.Join(TrackedDirectory, repository, "HEAD"))))
		if pin := "160000 commit " + strings.TrimSpace(string(head)) + "\t" + inside + "\x00"; !bytes.Contains(listing, []byte(pin)) {
			t.Errorf("%s's HEAD %s isn't the gitlink its parent's files pin", repository, head)
		}
	}
	got := []string{}
	filepath.WalkDir(filepath.Join(unpacked, TrackedDirectory), func(file string, entry fs.DirEntry, err error) error {
		if err == nil && !entry.IsDir() {
			name, _ := filepath.Rel(unpacked, file)
			got = append(got, filepath.ToSlash(name))
		}
		return err
	})
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the manifest holds %v, not %v", got, want)
	}
	for _, name := range want {
		if !slices.ContainsFunc(source.Chunks, func(chunk SourceChunk) bool { return chunk.First == name && chunk.Last == name && chunk.Files == 1 }) {
			t.Errorf("%s isn't a chunk of its own", name)
		}
	}
	if _, err := os.Lstat(filepath.Join(top, TrackedDirectory)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the manifest was written into the checkout: %v", err)
	}
	if status := gitIn(t, top, "status", "--porcelain", "--ignore-submodules=none"); len(status) > 0 {
		t.Errorf("the checkout changed: %s", status)
	}
}

// A submodule checked out at another commit than its parent pins fails the build, named, at any depth: its manifest
// would describe files the commit the tree records doesn't hold.
func TestASubmoduleAwayFromItsPinFailsNamed(t *testing.T) {
	for _, repository := range manifestRepositories[1:] {
		top := trackedTree(t)
		gitIn(t, filepath.Join(top, filepath.FromSlash(repository)), "commit", "-q", "--allow-empty", "-m", "moved")
		_, err := SourceChunks(top)
		if err == nil || !strings.Contains(err.Error(), "submodule "+repository+" is at") || !strings.Contains(err.Error(), "pins") {
			t.Errorf("%s moved from its pin: %v", repository, err)
		}
	}
}

// A submodule that isn't checked out fails the build, named, at any depth, never skipped: a runner's tests would read
// no manifest for it. So does one whose .git leads nowhere, which git can't answer for.
func TestAMissingSubmoduleFailsNamed(t *testing.T) {
	for _, repository := range manifestRepositories[1:] {
		top := trackedTree(t)
		parent, inside := top, repository
		if repository == "sub/deep/inner" {
			parent, inside = filepath.Join(top, "sub"), "deep/inner"
		}
		gitIn(t, parent, "submodule", "deinit", "-q", "-f", inside)
		_, err := SourceChunks(top)
		if err == nil || !strings.Contains(err.Error(), "submodule "+repository+", pinned at") || !strings.Contains(err.Error(), "isn't checked out") {
			t.Errorf("%s not checked out: %v", repository, err)
		}
	}
	top := trackedTree(t)
	os.WriteFile(filepath.Join(top, "sub", "deep", "inner", ".git"), []byte("gitdir: "+filepath.Join(t.TempDir(), "gone")+"\n"), 0o644)
	if _, err := TrackedManifest(top); err == nil || !strings.Contains(err.Error(), "submodule sub/deep/inner: git rev-parse HEAD") {
		t.Errorf("a submodule whose .git leads nowhere: %v", err)
	}
	if _, err := TrackedManifest(t.TempDir()); err == nil || !strings.Contains(err.Error(), "has no .git") {
		t.Errorf("a tree with no .git: %v", err)
	}
}

// The same tree makes the same manifest, byte for byte: another clone of it, its files written at other times, and a
// GIT_DIR in the environment naming another repository change nothing.
func TestTheManifestIsTheSameForTheSameTree(t *testing.T) {
	top := trackedTree(t)
	want, err := TrackedManifest(top)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 3*len(manifestRepositories) {
		t.Fatalf("%d manifest files", len(want))
	}
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, t.TempDir(), "clone", "-q", "--recurse-submodules", top, clone)
	other := gitTree(t, map[string]string{"other.txt": "other\n"})
	for name, tree := range map[string]string{"the tree again": top, "a clone": clone} {
		for _, environment := range [][2]string{{}, {"GIT_DIR", filepath.Join(other, ".git")}, {"GIT_INDEX_FILE", filepath.Join(other, ".git", "index")}} {
			if environment[0] != "" {
				t.Setenv(environment[0], environment[1])
			}
			got, err := TrackedManifest(tree)
			if environment[0] != "" {
				os.Unsetenv(environment[0])
			}
			if err != nil || !slices.EqualFunc(got, want, func(left, right archiveEntry) bool {
				return left.Name == right.Name && bytes.Equal(left.Content, right.Content)
			}) {
				t.Errorf("%s, with %v set, made another manifest: %v", name, environment, err)
			}
		}
	}
}

// A manifest file's chunk moves only when its own repository does: a commit to the tree alone changes the tree's three
// and keeps every submodule's, and a submodule moved changes its own and its parent's, and keeps the one inside it.
func TestTheManifestsChunksMoveOnlyWithTheirRepository(t *testing.T) {
	top := trackedTree(t)
	chunksOf := func() map[string]string {
		source, err := SourceChunks(top)
		if err != nil {
			t.Fatal(err)
		}
		blobs := map[string]string{}
		for _, chunk := range source.Chunks {
			if strings.HasPrefix(chunk.First, TrackedDirectory+"/") {
				blobs[chunk.First] = chunk.Blob
			}
		}
		return blobs
	}
	moved := func(before, after map[string]string) []string {
		names := []string{}
		for name, blob := range after {
			if before[name] != blob {
				names = append(names, path.Dir(name))
			}
		}
		slices.Sort(names)
		return slices.Compact(names)
	}
	first := chunksOf()
	if len(first) != 3*len(manifestRepositories) {
		t.Fatalf("%d manifest chunks: %v", len(first), first)
	}
	os.WriteFile(filepath.Join(top, "top.txt"), []byte("top, changed\n"), 0o644)
	gitCommit(t, top)
	second := chunksOf()
	if got := moved(first, second); !slices.Equal(got, []string{TrackedDirectory}) {
		t.Errorf("a commit to the tree alone moved %v", got)
	}
	os.WriteFile(filepath.Join(top, "sub", "sub.txt"), []byte("sub, changed\n"), 0o644)
	gitCommit(t, filepath.Join(top, "sub"))
	gitCommit(t, top)
	if got := moved(second, chunksOf()); !slices.Equal(got, []string{TrackedDirectory, TrackedDirectory + "/sub"}) {
		t.Errorf("moving the submodule moved %v", got)
	}
}

// A tree that tracks a path where the manifest goes is refused: the source would hold it twice, or beside it.
func TestATreeTrackingTheManifestsDirectoryIsRefused(t *testing.T) {
	tree := gitTree(t, map[string]string{"a.txt": "a\n", TrackedDirectory + "/note": "mine\n"})
	if _, err := SourceChunks(tree); err == nil || !strings.Contains(err.Error(), "tracks "+TrackedDirectory+"/note") {
		t.Fatalf("a tree tracking %s/note: %v", TrackedDirectory, err)
	}
}
