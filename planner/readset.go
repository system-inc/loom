package planner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A ReadSet is what a unit's traced runs read inside the tree's submodules (cohere, and TypeScript inside it) beyond
// the files its closure and declared reads already hold, tree-relative: every path a run opened, executed, stat'ed or
// missed, and every directory it listed. A unit keyed on one holds each of those paths by its content where its key
// would otherwise hold each submodule's commit, so a cohere bump moves only the keys of units that read what changed.
//
// It's sound because a set is recorded per code key (everything in the key but the submodule reads: the closure, the
// products, the declared reads in the superproject, the tools, the env, the selection). A unit's run is a function of
// its code and of what it reads, so on any tree where the code is the same and every path in the set reads the same,
// the run reads the same paths again, and nothing outside the set can change it. A traced run that reads beyond its
// key's set breaks that (a run that isn't deterministic, or a read the trace couldn't see): that is Loom's void, named,
// and the set grows by what the run read (CheckTrace, `loom reads-check`).
//
// Only tracked state is keyed: a path is its content when a submodule tracks it, "directory" when a tracked file sits
// under it, and "absent" otherwise, and a listing is its tracked children's names. So a file a run made inside a
// submodule keys as absent, as it is on every fresh checkout.
type ReadSet struct {
	Paths    []string `json:"paths"`
	Listings []string `json:"listings"`
}

// Id is the set's name, the sha256 of its canonical JSON: a key part (KeyParts.ReadSet), so the check knows exactly
// which paths the key held.
func (set ReadSet) Id() (string, error) {
	canonical, err := Canonical(set.normal())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// Empty says whether the set names nothing.
func (set ReadSet) Empty() bool {
	return len(set.Paths) == 0 && len(set.Listings) == 0
}

// Union is both sets' paths and listings. A set only grows at its code key: a run whose reads depend on data it read
// on another tree is keyed soundly by either set, and by both.
func (set ReadSet) Union(other ReadSet) ReadSet {
	return ReadSet{Paths: append(append([]string{}, set.Paths...), other.Paths...),
		Listings: append(append([]string{}, set.Listings...), other.Listings...)}.normal()
}

// normal is the set sorted and without repeats, never nil.
func (set ReadSet) normal() ReadSet {
	unique := func(paths []string) []string {
		seen := map[string]bool{}
		out := []string{}
		for _, name := range paths {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		sort.Strings(out)
		return out
	}
	return ReadSet{Paths: unique(set.Paths), Listings: unique(set.Listings)}
}

// ReadSetsDirectory is where read sets are kept, ~/.loom/read-sets unless a command names another (--read-sets):
// sets/<id>.json holds a set, units/<codeKey>.json the set its code key is keyed on now. Workshop's planner, builder
// and read check share it.
var ReadSetsDirectory = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".loom", "read-sets")
}()

// readSetUnit is units/<codeKey>.json: the set's id, and whose code key it is, for a person reading the directory.
type readSetUnit struct {
	ReadSet string `json:"readSet"`
	Kind    string `json:"kind"`
	Package string `json:"package"`
	Select  Select `json:"select"`
}

// LoadReadSet reads a set by its id, checking the id is its content's.
func LoadReadSet(directory, id string) (ReadSet, error) {
	if !Sha256Hex(id) {
		return ReadSet{}, fmt.Errorf("read set id %q isn't a sha256", id)
	}
	content, err := os.ReadFile(filepath.Join(directory, "sets", id+".json"))
	if err != nil {
		return ReadSet{}, fmt.Errorf("read set %s: %w", id, err)
	}
	var set ReadSet
	if err := json.Unmarshal(content, &set); err != nil {
		return ReadSet{}, fmt.Errorf("read set %s: %w", id, err)
	}
	set = set.normal()
	if actual, err := set.Id(); err != nil || actual != id {
		return ReadSet{}, fmt.Errorf("read set %s holds the set %s (%v)", id, actual, err)
	}
	return set, nil
}

// UnitReadSet is the set recorded for a code key and its id, or found false when none is.
func UnitReadSet(directory, codeKey string) (ReadSet, string, bool, error) {
	content, err := os.ReadFile(filepath.Join(directory, "units", codeKey+".json"))
	if os.IsNotExist(err) {
		return ReadSet{}, "", false, nil
	}
	if err != nil {
		return ReadSet{}, "", false, err
	}
	var unit readSetUnit
	if err := json.Unmarshal(content, &unit); err != nil {
		return ReadSet{}, "", false, fmt.Errorf("read set of %s: %w", codeKey, err)
	}
	set, err := LoadReadSet(directory, unit.ReadSet)
	if err != nil {
		return ReadSet{}, "", false, fmt.Errorf("read set of %s: %w", codeKey, err)
	}
	return set, unit.ReadSet, true, nil
}

