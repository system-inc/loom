package builder

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// What git's index vouches for is taken as its commit's unread (#s0cqqhk lever 5). Each mutant below must make a test
// here fail:
//
//	an entry marked assume-unchanged vouched for: TestGitVouchesOnlyForWhatItsIndexStatChecked (and
//	TestASourceItsManifestContradictsFailsNamed's hidden change)
//	an entry marked skip-worktree vouched for: TestGitVouchesOnlyForWhatItsIndexStatChecked,
//	TestAChangeGitIsToldNotToLookAtStillFails
//	diff-index's list not read: TestGitVouchesOnlyForWhatItsIndexStatChecked (and
//	TestASourceItsManifestContradictsFailsNamed's changed files)
//	a submodule's paths vouched for under its own names, not the tree's: TestGitVouchesOnlyForWhatItsIndexStatChecked
//	fsmonitor left as the repository sets it: TestAChangeGitIsToldNotToLookAtStillFails
//	a path under a conversion vouched for: TestGitVouchesOnlyForWhatItsIndexStatChecked,
//	TestAChangeGitIsToldNotToLookAtStillFails
//	core.autocrlf not read: TestGitVouchesOnlyForWhatItsIndexStatChecked
//	a vouched file hashed anyway (correct, and the warm check as slow as the cold):
//	TestAWarmCheckReadsOnlyWhatGitCantVouchFor

