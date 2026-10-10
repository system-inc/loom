package planner

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A Finding is one read a unit's traced run made outside what its key holds. Most are a tracked file nothing declares,
// so an edit to it would reuse a stale verdict: it goes to the unit's owner, who declares it (a reads line) or stops
// reading it. One with BeyondKey is a path inside a submodule that the read set the unit was keyed on doesn't name:
// that is Loom's void, not the owner's, and the set grows by it.
type Finding struct {
	UnitKey string `json:"unitKey,omitempty"`
	Package string `json:"package"`
	Path    string `json:"path"`
	Sha256  string `json:"sha256"` // a gitlink's is its recorded commit; empty beyond the key, where State says it
	// BeyondKey marks a read beyond the key's read set (ReadSet, its id); Listing marks a directory the run listed;
	// State is the path's state on the tree, as a key on a set naming it would hold it.
	BeyondKey bool   `json:"beyondKey,omitempty"`
	ReadSet   string `json:"readSet,omitempty"`
	Listing   bool   `json:"listing,omitempty"`
	State     string `json:"state,omitempty"`
}

// TraceCalls is the strace call list a traced run is recorded with, for a read set and its check:
//
//	strace -f --decode-fds=path -e trace=<TraceCalls> -o <trace> <the unit's test binary> ...
//
// so a directory descriptor prints as 5</abs/dir> and AT_FDCWD as AT_FDCWD</abs/cwd>. A trace of only open, openat,
// openat2 and execve serves the declared-reads check, never a read set: it can't show a run that stat'ed, missed or
// listed a path.
const TraceCalls = "open,openat,openat2,execve,stat,lstat,newfstatat,fstatat64,statx,access,faccessat,faccessat2,readlink,readlinkat,getdents,getdents64"

// traceCall is one strace-format syscall line: an optional pid ("123 " or "[pid 123] "), the call, its arguments and
// its result.
var traceCall = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?([a-z0-9_]+)\((.*)\)\s+=\s+(-?\d+)`)

// unfinished and resumed are strace's split form when two processes' calls interleave.
var (
	unfinished = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?(.*)\s<unfinished \.\.\.>$`)
	resumed    = regexp.MustCompile(`^(?:\[pid\s+(\d+)\]\s+|(\d+)\s+)?<\.\.\. (\w+) resumed>(.*)$`)
)

// The calls a trace names a path with: the first argument a path, or a directory descriptor and then a path.
var (
	pathCalls      = map[string]bool{"open": true, "execve": true, "stat": true, "lstat": true, "access": true, "readlink": true}
	directoryCalls = map[string]bool{"openat": true, "openat2": true, "newfstatat": true, "fstatat64": true, "statx": true,
		"faccessat": true, "faccessat2": true, "readlinkat": true}
	listingCalls = map[string]bool{"getdents": true, "getdents64": true}
)

// TracedAccesses is every path a trace shows a run reaching, absolute. Reads are what it opened for reading or
// executed and got; Lookups every other path it named, stat'ed, probed or missed (a failed open of any kind but a
// write); Listings every directory it listed.
type TracedAccesses struct {
	Reads    []string
	Lookups  []string
	Listings []string
}

// TracedReads is every file a trace shows read: a successful open, openat or openat2 that isn't write-only, or a
// successful execve. Each is an absolute path; a relative one resolves against its decoded directory descriptor, or
// against directory when the trace didn't decode it.
func TracedReads(trace io.Reader, directory string) ([]string, error) {
	accesses, err := TraceAccesses(trace, directory)
	return accesses.Reads, err
}