// RecordReadSet records what a traced run of the unit read under its code key: the set already recorded there grown
// by measured, written before the code key names it, each file by a rename, so a reader never sees half of one. It
// returns the id the code key names now. Two records at once may keep only one run's paths; the other run's paths come
// back as a void the next time a traced run reads them, never as a stale key.
func RecordReadSet(directory, codeKey string, parts KeyParts, measured ReadSet) (string, error) {
	if !Sha256Hex(codeKey) {
		return "", fmt.Errorf("code key %q isn't a sha256", codeKey)
	}
	set := measured.normal()
	recorded, _, found, err := UnitReadSet(directory, codeKey)
	if err != nil {
		return "", err
	}
	if found {
		set = recorded.Union(set)
	}
	id, err := set.Id()
	if err != nil {
		return "", err
	}
	setJSON, err := Canonical(set)
	if err != nil {
		return "", err
	}
	if err := writeRenamed(filepath.Join(directory, "sets", id+".json"), setJSON); err != nil {
		return "", err
	}
	unitJSON, err := Canonical(readSetUnit{ReadSet: id, Kind: parts.Kind, Package: parts.Package, Select: parts.Select})
	if err != nil {
		return "", err
	}
	return id, writeRenamed(filepath.Join(directory, "units", codeKey+".json"), unitJSON)
}

func writeRenamed(target string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(target), ".writing-*")
	if err != nil {
		return err
	}
	if _, err := file.Write(append(content, '\n')); err != nil {
		file.Close()
		os.Remove(file.Name())
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(file.Name())
		return err
	}
	if err := os.Rename(file.Name(), target); err != nil {
		os.Remove(file.Name())
		return err
	}
	return nil
}

// codeKeyVersion prefixes a code key, so one is never a unit key.
const codeKeyVersion = "loom-unit-code-v1"

// CodeKey is the key a unit's read set is recorded under: its key parts with the reads part holding only its declared
// reads outside the submodules, and no read set.
func CodeKey(parts KeyParts, files [][2]string) (string, error) {
	reads, err := pairsHash(files)
	if err != nil {
		return "", err
	}
	parts.Reads, parts.ReadSet = reads, ""
	canonical, err := Canonical(parts)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(codeKeyVersion+"\n"), canonical...))
	return hex.EncodeToString(sum[:]), nil
}

// withReadSet keys a test or product unit on its code key's read set, when one is recorded: its reads part becomes
// its declared reads outside the submodules and the set's paths and listings by their state on this tree, and its
// read set part the set's id. A unit with none keeps the reads part that holds each declared submodule at its commit,
// and so does a unit whose set is empty and whose declared reads name no submodule, whose key that already is.
func withReadSet(tree string, parts *KeyParts, pairs readPairs) (string, error) {
	codeKey, err := CodeKey(*parts, pairs.files)
	if err != nil {
		return "", err
	}
	set, id, found, err := UnitReadSet(ReadSetsDirectory, codeKey)
	if err != nil || !found || set.Empty() && pairs.gitlinks == 0 {
		return codeKey, err
	}
	reads, err := readSetReads(tree, pairs.files, set)
	if err != nil {
		return "", fmt.Errorf("read set %s: %w", id, err)
	}
	parts.Reads, parts.ReadSet = reads, id
	return codeKey, nil
}

// readSetReads is the reads part of a unit keyed on a read set: the declared files' pairs and each of the set's paths
// and listings (a listing's name ends in /) with its state on the tree, sorted.
func readSetReads(tree string, files [][2]string, set ReadSet) (string, error) {
	index, err := submoduleIndexOf(tree)
	if err != nil {
		return "", err
	}
	pairs := append([][2]string{}, files...)
	for _, name := range set.Paths {
		state, err := index.pathState(name)
		if err != nil {
			return "", err
		}
		pairs = append(pairs, [2]string{name, state})
	}
	for _, name := range set.Listings {
		state, err := index.listingState(name)
		if err != nil {
			return "", err
		}
		pairs = append(pairs, [2]string{name + "/", state})
	}
	sort.Slice(pairs, func(left, right int) bool {
		if pairs[left][0] != pairs[right][0] {
			return pairs[left][0] < pairs[right][0]
		}
		return pairs[left][1] < pairs[right][1]
	})
	return pairsHash(pairs)
}

