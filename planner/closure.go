package planner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// listedPackage is the part of go list -json the closure reads.
type listedPackage struct {
	ImportPath      string
	Dir             string
	Standard        bool
	ForTest         string
	Name            string
	GoFiles         []string
	CgoFiles        []string
	CFiles          []string
	CXXFiles        []string
	HFiles          []string
	SFiles          []string
	SysoFiles       []string
	EmbedFiles      []string
	TestGoFiles     []string
	XTestGoFiles    []string
	TestEmbedFiles  []string
	XTestEmbedFiles []string
}

// Closure is the contract's closure part: a hash over the sorted (file, sha256) of every source file in the
// package's import closure, its _test files and the files they embed included. A file is named by its package's
// import path and its name in that package. The standard library is left out: the Go version in tools covers it.
func Closure(tree, importPath string) (string, error) {
	files, err := closureFiles(tree, importPath)
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([][2]string, 0, len(names))
	for _, name := range names {
		content, err := os.ReadFile(files[name])
		if err != nil {
			return "", fmt.Errorf("closure file %s: %w", name, err)
		}
		sum := sha256.Sum256(content)
		pairs = append(pairs, [2]string{name, hex.EncodeToString(sum[:])})
	}
	canonical, err := Canonical(pairs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ClosureFiles is the closure's files that sit in the tree, repo-relative: what the read check counts as declared.
// Files from the module cache are left out, since a traced read outside the tree is never a finding.
func ClosureFiles(tree, importPath string) ([]string, error) {
	files, err := closureFiles(tree, importPath)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return nil, err
	}
	inTree := []string{}
	for _, file := range files {
		resolved, err := filepath.EvalSymlinks(file)
		if err != nil {
			return nil, err
		}
		if relative, err := filepath.Rel(root, resolved); err == nil && relative != ".." && !strings.HasPrefix(relative, "../") {
			inTree = append(inTree, filepath.ToSlash(relative))
		}
	}
	sort.Strings(inTree)
	return inTree, nil
}

// closureFiles lists the closure: each file's name (its package's import path and its name there) to its path.
func closureFiles(tree, importPath string) (map[string]string, error) {
	command := exec.Command("go", "list", "-deps", "-test", "-json", importPath)
	command.Dir = tree
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -deps -test %s: %w: %s", importPath, err, strings.TrimSpace(stderr.String()))
	}
	files := map[string]string{} // file name -> absolute path
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var listed listedPackage
		if err := decoder.Decode(&listed); err != nil {
			return nil, fmt.Errorf("go list output: %w", err)
		}
		// The synthesized test main (pkg.test) has no files of its own on disk.
		if listed.Standard || listed.Dir == "" || strings.HasSuffix(strings.SplitN(listed.ImportPath, " ", 2)[0], ".test") {
			continue
		}
		owner := strings.SplitN(listed.ImportPath, " ", 2)[0]
		for _, group := range [][]string{listed.GoFiles, listed.CgoFiles, listed.CFiles, listed.CXXFiles, listed.HFiles, listed.SFiles,
			listed.SysoFiles, listed.EmbedFiles, listed.TestGoFiles, listed.XTestGoFiles, listed.TestEmbedFiles, listed.XTestEmbedFiles} {
			for _, name := range group {
				files[owner+"/"+filepath.ToSlash(name)] = filepath.Join(listed.Dir, name)
			}
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("go list found no files for %s", importPath)
	}
	return files, nil
}
