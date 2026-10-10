package builder

import (
	"bytes"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/system-inc/loom/planner"
)

// A runner unpacks a tree's source from its chunks, and a chunk carries no .git, so a test that asks git about the
// tree (its commit, its tracked files: adamic's corpus tests, #cyasrr4) would go red on every runner, the
// environment's red charged to the author. Only the builder, which has .git, can answer, so it writes git's answers
// into the source as its manifest, and adamic's internal/tracked reads them where there is no .git.
//
// The manifest is one directory per repository under TrackedDirectory at the source's root: the tree's own at its top,
// each checked-out submodule's at its path (cohere's at .tracked/cohere, cohere's TypeScript at
// .tracked/cohere/TypeScript), each holding three files, a git command's output byte for byte, run in that repository
// through planner.LocalGit (its own objects alone: no replace refs, no grafts, no variable pointing elsewhere):
//
//	HEAD    git rev-parse HEAD
//	commit  git cat-file commit HEAD
//	files   git ls-tree -r -z --full-tree HEAD
//
// commit is the commit object HEAD names, checked here to hash to HEAD and to name the tree HEAD^{tree} does, so a
// reader can tie the listing to the commit. All three hang on the commit alone, so the same tree makes the same bytes.
// Each is a chunk of its own (chunks.go), so a submodule's list is uploaded and fetched again only when it moves.
//
// The manifest describes each repository's commit, and the source is read from the checkout, which the build has run
// in. So SourceChunks checks every file it archives against the manifest (checkSource): a tree a build step changed,
// or whose index hides a change (assume-unchanged), would have built its binaries from something that isn't its
// commit, and fails loudly rather than ship a source its own manifest contradicts.

// TrackedDirectory is where a tree's source carries its manifest, at its root. It is never written into the checkout.
const TrackedDirectory = ".tracked"

// manifestFileNames are the files each repository's manifest directory holds. A submodule at a path with one of these
// names (as a filesystem that folds case would take it) would put its directory where its parent's file is.
var manifestFileNames = []string{"HEAD", "commit", "files"}

// A trackedFile is what a repository's commit records for one file of the tree's source: its git mode (100644,
// 100755 or 120000) and the blob it holds.
type trackedFile struct {
	mode   string
	object string
}

// TrackedManifest is the tree's manifest as archive entries, made in memory: HEAD, commit and files for the tree, then
// for every submodule its listing records, recursively.
func TrackedManifest(tree string) ([]archiveEntry, error) {
	entries, _, err := trackedManifest(tree)
	return entries, err
}

// trackedManifest is TrackedManifest and what the commits record for every file of the source, by its path in the
// tree, submodules' files under their paths. Each submodule must be checked out (its .git there), at the commit its
// parent's gitlink pins, at a path no manifest file's name takes; one that isn't, or a repository git can't answer
// for, or a commit object that isn't HEAD's, fails it, named, since a runner's tests would read a manifest that isn't
// the source's. (build-tree's checkout inits every submodule.)
func trackedManifest(tree string) ([]archiveEntry, map[string]trackedFile, error) {
	entries := []archiveEntry{}
	tracked := map[string]trackedFile{}
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
		head, err := localGit(directory, "rev-parse", "HEAD")
		if err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		commit := strings.TrimSuffix(string(head), "\n")
		if !objectName(commit) || (pin != "" && commit != pin) {
			if pin != "" {
				return fmt.Errorf("the tree's manifest: %s is at %q, and its parent pins %s", name, commit, pin)
			}
			return fmt.Errorf("the tree's manifest: %s is at %q, not a commit", name, commit)
		}
		// HEAD by the name rev-parse gave, so all three are the one commit's.
		object, err := localGit(directory, "cat-file", "commit", commit)
		if err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		treeObject, err := localGit(directory, "rev-parse", commit+"^{tree}")
		if err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		if err = checkCommitObject(commit, object, strings.TrimSuffix(string(treeObject), "\n")); err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		listing, err := localGit(directory, "ls-tree", "-r", "-z", "--full-tree", commit)
		if err != nil {
			return fmt.Errorf("the tree's manifest: %s: %w", name, err)
		}
		at := path.Join(TrackedDirectory, relative)
		entries = append(entries, archiveEntry{Name: at + "/HEAD", Content: head}, archiveEntry{Name: at + "/commit", Content: object},
			archiveEntry{Name: at + "/files", Content: listing})
		for _, line := range bytes.Split(bytes.TrimSuffix(listing, []byte{0}), []byte{0}) {
			// "<mode> <type> <object>\t<path>"; a gitlink's mode is 160000.
			header, inner, _ := strings.Cut(string(line), "\t")
			fields := strings.Fields(header)
			if len(fields) != 3 || inner == "" || !filepath.IsLocal(filepath.FromSlash(inner)) || !objectName(fields[2]) {
				return fmt.Errorf("the tree's manifest: %s lists %q, not a git ls-tree -r entry", name, line)
			}
			full := path.Join(relative, inner)
			if fields[0] != "160000" {
				tracked[full] = trackedFile{mode: fields[0], object: fields[2]}
				continue
			}
			for _, part := range strings.Split(full, "/") {
				for _, taken := range manifestFileNames {
					if strings.EqualFold(part, taken) {
						return fmt.Errorf("the tree's manifest: submodule %s is at a path through %q, which the manifest's own %s file takes", full, part, taken)
					}
				}
			}
			if err := record(full, fields[2]); err != nil {
				return err
			}
		}
		return nil
	}
	if err := record("", ""); err != nil {
		return nil, nil, err
	}
	return entries, tracked, nil
}