// A submoduleIndex is what the tree's submodules track, nested ones included, read from their own indexes: each
// tracked path's mode and object, and each directory's tracked children (a directory's name ends in /).
type submoduleIndex struct {
	tree     string
	gitlinks map[string]string // the superproject's gitlinks, path to commit
	entries  map[string][2]string
	children map[string]map[string]bool
}

// submoduleIndexes keeps the last index read for each tree, under the commits its gitlinks record: the planner keys
// every unit of a tree against one listing of cohere's 72k files.
var submoduleIndexes = struct {
	sync.Mutex
	byTree map[string]submoduleIndexEntry
}{byTree: map[string]submoduleIndexEntry{}}

type submoduleIndexEntry struct {
	signature string
	index     *submoduleIndex
}

func submoduleIndexOf(tree string) (*submoduleIndex, error) {
	gitlinks, err := Gitlinks(tree)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(gitlinks))
	for name := range gitlinks {
		names = append(names, name)
	}
	sort.Strings(names)
	signature := ""
	for _, name := range names {
		signature += name + " " + gitlinks[name] + "\n"
	}
	submoduleIndexes.Lock()
	cached, found := submoduleIndexes.byTree[tree]
	submoduleIndexes.Unlock()
	if found && cached.signature == signature {
		return cached.index, nil
	}
	index := &submoduleIndex{tree: tree, gitlinks: gitlinks, entries: map[string][2]string{}, children: map[string]map[string]bool{}}
	if len(names) > 0 {
		command := exec.Command("git", append([]string{"-C", tree, "ls-files", "-s", "-z", "--recurse-submodules", "--"}, names...)...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-files --recurse-submodules in %s: %w: %s", tree, err, strings.TrimSpace(stderr.String()))
		}
		for _, entry := range strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00") {
			// <mode> <object> <stage>\t<path>
			head, name, found := strings.Cut(entry, "\t")
			fields := strings.Fields(head)
			if !found || len(fields) != 3 {
				continue
			}
			index.entries[name] = [2]string{fields[0], fields[1]}
			for child := name; ; {
				parent, base := path.Dir(child), path.Base(child)
				if child != name {
					base += "/"
				}
				if index.children[parent] == nil {
					index.children[parent] = map[string]bool{}
				}
				known := index.children[parent][base]
				index.children[parent][base] = true
				// A directory already known has its ancestors already.
				if known || parent == "." || parent == "/" {
					break
				}
				child = parent
			}
		}
	}
	submoduleIndexes.Lock()
	submoduleIndexes.byTree[tree] = submoduleIndexEntry{signature: signature, index: index}
	submoduleIndexes.Unlock()
	return index, nil
}

// inSubmodule says whether a tree-relative path sits inside one of the tree's submodules.
func (index *submoduleIndex) inSubmodule(name string) bool {
	for gitlink := range index.gitlinks {
		if strings.HasPrefix(name, gitlink+"/") {
			return true
		}
	}
	return false
}

// checkedOut refuses a path under a submodule the tree hasn't checked out (ls-files lists it as a gitlink): its files
// can't be keyed, and keying them absent would hide what the run read.
func (index *submoduleIndex) checkedOut(name string) error {
	for candidate := name; candidate != "." && candidate != "/"; candidate = path.Dir(candidate) {
		if entry, found := index.entries[candidate]; found && entry[0] == "160000" && candidate != name {
			return fmt.Errorf("%s is inside the submodule %s, which isn't checked out", name, candidate)
		}
	}
	return nil
}

// pathState is a path's state on the tree: when a submodule tracks it, its mode and the object its index records (a
// file's content, a symlink's target, a nested gitlink's commit), so keying a 66k-file set reads no file; "directory"
// when a tracked file sits under it; else "absent". The planner's checkouts are clean, so the index is the checkout.
func (index *submoduleIndex) pathState(name string) (string, error) {
	if err := index.checkedOut(name); err != nil {
		return "", err
	}
	if entry, tracked := index.entries[name]; tracked {
		return entry[0] + " " + entry[1], nil
	}
	if index.children[name] != nil {
		return "directory", nil
	}
	return "absent", nil
}

// listingState is a listed directory's state: the sha256 of its tracked children's sorted names, a directory's
// ending in /.
func (index *submoduleIndex) listingState(name string) (string, error) {
	if err := index.checkedOut(name + "/."); err != nil {
		return "", err
	}
	children := make([]string, 0, len(index.children[name]))
	for child := range index.children[name] {
		children = append(children, child)
	}
	sort.Strings(children)
	sum := sha256.Sum256([]byte(strings.Join(children, "\n")))
	return "listing " + hex.EncodeToString(sum[:]), nil
}
