package builder

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// A runner unpacks a tree's source from its chunks, and a chunk carries no .git, so a test that asks git about the
// tree (its commit, its tracked files: adamic's corpus tests, #cyasrr4) would go red on every runner, the
// environment's red charged to the author. Only the builder, which has .git, can answer, so it writes git's answers
// into the source as its manifest, and adamic's internal/tracked reads them where there is no .git.
//
// The manifest is one directory per repository under TrackedDirectory at the source's root: the tree's own at its top,
// each checked-out submodule's at its path (cohere's at .tracked/cohere, cohere's TypeScript at
// .tracked/cohere/TypeScript), each holding three files, a git command's output byte for byte, run in that repository:
//
//	HEAD    git rev-parse HEAD
//	commit  git cat-file commit HEAD
//	files   git ls-tree -r -z --full-tree HEAD
//
// commit is the commit object HEAD names, so a reader can check it hashes to HEAD and that its tree is the one files
// rebuilds, tying the listing to the commit. All three hang on the commit alone, so the same tree makes the same bytes. Each is a chunk of its own (chunks.go), so a
// submodule's list is uploaded and fetched again only when the submodule moves.

// TrackedDirectory is where a tree's source carries its manifest, at its root. It is never written into the checkout.
const TrackedDirectory = ".tracked"

// trackedGitDropped are the environment's variables that point git at another repository, index or object store than
// the directory it runs in: with any of them set, a submodule's answers could be another repository's.
var trackedGitDropped = []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_COMMON_DIR", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE"}

// TrackedManifest is the tree's manifest as archive entries, made in memory: HEAD, commit and files for the tree, then
// for every submodule its listing records, recursively. Each submodule must be checked out (its .git there), at the
// commit its parent's gitlink pins; one that isn't, or a repository git can't answer for, fails it, named, since a
// runner's tests would read a manifest that isn't the source's. (build-tree's checkout inits every submodule.)
func TrackedManifest(tree string) ([]archiveEntry, error) {
	entries := []archiveEntry{}
	var record func(relative, pin string) error
	record = func(relative, pin string) error {
		name := "the tree"
		if relative != "" {
			name = "submodule " + relative
		}
		directory := filepath.Join(tree, filepath.FromSlash(relative))
		// Without its own .git, git would answer from the repository around it.
		if _, err := os.Lstat(filepath.Join(directory, ".git")); errors.Is(err, fs.ErrNotExist) {
			if relative == "" {
				return fmt.Errorf("the tree's manifest: %s has no .git, so git can't say what it tracks", tree)
			}
			return fmt.Errorf("the tree's manifest: %s, pinned at %s, isn't checked out", name, pin)
		} else if err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		head, err := trackedGit(directory, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		if commit := strings.TrimSuffix(string(head), "\n"); !objectName(commit) || (pin != "" && commit != pin) {
			if pin != "" {
				return fmt.Errorf("the tree's manifest: %s is at %q, and its parent pins %s", name, commit, pin)
			}
			return fmt.Errorf("the tree's manifest: %s is at %q, not a commit", name, commit)
		}
		// HEAD by the name rev-parse gave, so all three are the one commit's.
		commit, err := trackedGit(directory, "cat-file", "commit", strings.TrimSuffix(string(head), "\n"))
		if err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		listing, err := trackedGit(directory, "ls-tree", "-r", "-z", "--full-tree", strings.TrimSuffix(string(head), "\n"))
		if err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		at := path.Join(TrackedDirectory, relative)
		entries = append(entries, archiveEntry{Name: at + "/HEAD", Content: head}, archiveEntry{Name: at + "/commit", Content: commit},
			archiveEntry{Name: at + "/files", Content: listing})
		for _, line := range bytes.Split(bytes.TrimSuffix(listing, []byte{0}), []byte{0}) {
			// "<mode> <type> <object>\t<path>"; a gitlink's mode is 160000.
			header, inner, _ := strings.Cut(string(line), "\t")
			fields := strings.Fields(header)
			if len(fields) != 3 || fields[0] != "160000" {
				continue
			}
			if inner == "" || !filepath.IsLocal(filepath.FromSlash(inner)) || !objectName(fields[2]) {
				return fmt.Errorf("the tree's manifest: %s lists a gitlink %q", name, line)
			}
			if err := record(path.Join(relative, inner), fields[2]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := record("", ""); err != nil {
		return nil, err
	}
	return entries, nil
}

// trackedGit is git with arguments in directory, with no variable that would point it at another repository, and its
// output as it printed it.
func trackedGit(directory string, arguments ...string) ([]byte, error) {
	command := exec.Command("git", arguments...)
	command.Dir = directory
	for _, variable := range os.Environ() {
		if name, _, _ := strings.Cut(variable, "="); !slices.Contains(trackedGitDropped, name) {
			command.Env = append(command.Env, variable)
		}
	}
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

// objectName reports whether text is a git object's name: 40 hex digits (sha1) or 64 (sha256).
func objectName(text string) bool {
	return (len(text) == 40 || len(text) == 64) && strings.Trim(text, "0123456789abcdef") == ""
}