// checkCommitObject refuses a commit object that isn't the one head names: one that doesn't hash to head, as git
// hashes a commit, or whose tree line isn't tree.
func checkCommitObject(head string, object []byte, tree string) error {
	sum, err := gitObject("commit", bytes.NewReader(object), int64(len(object)), len(head))
	if err != nil {
		return err
	}
	if sum != head {
		return fmt.Errorf("its commit object hashes to %s, not %s", sum, head)
	}
	if first, _, _ := bytes.Cut(object, []byte("\n")); string(first) != "tree "+tree {
		return fmt.Errorf("its commit object's first line is %q, and %s^{tree} is %s", first, head, tree)
	}
	return nil
}

// checkSource refuses a source whose files aren't what the manifest records: names, the paths it archives (git
// ls-files --recurse-submodules, read from the index), against tracked, read from each commit. A path in one and not
// the other, another kind (a file for a link), another executable bit, or content that doesn't hash to its blob fails
// it, naming the first such path in byte order.
func checkSource(tree string, names []string, tracked map[string]trackedFile) error {
	paths := slices.Clone(names)
	for name := range tracked {
		paths = append(paths, name)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	listed := map[string]bool{}
	for _, name := range names {
		listed[name] = true
	}
	verdicts := make([]string, len(paths))
	err := each(len(paths), runtime.NumCPU(), func(index int) error {
		name := paths[index]
		record, recorded := tracked[name]
		switch {
		case !recorded:
			verdicts[index] = "the index lists it, and no commit records it"
		case !listed[name]:
			verdicts[index] = "its commit records it, and the index doesn't list it"
		default:
			verdict, err := differs(filepath.Join(tree, filepath.FromSlash(name)), record)
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			verdicts[index] = verdict
		}
		return nil
	})
	if err != nil {
		return err
	}
	for index, verdict := range verdicts {
		if verdict != "" {
			return fmt.Errorf("the tree's source isn't what its manifest records, first at %s: %s (a build step changed the tree, or its index hides a change)", paths[index], verdict)
		}
	}
	return nil
}

// differs says how the file at name isn't what record holds, or "" when it is: missing, another kind, another
// executable bit, or content that doesn't hash to its blob.
func differs(name string, record trackedFile) (string, error) {
	info, err := os.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "it is missing", nil
	}
	if err != nil {
		return "", err
	}
	var content io.Reader
	size := info.Size()
	switch {
	case record.mode == "120000" && info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Readlink(name)
		if err != nil {
			return "", err
		}
		content, size = strings.NewReader(target), int64(len(target))
	case (record.mode == "100644" || record.mode == "100755") && info.Mode().IsRegular():
		if executable := info.Mode().Perm()&0o111 != 0; executable != (record.mode == "100755") {
			return fmt.Sprintf("its executable bit is %t, and its commit records mode %s", executable, record.mode), nil
		}
		file, err := os.Open(name)
		if err != nil {
			return "", err
		}
		defer file.Close()
		content = file
	default:
		kind := "file"
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			kind = "link"
		case info.IsDir():
			kind = "directory"
		case !info.Mode().IsRegular():
			kind = "special file"
		}
		return fmt.Sprintf("it is a %s, and its commit records mode %s", kind, record.mode), nil
	}
	sum, err := gitObject("blob", content, size, len(record.object))
	if err != nil {
		return "", err
	}
	if sum != record.object {
		return fmt.Sprintf("it hashes to %s, and its commit records %s", sum, record.object), nil
	}
	return "", nil
}

// gitObject is git's name for content as an object of kind: sha1 for a 40-digit name, sha256 for a 64-digit one. A
// reader that doesn't hold exactly size bytes is refused.
func gitObject(kind string, content io.Reader, size int64, digits int) (string, error) {
	var sum hash.Hash
	switch digits {
	case 40:
		sum = sha1.New()
	case 64:
		sum = sha256.New()
	default:
		return "", fmt.Errorf("an object name of %d digits", digits)
	}
	fmt.Fprintf(sum, "%s %d\x00", kind, size)
	read, err := io.Copy(sum, content)
	if err != nil {
		return "", err
	}
	if read != size {
		return "", fmt.Errorf("%d bytes read, and it held %d", read, size)
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// localGit is planner.LocalGit's output, with git's own complaint in the error.
func localGit(directory string, arguments ...string) ([]byte, error) {
	command := planner.LocalGit(directory, arguments...)
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
