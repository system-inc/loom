package planner

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// DeclaredReads is a stub for the contract's traced reads (#ed4p461 replaces it): the files a unit's tests read
// outside its Go closure, as declared today. That is the package's own testdata (read by path, never compiled), and
// every tracked file a reads line in the gate tools' executors.txt names for the package, the tools' lines first and
// then the tree's. A tree's lines only add, so a branch can declare its own reads and never hide one.
func DeclaredReads(tree, tools, packageDirectory string) ([]string, error) {
	if tools == "" {
		return nil, fmt.Errorf("declared reads need the gate tools for their executors.txt")
	}
	listing := exec.Command("git", "-C", tree, "ls-files", "-z")
	output, err := listing.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", tree, err)
	}
	files := strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00")
	patterns := []string{}
	for _, root := range []string{tools, tree} {
		lines, err := readsLines(filepath.Join(root, "cloud/fast-gate/executors.txt"), packageDirectory)
		if err != nil {
			return nil, err
		}
		patterns = append(patterns, lines...)
	}
	read := map[string]bool{}
	for _, file := range files {
		parts := strings.Split(file, "/")
		for index, part := range parts {
			// The package's own testdata: the directory above the testdata component is the package.
			if part == "testdata" && strings.Join(parts[:index], "/") == packageDirectory {
				read[file] = true
			}
		}
		for _, pattern := range patterns {
			// executors.txt's globs are fnmatch, where * spans directories; path.Match's doesn't, so a
			// pattern's * is tried against the whole remaining path too.
			if fnmatch(pattern, file) {
				read[file] = true
			}
		}
	}
	reads := make([]string, 0, len(read))
	for file := range read {
		reads = append(reads, file)
	}
	sort.Strings(reads)
	return reads, nil
}

func readsLines(executors, packageDirectory string) ([]string, error) {
	content, err := os.ReadFile(executors)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var patterns []string
	scanner := bufio.NewScanner(bytes.NewReader(content))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 3 && fields[0] == "reads" && fields[1] == packageDirectory {
			patterns = append(patterns, fields[2])
		}
	}
	return patterns, scanner.Err()
}

// fnmatch is Python's fnmatch.fnmatchcase as the gate uses it: * and ? match any character, / included.
func fnmatch(pattern, name string) bool {
	if !strings.ContainsAny(pattern, "*?[") {
		return pattern == name
	}
	// path.Match stops * at /, so match with / swapped for a byte no repository path holds.
	const slash = "\x01"
	matched, err := path.Match(strings.ReplaceAll(pattern, "/", slash), strings.ReplaceAll(name, "/", slash))
	return err == nil && matched
}

// ReadsHash is the contract's reads part over the given files: a hash of their sorted (path, sha256). A tracked
// symlink is its target, as git records it ("symlink <target>"), so a dangling one a test walks still keys. A read that
// names a submodule (executors.txt names cohere by its gitlink's exact path) is the submodule at its recorded commit,
// so it hashes as "gitlink <commit>". That is a unit's reads part until a read set is recorded for it (readset.go):
// then its key holds the submodule paths its runs read, each by its content, in place of every gitlink.
func ReadsHash(tree string, reads []string) (string, error) {
	gitlinks, err := Gitlinks(tree)
	if err != nil {
		return "", err
	}
	pairs, err := readPairsOf(tree, reads, gitlinks)
	if err != nil {
		return "", err
	}
	return pairs.all, nil
}

// readPairs is a unit's declared reads as its key holds them: all is ReadsHash, every gitlink read at its commit;
// files is the (path, sha256) of each read that isn't a gitlink; gitlinks counts the reads that are.
type readPairs struct {
	all      string
	files    [][2]string
	gitlinks int
}

func readPairsOf(tree string, reads []string, gitlinks map[string]string) (readPairs, error) {
	all := make([][2]string, 0, len(reads))
	pairs := readPairs{files: [][2]string{}}
	for _, read := range reads {
		if commit, found := gitlinks[read]; found {
			all = append(all, [2]string{read, "gitlink " + commit})
			pairs.gitlinks++
			continue
		}
		digest, err := fileDigest(filepath.Join(tree, filepath.FromSlash(read)))
		if err != nil {
			return readPairs{}, fmt.Errorf("read %s: %w", read, err)
		}
		all = append(all, [2]string{read, digest})
		pairs.files = append(pairs.files, [2]string{read, digest})
	}
	var err error
	pairs.all, err = pairsHash(all)
	return pairs, err
}

// pairsHash is the sha256 of the pairs' canonical JSON, in the order given.
func pairsHash(pairs [][2]string) (string, error) {
	canonical, err := Canonical(pairs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// Gitlinks is the tree's submodules: each gitlink's path to the commit the tree records for it.
func Gitlinks(tree string) (map[string]string, error) {
	output, err := exec.Command("git", "-C", tree, "ls-files", "-s", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files -s in %s: %w", tree, err)
	}
	gitlinks := map[string]string{}
	for _, entry := range strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00") {
		// <mode> <object> <stage>\t<path>
		head, path, found := strings.Cut(entry, "\t")
		if fields := strings.Fields(head); found && len(fields) == 3 && fields[0] == "160000" {
			gitlinks[path] = fields[1]
		}
	}
	return gitlinks, nil
}

// fileDigest is a tracked file's sha256, or "symlink <target>" for a symlink, which is never followed.
func fileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		return "symlink " + target, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}