// settled is the tree as a checkout long done is, for git's stat check: every file under it, submodules' too, set
// back a day, and each repository's index refreshed, so git trusts by stat every entry it can and none was written in
// the second its index was (racy git, which git reads itself). A link's own times can't be set back from Go, so a
// link may stay racy, and the tests here leave links out.
func settled(t *testing.T, top string, repositories []string) {
	t.Helper()
	past := time.Now().Add(-24 * time.Hour)
	err := filepath.WalkDir(top, func(file string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.Name() == ".git" && entry.IsDir():
			return filepath.SkipDir
		case entry.Name() != ".git" && entry.Type().IsRegular():
			return os.Chtimes(file, past, past)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, repository := range repositories {
		gitIn(t, filepath.Join(top, filepath.FromSlash(repository)), "update-index", "-q", "--refresh")
	}
}

// regularVouched is what gitVouches answers for the tree, its regular files alone, sorted.
func regularVouched(t *testing.T, top string) []string {
	t.Helper()
	_, _, repositories, err := trackedManifest(top)
	if err != nil {
		t.Fatal(err)
	}
	vouched, err := gitVouches(top, repositories)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for name := range vouched {
		if info, err := os.Lstat(filepath.Join(top, filepath.FromSlash(name))); err == nil && info.Mode().IsRegular() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// Git vouches, in the tree and every submodule under the tree's names for its files, for exactly the files its index
// stat-checks and finds as their commit has them: not one changed since, not one marked assume-unchanged or
// skip-worktree (changed or not), and not one under a text attribute or, with none, core.autocrlf, since git compares
// a file's content after its conversions and the source archives its bytes.
func TestGitVouchesOnlyForWhatItsIndexStatChecked(t *testing.T) {
	top := trackedTree(t)
	settled(t, top, manifestRepositories)
	all := []string{".gitmodules", "a/b.txt", "sub/.gitmodules", "sub/deep/inner/b/c.txt", "sub/deep/inner/inner.txt", "sub/sub.txt", "sub/x/y.txt", "top.txt", "z.txt"}
	if got := regularVouched(t, top); !slices.Equal(got, all) {
		t.Fatalf("a settled tree: git vouches for %v, not %v", got, all)
	}
	os.WriteFile(filepath.Join(top, "top.txt"), []byte("top, changed\n"), 0o644)
	os.WriteFile(filepath.Join(top, "sub", "x", "y.txt"), []byte("y, changed\n"), 0o644)
	gitIn(t, top, "update-index", "--assume-unchanged", "z.txt")
	gitIn(t, top, "update-index", "--skip-worktree", "a/b.txt")
	// The submodule's own attributes file, which no commit holds: sub.txt is text.
	attributes := strings.TrimSpace(string(gitIn(t, filepath.Join(top, "sub"), "rev-parse", "--path-format=absolute", "--git-path", "info/attributes")))
	os.MkdirAll(filepath.Dir(attributes), 0o755)
	os.WriteFile(attributes, []byte("sub.txt text\n"), 0o644)
	// With no attribute, core.autocrlf makes the inner submodule's every file text.
	gitIn(t, filepath.Join(top, "sub", "deep", "inner"), "config", "core.autocrlf", "input")
	want := []string{".gitmodules", "sub/.gitmodules"}
	if got := regularVouched(t, top); !slices.Equal(got, want) {
		t.Errorf("git vouches for %v, not %v", got, want)
	}
}

// A file changed where git's index is told not to look still fails the build, named: one marked skip-worktree, one an
// fsmonitor answers for as unchanged, and one whose filter makes git's content for it its commit's blob though its
// bytes, the same size, aren't.
func TestAChangeGitIsToldNotToLookAtStillFails(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, top string){
		"skip-worktree": func(t *testing.T, top string) {
			gitIn(t, top, "update-index", "--skip-worktree", "z.txt")
			os.WriteFile(filepath.Join(top, "z.txt"), []byte("hidden\n"), 0o644)
		},
		"an fsmonitor that answers nothing changed": func(t *testing.T, top string) {
			hook := filepath.Join(t.TempDir(), "fsmonitor")
			os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'loom-token\\0'\n"), 0o755)
			gitIn(t, top, "config", "core.fsmonitor", hook)
			gitIn(t, top, "update-index", "-q", "--refresh")
			gitIn(t, top, "status", "--porcelain")
			os.WriteFile(filepath.Join(top, "z.txt"), []byte("hidden\n"), 0o644)
			if listed := gitIn(t, top, "diff-index", "--name-only", "HEAD"); len(listed) != 0 {
				t.Fatalf("the fsmonitor hides nothing from git: %s", listed)
			}
		},
		"a filter": func(t *testing.T, top string) {
			gitIn(t, top, "config", "filter.upper.clean", "tr a-z A-Z")
			gitIn(t, top, "config", "filter.upper.smudge", "cat")
			attributes := strings.TrimSpace(string(gitIn(t, top, "rev-parse", "--path-format=absolute", "--git-path", "info/attributes")))
			os.MkdirAll(filepath.Dir(attributes), 0o755)
			os.WriteFile(attributes, []byte("z.txt filter=upper\n"), 0o644)
			// Git's content for z\n is Z\n: committed so, the file's bytes stay z\n.
			gitIn(t, top, "add", "--renormalize", "z.txt")
			gitCommit(t, top)
			settled(t, top, manifestRepositories)
			if listed := gitIn(t, top, "diff-index", "--name-only", "HEAD"); len(listed) != 0 {
				t.Fatalf("git sees the filtered file as changed: %s", listed)
			}
		},
	} {
		top := trackedTree(t)
		settled(t, top, manifestRepositories)
		change(t, top)
		_, err := SourceChunks(top, nil)
		if err == nil || !strings.Contains(err.Error(), "isn't what its manifest records, first at z.txt: it hashes to") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A warm check reads only what git can't vouch for: a settled tree's files can all be unreadable and its source is
// still checked, while the one file changed since is read, and fails it.
func TestAWarmCheckReadsOnlyWhatGitCantVouchFor(t *testing.T) {
	top := trackedTree(t)
	settled(t, top, manifestRepositories)
	_, tracked, repositories, err := trackedManifest(top)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for name := range tracked {
		names = append(names, name)
	}
	vouched, err := gitVouches(top, repositories)
	if err != nil {
		t.Fatal(err)
	}
	// Opening a file read-only needs its read bit; chmod moves its ctime, so it is taken away after git has answered.
	for _, name := range []string{"top.txt", "sub/deep/inner/inner.txt"} {
		file := filepath.Join(top, filepath.FromSlash(name))
		os.Chmod(file, 0o200)
		t.Cleanup(func() { os.Chmod(file, 0o644) })
	}
	if err = checkSource(top, names, tracked, vouched); err != nil {
		t.Errorf("the vouched files were read: %v", err)
	}
	delete(vouched, "top.txt")
	if err = checkSource(top, names, tracked, vouched); err == nil || !strings.Contains(err.Error(), "top.txt") {
		t.Errorf("a file git didn't vouch for wasn't read: %v", err)
	}
}
