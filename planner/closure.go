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
	Module          *struct{ Path, GoMod string }
}

// Closure is the contract's closure part: a hash over the sorted (file, sha256) of every source file in the
// package's import closure, its _test files and the files they embed included, and of the module files the go
// command builds them under: each module's go.mod and go.sum, and the workspace's go.work and go.work.sum. A source
// file is named by its package's import path and its name in that package, a module file by its module path (the
// workspace's by "go.work"). The standard library is left out: the Go version in tools covers it.
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
		addListedFiles(listed, files)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("go list found no files for %s", importPath)
	}
	if err := addWorkspaceFiles(tree, files); err != nil {
		return nil, err
	}
	return files, nil
}

// addListedFiles adds one listed package's files to a closure, each by its name (its package's import path and its
// name there), and its module's files.
func addListedFiles(listed listedPackage, files map[string]string) {
	// The synthesized test main (pkg.test) has no files of its own on disk.
	if listed.Standard || listed.Dir == "" || strings.HasSuffix(strings.SplitN(listed.ImportPath, " ", 2)[0], ".test") {
		return
	}
	owner := strings.SplitN(listed.ImportPath, " ", 2)[0]
	for _, group := range [][]string{listed.GoFiles, listed.CgoFiles, listed.CFiles, listed.CXXFiles, listed.HFiles, listed.SFiles,
		listed.SysoFiles, listed.EmbedFiles, listed.TestGoFiles, listed.XTestGoFiles, listed.TestEmbedFiles, listed.XTestEmbedFiles} {
		for _, name := range group {
			files[owner+"/"+filepath.ToSlash(name)] = filepath.Join(listed.Dir, name)
		}
	}
	addModuleFiles(listed, files)
}

// A ListedClosure is one entry of a whole tree's `go list -deps -test -json`, with the transitive imports go lists
// for it: the resident builder (#d1gp9ze) reads one listing for every package of a tree, and names each closure's
// files from it with AddFiles, the one function Closure names them with.
type ListedClosure struct {
	listedPackage
	Deps  []string
	Error *struct{ Err string }
}

// AddFiles adds the entry's files to a closure, as Closure adds each package go lists for it.
func (listed ListedClosure) AddFiles(files map[string]string) {
	addListedFiles(listed.listedPackage, files)
}

// AddWorkspaceFiles adds the go.work the go command uses in the tree, and its go.work.sum, as Closure does.
func AddWorkspaceFiles(tree string, files map[string]string) error {
	return addWorkspaceFiles(tree, files)
}

// addModuleFiles adds a package's module's go.mod and, beside it, its go.sum: a go line or a replace moves the build
// even when every source file is the same.
func addModuleFiles(listed listedPackage, files map[string]string) {
	if listed.Module == nil || listed.Module.GoMod == "" {
		return
	}
	files[listed.Module.Path+"/go.mod"] = listed.Module.GoMod
	sum := filepath.Join(filepath.Dir(listed.Module.GoMod), "go.sum")
	if filepath.Base(listed.Module.GoMod) == "go.mod" && fileExists(sum) {
		files[listed.Module.Path+"/go.sum"] = sum
	}
}

// addWorkspaceFiles adds the go.work the go command uses in the tree, and its go.work.sum, when there is one.
func addWorkspaceFiles(tree string, files map[string]string) error {
	command := exec.Command("go", "env", "GOWORK")
	command.Dir = tree
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("go env GOWORK: %w", err)
	}
	work := strings.TrimSpace(string(output))
	if work == "" || work == "off" {
		return nil
	}
	files["go.work"] = work
	if sum := work + ".sum"; fileExists(sum) {
		files["go.work.sum"] = sum
	}
	return nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
