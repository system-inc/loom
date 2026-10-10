package builder

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The independent review of 98eae8c's proofs, as it wrote them: each passes when the build refuses the tree, or when
// the source it builds agrees with its manifest. tracked_test.go holds the tests that say which, and why.

// manifestFiles parses the unpacked .tracked/<repo>/files into path -> blob.
func manifestFiles(t *testing.T, unpacked, repo string) map[string]string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(unpacked, ".tracked", repo, "files"))
	if err != nil {
		t.Fatalf("reading manifest for %q: %v", repo, err)
	}
	out := map[string]string{}
	for _, line := range bytes.Split(bytes.TrimSuffix(content, []byte{0}), []byte{0}) {
		header, name, _ := strings.Cut(string(line), "\t")
		fields := strings.Fields(header)
		out[name] = fields[2]
	}
	return out
}

func hashObject(t *testing.T, file string) string {
	out, err := exec.Command("git", "hash-object", "--no-filters", file).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// compare reports every disagreement between the unpacked top-level files and the tree's own manifest.
func compare(t *testing.T, unpacked string, repo string) []string {
	files := manifestFiles(t, unpacked, repo)
	var diffs []string
	for name, blob := range files {
		full := filepath.Join(unpacked, repo, name)
		info, err := os.Lstat(full)
		if err != nil {
			diffs = append(diffs, "missing from archive: "+name)
			continue
		}
		if info.Mode().IsRegular() && hashObject(t, full) != blob {
			diffs = append(diffs, "content differs from manifest: "+name)
		}
	}
	filepath.Walk(filepath.Join(unpacked, repo), func(p string, info os.FileInfo, err error) error {
		rel, _ := filepath.Rel(filepath.Join(unpacked, repo), p)
		if info.IsDir() || strings.HasPrefix(rel, ".tracked") || strings.HasPrefix(rel, "sub/") {
			return nil
		}
		if _, ok := files[filepath.ToSlash(rel)]; !ok {
			diffs = append(diffs, "in archive, not in manifest: "+rel)
		}
		return nil
	})
	return diffs
}

func TestReviewModifiedTrackedFile(t *testing.T) {
	top := trackedTree(t)
	os.WriteFile(filepath.Join(top, "top.txt"), []byte("mutated by a build step\n"), 0o644)
	source, err := SourceChunks(top, nil)
	if err != nil {
		t.Logf("refused: %v", err)
		return
	}
	if d := compare(t, unpackSource(t, source), ""); len(d) > 0 {
		t.Errorf("CONFIRMED modified file: %v", d)
	}
}

func TestReviewStagedNewFile(t *testing.T) {
	top := trackedTree(t)
	os.WriteFile(filepath.Join(top, "staged.txt"), []byte("staged\n"), 0o644)
	gitIn(t, top, "add", "staged.txt")
	source, err := SourceChunks(top, nil)
	if err != nil {
		t.Logf("refused: %v", err)
		return
	}
	if d := compare(t, unpackSource(t, source), ""); len(d) > 0 {
		t.Errorf("CONFIRMED staged file: %v", d)
	}
}

func TestReviewSubmoduleDirty(t *testing.T) {
	top := trackedTree(t)
	os.WriteFile(filepath.Join(top, "sub", "sub.txt"), []byte("dirty in sub\n"), 0o644)
	source, err := SourceChunks(top, nil)
	if err != nil {
		t.Logf("refused: %v", err)
		return
	}
	u := unpackSource(t, source)
	files := manifestFiles(t, u, "sub")
	if hashObject(t, filepath.Join(u, "sub", "sub.txt")) != files["sub.txt"] {
		t.Errorf("CONFIRMED dirty submodule archived, manifest says HEAD")
	}
}

func TestReviewGitIndexFileEnv(t *testing.T) {
	top := trackedTree(t)
	// An index holding only z.txt.
	other := filepath.Join(t.TempDir(), "index")
	cmd := exec.Command("git", "read-tree", "HEAD")
	cmd.Dir = top
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+other)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	cmd = exec.Command("git", "rm", "-q", "--cached", "top.txt", "a/b.txt")
	cmd.Dir = top
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+other)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	t.Setenv("GIT_INDEX_FILE", other)
	source, err := SourceChunks(top, nil)
	if err != nil {
		t.Logf("refused: %v", err)
		return
	}
	if d := compare(t, unpackSource(t, source), ""); len(d) > 0 {
		t.Errorf("CONFIRMED GIT_INDEX_FILE: ls-files honors it, manifest strips it: %v", d)
	}
}

func TestReviewSubmoduleNamedFiles(t *testing.T) {
	inner := gitTree(t, map[string]string{"i.txt": "i\n"})
	top := gitTree(t, map[string]string{"top.txt": "top\n"})
	gitIn(t, top, "submodule", "add", "-q", inner, "files")
	gitCommit(t, top)
	source, err := SourceChunks(top, nil)
	if err != nil {
		t.Logf("refused at build: %v", err)
		return
	}
	directory := t.TempDir()
	for _, chunk := range source.Chunks {
		if err := UnpackChunk(bytes.NewReader(source.Blobs[chunk.Blob]), directory, chunk); err != nil {
			t.Errorf("CONFIRMED submodule at path 'files' collides with .tracked/files: built fine, unpack fails: %v", err)
			return
		}
	}
	t.Errorf("CONFIRMED? unpacked without error; check layout")
}

func TestReviewReplaceRef(t *testing.T) {
	top := trackedTree(t)
	head := strings.TrimSpace(string(gitIn(t, top, "rev-parse", "HEAD")))
	// A replacement commit with a different message and an empty tree.
	empty := strings.TrimSpace(string(gitIn(t, top, "hash-object", "-t", "tree", "-w", "--stdin")))
	_ = empty
	replacement := strings.TrimSpace(string(gitIn(t, top, "commit-tree", "-m", "replaced", "4b825dc642cb6eb9a060e54bf8d69288fbee4904")))
	gitIn(t, top, "replace", head, replacement)
	gitIn(t, top, "checkout", "-q", "--force", "--detach", head) // what GitCheckout does
	source, err := SourceChunks(top, nil)
	if err != nil {
		t.Logf("refused: %v", err)
		return
	}
	u := unpackSource(t, source)
	commit, _ := os.ReadFile(filepath.Join(u, ".tracked", "commit"))
	cmd := exec.Command("git", "hash-object", "-t", "commit", "--stdin")
	cmd.Stdin = bytes.NewReader(commit)
	sum, _ := cmd.Output()
	if strings.TrimSpace(string(sum)) != head {
		t.Errorf("CONFIRMED replace ref: .tracked/commit hashes to %s, HEAD %s", strings.TrimSpace(string(sum)), head)
	}
}

func TestReviewAssumeUnchanged(t *testing.T) {
	top := trackedTree(t)
	gitIn(t, top, "update-index", "--assume-unchanged", "z.txt")
	os.WriteFile(filepath.Join(top, "z.txt"), []byte("hidden change\n"), 0o644)
	status := gitIn(t, top, "status", "--porcelain", "--untracked-files=no")
	source, err := SourceChunks(top, nil)
	if err != nil {
		t.Logf("refused: %v", err)
		return
	}
	if d := compare(t, unpackSource(t, source), ""); len(d) > 0 {
		t.Errorf("CONFIRMED status=%q (TreeHash sees clean) yet: %v", status, d)
	}
}
