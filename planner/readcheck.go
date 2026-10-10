package planner

import (
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
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
	present := map[string]bool{}
	for _, path := range accesses.Present {
		present[path] = true
	}
	inKey, keyedGitlinks := map[string]bool{}, map[string]bool{}
	if keyed != nil {
		for _, gitlink := range keyed.Gitlinks {
			keyedGitlinks[gitlink] = true
		}
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
			if uncleaned, dotted := uncleanedRelative(root, tree, path); dotted {
				// A .. after a name stays for the index to resolve as the kernel did.
				relative, ok = uncleaned, true
			}
			if !ok || declared[relative] {
				continue
			}
			// A path in a submodule, or one that resolves into one through a symlink (a superproject testdata link
			// into cohere, say), is the read set's: its key follows the links to what they lead to.
			resolved, err := index.resolve(relative)
			if err != nil {
				return nil, ReadSet{}, err
			}
			if resolved.touches {
				if group.listing {
					measured.Listings = append(measured.Listings, relative)
				} else {
					measured.Paths = append(measured.Paths, relative)
				}
				// A path the run found that the submodule doesn't track (and the run didn't make) has no state a key
				// can hold but the submodule's commit.
				untracked := ""
				if _, isTracked := index.entries[resolved.resolved]; (group.read || group.listing || present[path]) && !resolved.outside &&
					!isTracked && index.children[resolved.resolved] == nil {
					untracked = index.submoduleOf(resolved.resolved)
				}
				if untracked != "" {
					measured.Gitlinks = append(measured.Gitlinks, untracked)
				}
				if keyed != nil {
					if untracked != "" {
						if !keyedGitlinks[untracked] && !seen["\x00"+relative] {
							seen["\x00"+relative] = true
							findings = append(findings, Finding{Package: unit.Package, Path: relative, BeyondKey: true, ReadSet: keyedId,
								Listing: group.listing, State: "untracked in " + untracked + ", whose commit the key doesn't hold"})
						}
					} else if err := beyond(relative, group.listing); err != nil {
						return nil, ReadSet{}, err
					}
					continue
				}
			}
			if !group.read {
				// A lookup or listing outside the submodules is the declared reads' to hold, as today: only reads are.
				continue
			}
			if _, dotted := uncleanedRelative(root, tree, path); dotted {
				// The declared reads name the file the kernel reached.
				if resolved.outside || declared[resolved.resolved] {
					continue
				}
				relative = resolved.resolved
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

// uncleanedRelative is an uncleaned path's (uncleanedPath's) repo-relative name, and whether it is one: a path that
// sits in the tree and still holds a .. component.
func uncleanedRelative(root, tree, path string) (string, bool) {
	if !slices.Contains(strings.Split(path, "/"), "..") {
		return "", false
	}
	for _, base := range []string{tree, root} {
		if relative, found := strings.CutPrefix(path, strings.TrimSuffix(filepath.ToSlash(base), "/")+"/"); found {
			return relative, true
		}
	}
	return "", false
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
		if ReadSetsDirectory == "" {
			return TraceCheck{}, fmt.Errorf("unit %s is keyed on read set %.12s, and no read sets directory is named", parts.Package, parts.ReadSet)
		}
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