// TraceAccesses reads a trace into the paths its run reached (TracedAccesses). A path resolves as TracedReads says; a
// listing names its descriptor's decoded path, and one the trace didn't decode is refused, since its directory is
// unknown.
func TraceAccesses(trace io.Reader, directory string) (TracedAccesses, error) {
	pending := map[string]string{}
	reads, lookups, listings := map[string]bool{}, map[string]bool{}, map[string]bool{}
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
		call, arguments := match[3], match[4]
		result, err := strconv.Atoi(match[5])
		if err != nil {
			continue
		}
		switch {
		case listingCalls[call]:
			if result < 0 {
				continue
			}
			descriptorMatch := descriptor.FindStringSubmatch(arguments + ", ")
			if descriptorMatch == nil || descriptorMatch[1] == "" {
				return TracedAccesses{}, fmt.Errorf("trace line %q: a listing whose directory wasn't decoded (--decode-fds=path)", line)
			}
			listings[filepath.Clean(descriptorMatch[1])] = true
		case pathCalls[call] || directoryCalls[call]:
			path, write, err := callPath(call, arguments, directory)
			if err != nil {
				return TracedAccesses{}, fmt.Errorf("trace line %q: %w", line, err)
			}
			opened := call == "open" || call == "openat" || call == "openat2"
			switch {
			case write:
				// A write is never a read, and a failed one names nothing the run read either.
			case (opened || call == "execve") && result >= 0 && !strings.Contains(arguments, "O_PATH"):
				reads[path] = true
			default:
				lookups[path] = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return TracedAccesses{}, err
	}
	return TracedAccesses{Reads: sortedKeys(reads), Lookups: sortedKeys(lookups), Listings: sortedKeys(listings)}, nil
}

func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// descriptor is a decoded directory argument: AT_FDCWD</dir> or 5</dir>.
var descriptor = regexp.MustCompile(`^(?:AT_FDCWD|\d+)(?:<(.*?)>)?,\s*`)

// callPath is the absolute path a call named, and whether it was an open for writing only. An empty path after a
// descriptor (AT_EMPTY_PATH) is the descriptor's own.
func callPath(call, arguments, directory string) (string, bool, error) {
	base := directory
	if directoryCalls[call] {
		match := descriptor.FindStringSubmatch(arguments)
		if match == nil {
			return "", false, fmt.Errorf("no directory argument")
		}
		if match[1] != "" {
			base = match[1]
		}
		arguments = arguments[len(match[0]):]
	}
	name, rest, err := quoted(arguments)
	if err != nil {
		return "", false, err
	}
	write := (call == "open" || call == "openat" || call == "openat2") && strings.Contains(rest, "O_WRONLY")
	if name == "" {
		name = base
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(base, name)
	}
	return filepath.Clean(name), write, nil
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
	findings, _, err := checkAccesses(tree, gateTools, unit, compilerPackages, TracedAccesses{Reads: traced}, nil, "")
	return findings, err
}

// checkAccesses is the read check of one traced run, and the read set it measured: every path the run reached inside
// a submodule that its closures and declared reads don't hold, grown from keyed. With no keyed set, a read inside a
// submodule that no closure holds reads the gitlink, as CheckReads says. With one (its id keyedId), any path the run
// reached inside a submodule that the closures and the set don't hold is a finding beyond the key.
func checkAccesses(tree, gateTools string, unit Unit, compilerPackages []string, accesses TracedAccesses, keyed *ReadSet, keyedId string) ([]Finding, ReadSet, error) {
	declared := map[string]bool{}
	for _, importPath := range append([]string{unit.Package}, compilerPackages...) {
		files, err := ClosureFiles(tree, importPath)
		if err != nil {
			return nil, ReadSet{}, err
		}
		for _, file := range files {
			declared[file] = true
		}
	}
	reads, err := DeclaredReads(tree, gateTools, unit.Directory)
	if err != nil {
		return nil, ReadSet{}, err
	}
	for _, file := range reads {
		declared[file] = true
	}
	listing, err := exec.Command("git", "-C", tree, "ls-files", "-z").Output()
	if err != nil {
		return nil, ReadSet{}, fmt.Errorf("git ls-files in %s: %w", tree, err)
	}
	tracked := map[string]bool{}
	for _, file := range strings.Split(strings.TrimSuffix(string(listing), "\x00"), "\x00") {
		tracked[file] = true
	}
	index, err := submoduleIndexOf(tree)
	if err != nil {
		return nil, ReadSet{}, err
	}
	root, err := filepath.EvalSymlinks(tree)
	if err != nil {
		return nil, ReadSet{}, err
	}
	inKey := map[string]bool{}
	if keyed != nil {
		for _, name := range keyed.Paths {
			inKey[name] = true
		}
		for _, name := range keyed.Listings {
			inKey[name+"/"] = true
		}
	}
	findings := []Finding{}
	seen := map[string]bool{}
	measured := ReadSet{Paths: []string{}, Listings: []string{}}
	beyond := func(relative string, listing bool) error {
		name := relative
		if listing {
			name += "/"
		}
		if keyed == nil || inKey[name] || seen[name] {
			return nil
		}
		seen[name] = true
		finding := Finding{Package: unit.Package, Path: relative, BeyondKey: true, ReadSet: keyedId, Listing: listing}
		var err error
		if listing {
			finding.State, err = index.listingState(relative)
		} else {
			finding.State, err = index.pathState(relative)
		}
		findings = append(findings, finding)
		return err
	}
	for _, group := range []struct {
		paths   []string
		read    bool
		listing bool
	}{{accesses.Reads, true, false}, {accesses.Lookups, false, false}, {accesses.Listings, false, true}} {
		for _, path := range group.paths {
			relative, ok := inTree(root, tree, path)
			if !ok || declared[relative] {
				continue
			}
			if index.inSubmodule(relative) {
				if group.listing {
					measured.Listings = append(measured.Listings, relative)
				} else {
					measured.Paths = append(measured.Paths, relative)
				}
				if keyed != nil {
					if err := beyond(relative, group.listing); err != nil {
						return nil, ReadSet{}, err
					}
					continue
				}
			}
			if !group.read {
				// A lookup or listing outside the submodules is the declared reads' to hold, as today: only reads are.
				continue
			}
			// A read inside a submodule that no closure holds, on a key with no read set, reads the gitlink: the
			// submodule at its recorded commit is what a reads line can declare.
			for gitlink := range index.gitlinks {
				if strings.HasPrefix(relative, gitlink+"/") {
					relative = gitlink
				}
			}
			if !tracked[relative] || declared[relative] || seen[relative] {
				continue
			}
			seen[relative] = true
			digest := ""
			if commit, found := index.gitlinks[relative]; found {
				digest = commit
			} else {
				digest, err = fileDigest(filepath.Join(tree, filepath.FromSlash(relative)))
				if err != nil {
					return nil, ReadSet{}, fmt.Errorf("read %s: %w", relative, err)
				}
			}
			findings = append(findings, Finding{Package: unit.Package, Path: relative, Sha256: digest})
		}
	}
	sort.Slice(findings, func(left, right int) bool {
		if findings[left].Path != findings[right].Path {
			return findings[left].Path < findings[right].Path
		}
		return !findings[left].Listing && findings[right].Listing
	})
	if keyed != nil {
		measured = keyed.Union(measured)
	}
	return findings, measured.normal(), nil
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

// A TraceCheck is what one traced run of a planned unit says: its findings, the code key its read set is recorded
// under, and the set the run measured (the keyed set grown by every submodule path the run reached outside its
// closures and declared reads).
type TraceCheck struct {
	Findings []Finding
	CodeKey  string
	Measured ReadSet
}

// Beyond is the findings beyond the key's read set: Loom's void.
func (check TraceCheck) Beyond() []Finding {
	beyond := []Finding{}
	for _, finding := range check.Findings {
		if finding.BeyondKey {
			beyond = append(beyond, finding)
		}
	}
	return beyond
}

// CheckTrace is the read check of a traced run of a planned unit (a test or product unit's key parts, as the plan
// posted them) on the tree it ran on, traced with TraceCalls. The unit is keyed again on the tree, its env carried over
// as KeyAt does, and on the read set its parts name, from ReadSetsDirectory; parts that don't key the same are refused,
// so a trace is never checked against another tree's key. Recording what it measured is the caller's
// (RecordReadSet), as is the void a finding beyond the key is.
func CheckTrace(tree, gateTools string, parts KeyParts, trace io.Reader) (TraceCheck, error) {
	if parts.Kind != "test" && parts.Kind != "product" {
		return TraceCheck{}, fmt.Errorf("a %q unit has no read set: only test and product units read the tree by their tests", parts.Kind)
	}
	at, err := baseKeyAt(tree, gateTools, parts)
	if err != nil {
		return TraceCheck{}, err
	}
	codeKey, err := CodeKey(at.parts, at.pairs.files)
	if err != nil {
		return TraceCheck{}, err
	}
	var keyed *ReadSet
	expected := at.parts
	if parts.ReadSet != "" {
		set, err := LoadReadSet(ReadSetsDirectory, parts.ReadSet)
		if err != nil {
			return TraceCheck{}, err
		}
		keyed = &set
		if expected.Reads, err = readSetReads(tree, at.pairs.files, set); err != nil {
			return TraceCheck{}, err
		}
		expected.ReadSet = parts.ReadSet
	}
	want, err := UnitKey(parts)
	if err != nil {
		return TraceCheck{}, err
	}
	if got, err := UnitKey(expected); err != nil || got != want {
		return TraceCheck{}, fmt.Errorf("unit %s keys %.12s on this tree, not its parts' %.12s (%v): the trace isn't checked against another tree's key", parts.Package, got, want, err)
	}
	accesses, err := TraceAccesses(trace, filepath.Join(tree, filepath.FromSlash(at.unit.Directory)))
	if err != nil {
		return TraceCheck{}, err
	}
	findings, measured, err := checkAccesses(tree, gateTools, at.unit, at.compilers, accesses, keyed, parts.ReadSet)
	if err != nil {
		return TraceCheck{}, err
	}
	return TraceCheck{Findings: findings, CodeKey: codeKey, Measured: measured}, nil
}
