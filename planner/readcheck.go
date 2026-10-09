package planner

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A Finding is one read a unit's traced run made outside its declared inputs: a tracked file the key doesn't hold,
// so an edit to it would reuse a stale verdict. It goes to the unit's owner, who declares it (a reads line) or stops
// reading it.
type Finding struct {
	UnitKey string `json:"unitKey,omitempty"`
	Package string `json:"package"`
	Path    string `json:"path"`
	Sha256  string `json:"sha256"` // a gitlink's is its recorded commit
}

// traceCall is one strace-format syscall line: an optional pid ("123 " or "[pid 123] "), the call, its arguments and
// its result. The tracer runs strace -f --decode-fds=path -e trace=open,openat,openat2,execve, so a directory
// descriptor prints as 5</abs/dir> and AT_FDCWD as AT_FDCWD</abs/cwd>.
var traceCall = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?(open|openat|openat2|execve)\((.*)\)\s+=\s+(-?\d+)`)

// unfinished and resumed are strace's split form when two processes' calls interleave.
var (
	unfinished = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?(.*)\s<unfinished \.\.\.>$`)
	resumed    = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?<\.\.\. (\w+) resumed>(.*)$`)
)

// TracedReads is every file a trace shows read: a successful open, openat or openat2 that isn't write-only, or a
// successful execve. Each is an absolute path; a relative one resolves against its decoded directory descriptor, or
// against directory when the trace didn't decode it.
func TracedReads(trace io.Reader, directory string) ([]string, error) {
	pending := map[string]string{}
	read := map[string]bool{}
	scanner := bufio.NewScanner(trace)
	scanner.Buffer(make([]byte, 1<<20), 1<<24)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if match := unfinished.FindStringSubmatch(line); match != nil {
			pending[match[1]+match[2]] = match[3]
			continue
		}
		if match := resumed.FindStringSubmatch(line); match != nil {
			start, found := pending[match[1]+match[2]]
			if !found {
				continue
			}
			delete(pending, match[1]+match[2])
			line = strings.TrimSpace(match[1] + match[2] + " " + start + match[4])
		}
		match := traceCall.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		call, arguments, result := match[3], match[4], match[5]
		if result, err := strconv.Atoi(result); err != nil || result < 0 {
			continue
		}
		path, err := readPath(call, arguments, directory)
		if err != nil {
			return nil, fmt.Errorf("trace line %q: %w", line, err)
		}
		if path != "" {
			read[path] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	reads := make([]string, 0, len(read))
	for path := range read {
		reads = append(reads, path)
	}
	sort.Strings(reads)
	return reads, nil
}

// descriptor is a decoded directory argument: AT_FDCWD</dir> or 5</dir>.
var descriptor = regexp.MustCompile(`^(?:AT_FDCWD|\d+)(?:<(.*?)>)?,\s*`)

// readPath is the absolute path a call read, or "" for a write-only open.
func readPath(call, arguments, directory string) (string, error) {
	base := directory
	if call == "openat" || call == "openat2" {
		match := descriptor.FindStringSubmatch(arguments)
		if match == nil {
			return "", fmt.Errorf("no directory argument")
		}
		if match[1] != "" {
			base = match[1]
		}
		arguments = arguments[len(match[0]):]
	}
	name, rest, err := quoted(arguments)
	if err != nil {
		return "", err
	}
	if call != "execve" && (strings.Contains(rest, "O_WRONLY") || strings.Contains(rest, "O_PATH")) {
		return "", nil
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(base, name)
	}
	return filepath.Clean(name), nil
}

// quoted reads strace's leading C string argument and returns it with what follows.
func quoted(arguments string) (string, string, error) {
	if !strings.HasPrefix(arguments, `"`) {
		return "", "", fmt.Errorf("no path argument")
	}
	for index := 1; index < len(arguments); index++ {
		switch arguments[index] {
		case '\\':
			index++
		case '"':
			name, err := strconv.Unquote(arguments[:index+1])
			if err != nil {
				return "", "", fmt.Errorf("path %s: %w", arguments[:index+1], err)
			}
			return name, arguments[index+1:], nil
		}
	}
	return "", "", fmt.Errorf("unterminated path")
}

