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
// Only tracked state is keyed: a path is its content when the tree tracks it, "directory" when a tracked file sits
// under it, and "absent" otherwise, and a listing is its tracked children's names. What a run made itself is never
// its input (TraceAccesses). A file the run found in a submodule that the submodule doesn't track (what an install put
// there, say) has no tracked state, so Gitlinks names that submodule and the key holds its commit, as it did before
// read sets: the files an install puts in cohere follow from cohere's commit as far as any key could say.
type ReadSet struct {
	Paths    []string `json:"paths"`
	Listings []string `json:"listings"`
	Gitlinks []string `json:"gitlinks,omitempty"`
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
	return len(set.Paths) == 0 && len(set.Listings) == 0 && len(set.Gitlinks) == 0
}

// Union is both sets' paths and listings. A set only grows at its code key: a run whose reads depend on data it read
// on another tree is keyed soundly by either set, and by both.
func (set ReadSet) Union(other ReadSet) ReadSet {
	return ReadSet{Paths: append(append([]string{}, set.Paths...), other.Paths...),
		Listings: append(append([]string{}, set.Listings...), other.Listings...),
		Gitlinks: append(append([]string{}, set.Gitlinks...), other.Gitlinks...)}.normal()
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
	normal := ReadSet{Paths: unique(set.Paths), Listings: unique(set.Listings)}
	if len(set.Gitlinks) > 0 {
		normal.Gitlinks = unique(set.Gitlinks)
	}
	return normal
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
	for _, gitlink := range set.Gitlinks {
		// A submodule a run found untracked files in keys at its commit; a NUL can't start a path's name.
		commit, found := index.gitlinks[gitlink]
		if !found {
			commit = "absent"
		}
		pairs = append(pairs, [2]string{"\x00gitlink " + gitlink, commit})
	}
	sort.Slice(pairs, func(left, right int) bool {
		if pairs[left][0] != pairs[right][0] {
			return pairs[left][0] < pairs[right][0]
		}
		return pairs[left][1] < pairs[right][1]
	})
	return pairsHash(pairs)
}

// A submoduleIndex is what the tree tracks, its submodules' files included (nested ones too, read from their own
// indexes): each tracked path's mode and object, and each directory's tracked children (a directory's name ends in /).
// Paths resolve through it as the kernel resolves them, through every tracked symlink on the way.
type submoduleIndex struct {
	tree     string
	gitlinks map[string]string // the superproject's gitlinks, path to commit
	entries  map[string][2]string
	children map[string]map[string]bool
}

// submoduleIndexes keeps the last index read for each tree, under its superproject index's listing (which holds the
// gitlinks' commits): the planner keys every unit of a tree against one listing of cohere's 72k files.
var submoduleIndexes = struct {
	sync.Mutex
	byTree map[string]submoduleIndexEntry
}{byTree: map[string]submoduleIndexEntry{}}

type submoduleIndexEntry struct {
	signature string
	index     *submoduleIndex
}

func submoduleIndexOf(tree string) (*submoduleIndex, error) {
	superproject, err := lsFiles(tree)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(superproject)
	signature := hex.EncodeToString(sum[:])
	submoduleIndexes.Lock()
	cached, found := submoduleIndexes.byTree[tree]
	submoduleIndexes.Unlock()
	if found && cached.signature == signature {
		return cached.index, nil
	}
	index := &submoduleIndex{tree: tree, gitlinks: map[string]string{}, entries: map[string][2]string{}, children: map[string]map[string]bool{}}
	names := []string{}
	parseLsFiles(superproject, func(mode, object, name string) {
		if mode == "160000" {
			// A checked-out submodule's files come from its own index below; one that isn't stays a gitlink there.
			index.gitlinks[name] = object
			names = append(names, name)
			return
		}
		index.add(mode, object, name)
	})
	if len(names) > 0 {
		sort.Strings(names)
		command := exec.Command("git", append([]string{"-C", tree, "ls-files", "-s", "-z", "--recurse-submodules", "--"}, names...)...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		output, err := command.Output()
		if err != nil {
			return nil, fmt.Errorf("git ls-files --recurse-submodules in %s: %w: %s", tree, err, strings.TrimSpace(stderr.String()))
		}
		parseLsFiles(output, index.add)
	}
	submoduleIndexes.Lock()
	submoduleIndexes.byTree[tree] = submoduleIndexEntry{signature: signature, index: index}
	submoduleIndexes.Unlock()
	return index, nil
}

func lsFiles(tree string) ([]byte, error) {
	command := exec.Command("git", "-C", tree, "ls-files", "-s", "-z")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files -s in %s: %w: %s", tree, err, strings.TrimSpace(stderr.String()))
	}
	return output, nil
}

// parseLsFiles calls each with every entry of `git ls-files -s -z`'s output.
func parseLsFiles(output []byte, each func(mode, object, name string)) {
	for _, entry := range strings.Split(strings.TrimSuffix(string(output), "\x00"), "\x00") {
		// <mode> <object> <stage>\t<path>
		head, name, found := strings.Cut(entry, "\t")
		fields := strings.Fields(head)
		if found && len(fields) == 3 {
			each(fields[0], fields[1], name)
		}
	}
}

