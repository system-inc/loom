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

// ReadsHash is the contract's reads part over the given files: a hash of their sorted (path, sha256).
func ReadsHash(tree string, reads []string) (string, error) {
	pairs := make([][2]string, 0, len(reads))
	for _, read := range reads {
		content, err := os.ReadFile(filepath.Join(tree, filepath.FromSlash(read)))
		if err != nil {
			return "", fmt.Errorf("read %s: %w", read, err)
		}
		sum := sha256.Sum256(content)
		pairs = append(pairs, [2]string{read, hex.EncodeToString(sum[:])})
	}
	canonical, err := Canonical(pairs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}