// CheckReads compares a unit's traced reads with its declared inputs: the tracked files of its closure, of its
// declared compiler packages' closures (its products, built locally while Builder is stubbed) and of its declared
// reads. Every tracked file read outside them is a finding. Reads outside the tree (the toolchain, caches, /tmp)
// and untracked files the run made are never findings.
func CheckReads(tree, gateTools string, unit Unit, compilerPackages []string, traced []string) ([]Finding, error) {
	declared := map[string]bool{}
	for _, importPath := range append([]string{unit.Package}, compilerPackages...) {
		files, err := ClosureFiles(tree, importPath)
		if err != nil {
			return nil, err
		}
		for _, file := range files {
			declared[file] = true
		}
	}
	reads, err := DeclaredReads(tree, gateTools, unit.Directory)
	if err != nil {
		return nil, err
	}
	for _, file := range reads {
		declared[file] = true
	}
	listing, err := exec.Command("git", "-C", tree, "ls-files", "-z").Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", tree, err)
	}
	tracked := map[string]bool{}
	for _, file := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		tracked[file] = true
	}
	gitlinks, err := Gitlinks(tree)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return nil, err
	}
	findings := []Finding{}
	seen := map[string]bool{}
	for _, path := range traced {
		relative, ok := inTree(root, tree, path)
		if !ok || declared[relative] {
			continue
		}
		// A read inside a submodule that no closure holds reads the gitlink: the submodule at its recorded commit is
		// what a reads line can declare.
		for gitlink := range gitlinks {
			if strings.HasPrefix(relative, gitlink+"/") {
				relative = gitlink
			}
		}
		if !tracked[relative] || declared[relative] || seen[relative] {
			continue
		}
		seen[relative] = true
		digest := ""
		if commit, found := gitlinks[relative]; found {
			digest = commit
		} else {
			content, err := os.ReadFile(filepath.Join(tree, filepath.FromSlash(relative)))
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", relative, err)
			}
			sum := sha256.Sum256(content)
			digest = hex.EncodeToString(sum[:])
		}
		findings = append(findings, Finding{Package: unit.Package, Path: relative, Sha256: digest})
	}
	sort.Slice(findings, func(left, right int) bool { return findings[left].Path < findings[right].Path })
	return findings, nil
}

// inTree is path's repo-relative name when it sits in the tree, under the tree's own spelling or its resolved one.
func inTree(root, tree, path string) (string, bool) {
	for _, base := range []string{tree, root} {
		relative, err := filepath.Rel(base, path)
		if err == nil && relative != ".." && !strings.HasPrefix(relative, "../") && !filepath.IsAbs(relative) {
			return filepath.ToSlash(relative), true
		}
	}
	return "", false
}

// CheckUnitReads is the read check for one test unit by import path, as PlanTree keys it: its directory from the
// module path, its compiler packages from the tree's compiler-dependencies.json, its reads from trace.
func CheckUnitReads(tree, gateTools, importPath string, trace io.Reader) ([]Finding, error) {
	module, err := modulePath(tree)
	if err != nil {
		return nil, err
	}
	declared, err := compilerDeclarations(tree)
	if err != nil {
		return nil, err
	}
	directory := strings.TrimPrefix(strings.TrimPrefix(importPath, module), "/")
	compilers := []string{}
	for _, input := range declared[directory] {
		compilers = append(compilers, module+"/"+input)
	}
	traced, err := TracedReads(trace, filepath.Join(tree, filepath.FromSlash(directory)))
	if err != nil {
		return nil, err
	}
	unit := Unit{Kind: "test", Package: importPath, Directory: directory}
	return CheckReads(tree, gateTools, unit, compilers, traced)
}