func (index *submoduleIndex) add(mode, object, name string) {
	index.entries[name] = [2]string{mode, object}
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

// inSubmodule says whether a tree-relative path is one of the tree's submodules or sits inside one.
func (index *submoduleIndex) inSubmodule(name string) bool {
	return index.submoduleOf(name) != ""
}

// submoduleOf is the superproject's gitlink a tree-relative path is or sits inside, or "".
func (index *submoduleIndex) submoduleOf(name string) string {
	for gitlink := range index.gitlinks {
		if name == gitlink || strings.HasPrefix(name, gitlink+"/") {
			return gitlink
		}
	}
	return ""
}

// maxSymlinks is how many symlinks a resolution follows before it's refused, as the kernel's ELOOP.
const maxSymlinks = 40

// A resolution is where a tree-relative path leads on the tree: each tracked symlink it went through ("<link> ->
// <target>"), the path it ends at (tree-relative, or absolute when a link left the tree), and whether any step was
// inside a submodule.
type resolution struct {
	links    []string
	resolved string
	outside  bool
	touches  bool
}

// resolve follows a path through the tree's tracked symlinks, every component and the last too, as open and stat
// do: a symlinked directory or file keys its link and where it leads. A link's target is read from the checkout,
// which the index's object for it names. A path under a submodule that isn't checked out is refused, since its files
// can't be keyed.
func (index *submoduleIndex) resolve(name string) (resolution, error) {
	result := resolution{touches: index.inSubmodule(name)}
	parts := strings.Split(name, "/")
	resolved := ""
	for followed := 0; len(parts) > 0; {
		part := parts[0]
		parts = parts[1:]
		switch part {
		case "", ".":
			continue
		case "..":
			if resolved == "" {
				// Above the tree's root: the rest is outside it.
				result.resolved, result.outside = path.Clean("/"+strings.Join(append([]string{index.tree, ".."}, parts...), "/")), true
				return result, nil
			}
			resolved = path.Dir(resolved)
			if resolved == "." {
				resolved = ""
			}
			continue
		}
		candidate := path.Join(resolved, part)
		if index.inSubmodule(candidate) {
			result.touches = true
		}
		entry, tracked := index.entries[candidate]
		if tracked && entry[0] == "160000" && len(parts) > 0 {
			return resolution{}, fmt.Errorf("%s is inside the submodule %s, which isn't checked out", name, candidate)
		}
		if !tracked || entry[0] != "120000" {
			resolved = candidate
			continue
		}
		followed++
		if followed > maxSymlinks {
			return resolution{}, fmt.Errorf("%s: more than %d symlinks", name, maxSymlinks)
		}
		target, err := os.Readlink(filepath.Join(index.tree, filepath.FromSlash(candidate)))
		if err != nil {
			return resolution{}, fmt.Errorf("symlink %s: %w", candidate, err)
		}
		result.links = append(result.links, candidate+" -> "+target)
		if filepath.IsAbs(target) {
			relative, inside := inTree(index.tree, index.tree, target)
			if !inside {
				result.resolved, result.outside = path.Join(append([]string{target}, parts...)...), true
				return result, nil
			}
			resolved, target = "", relative
		}
		parts = append(strings.Split(filepath.ToSlash(target), "/"), parts...)
	}
	result.resolved = resolved
	if index.inSubmodule(resolved) {
		result.touches = true
	}
	return result, nil
}

// pathState is a path's state on the tree: the symlinks it resolves through, then where it ends: when the tree tracks
// it, its mode and the object its index records (a file's content, a symlink's target, a nested gitlink's commit), so
// keying a 66k-file set reads no file; "directory" when a tracked file sits under it; "outside" and the path when a
// link left the tree; else "absent". The planner's checkouts are clean, so the index is the checkout.
func (index *submoduleIndex) pathState(name string) (string, error) {
	result, err := index.resolve(name)
	if err != nil {
		return "", err
	}
	state := ""
	switch entry, tracked := index.entries[result.resolved]; {
	case result.outside:
		state = "outside " + result.resolved
	case tracked:
		state = entry[0] + " " + entry[1]
	case index.children[result.resolved] != nil:
		state = "directory"
	default:
		state = "absent"
	}
	return strings.Join(append(result.links, state), "\n"), nil
}

// listingState is a listed directory's state: the symlinks it resolves through, then the sha256 of the tracked
// children's sorted names where it ends, a directory's ending in /.
func (index *submoduleIndex) listingState(name string) (string, error) {
	result, err := index.resolve(name)
	if err != nil {
		return "", err
	}
	if entry, tracked := index.entries[result.resolved]; tracked && entry[0] == "160000" {
		return "", fmt.Errorf("%s lists the submodule %s, which isn't checked out", name, result.resolved)
	}
	children := []string{}
	if !result.outside {
		for child := range index.children[result.resolved] {
			children = append(children, child)
		}
	}
	sort.Strings(children)
	sum := sha256.Sum256([]byte(strings.Join(children, "\n")))
	return strings.Join(append(result.links, "listing "+hex.EncodeToString(sum[:])), "\n"), nil
}
