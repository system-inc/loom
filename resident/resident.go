// Package resident is Workshop's resident builder (#d1gp9ze, lever 6 of #s0cqqhk): it lives in `loom build-trees`'
// process and keeps the trees it has keyed warm in memory, so a candidate's tree costs only its diff from the nearest
// one. A tree held warm is three things: every tracked file by its git object (submodules recursed through their
// gitlinks), one listing of every test package's import closure (`go list -deps -test -json`), and each test
// package's closure key. A sha256 per git object is kept across trees, so a file is hashed once for every tree that
// holds it, and from git's object, never from a checkout's file.
//
// A candidate is read as git's listing of its tree, compared with each warm tree's; the nearest is the one fewest paths
// apart. Only what the diff can reach is asked again: the test packages, when a Go file or a module file changed; the
// closures of the packages whose listed directories hold a changed path (or a parent of one), and every closure when a
// module or workspace file changed. Every key is then hashed again from the object memo, so a key never rests on a
// copy of an earlier one.
//
// The rule (#d1gp9ze): the resident's answer for a tree equals a cold read's. Cold (cold.go) is that read, the one
// Workshop makes without a resident (builder.TestPackages, then planner.Closure per package), and Check compares the
// two, each package's closure key and each closure file's sha256 against the file's own bytes on disk.
package resident

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
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/planner"
)

// A Tree is one commit's tree as the resident holds it.
type Tree struct {
	Commit string
	// Hash is the commit's git tree hash, the one planner.TreeHash reads and the tree key names.
	Hash string
	// Packages are the tree's test packages, as builder.TestPackages lists them.
	Packages []planner.ProductTest
	// Closures are each test package's closure, by import path.
	Closures map[string]Closure
	// From is the warm tree's commit this one was read against, empty when it was read whole; Changed are the paths
	// whose objects differ between them, and Relisted the test packages whose closures were listed again.
	From     string
	Changed  []string
	Relisted []string
	// Seconds is how long keying it took.
	Seconds float64

	files     map[string]tracked
	gitlinks  map[string]string
	listing   map[string]planner.ListedClosure
	tests     map[string]testFiles
	workspace map[string]string
}

// testFiles are the files a package's tests embed, which go resolves only for a package named on its command line,
// never for one it lists as an import (its test files it lists for both): a listing's entries are kept without them,
// and each test package's own beside them, so a closure names what a package's tests embed only when that package is
// the one keyed, as planner.Closure's own listing does.
type testFiles struct {
	testEmbed, xtestEmbed []string
}

// A Closure is one test package's closure: its key, which equals planner.Closure's on the same tree, or why go
// couldn't list it.
type Closure struct {
	Key   string `json:"key,omitempty"`
	Error string `json:"error,omitempty"`
	// Entries are the listing's entries in the closure (the package, its test variants, its test main and their
	// imports), what an advance reads to decide whether a change reaches it.
	entries []string
	// files are the closure's files, each by its name (planner's), to its path: tree-relative in the tree, absolute
	// outside it (the module cache).
	files map[string]string
}

// A Resident holds warm trees and the sha256 of every git object it has hashed.
type Resident struct {
	// Keep is how many trees are held warm, the newest kept (zero: four).
	Keep int
	// stale is the mutant's path: an advance keeps the warm tree's object for it, the stale hash a resident must never
	// serve, which Check has to catch.
	stale string

	mutex   sync.Mutex
	trees   []*Tree
	objects map[string]string
	outside map[string]string
	modules string
}

// New is a Resident holding nothing yet.
func New() *Resident {
	return &Resident{objects: map[string]string{}, outside: map[string]string{}}
}

func (resident *Resident) keep() int {
	if resident.Keep > 0 {
		return resident.Keep
	}
	return 4
}

// moduleFiles are the files whose change can move any package's closure or listing, wherever they sit.
var moduleFiles = []string{"go.mod", "go.sum", "go.work", "go.work.sum"}

// Key keys the commit checked out at checkout, from the nearest warm tree, and holds it warm. The checkout must be
// at commit with no change to a tracked file, as planner.TreeHash requires.
func (resident *Resident) Key(checkout, commit string) (*Tree, error) {
	resident.mutex.Lock()
	defer resident.mutex.Unlock()
	started := time.Now()
	head, err := planner.LocalGit(checkout, "rev-parse", "HEAD").Output()
	if err != nil {
		return nil, fmt.Errorf("git rev-parse HEAD in %s: %w", checkout, gitError(err))
	}
	if strings.TrimSpace(string(head)) != commit {
		return nil, fmt.Errorf("%s is checked out at %s, not %s", checkout, strings.TrimSpace(string(head)), commit)
	}
	hash, err := planner.TreeHash(checkout)
	if err != nil {
		return nil, err
	}
	if resident.modules == "" {
		output, err := exec.Command("go", "env", "GOMODCACHE").Output()
		if err != nil {
			return nil, fmt.Errorf("go env GOMODCACHE: %w", err)
		}
		resident.modules = strings.TrimSpace(string(output))
	}
	tree := &Tree{Commit: commit, Hash: hash}
	if tree.files, tree.gitlinks, err = listTracked(checkout, commit); err != nil {
		return nil, err
	}
	from := resident.nearest(tree)
	if err := resident.read(checkout, tree, from); err != nil {
		return nil, err
	}
	tree.Seconds = time.Since(started).Seconds()
	resident.hold(tree)
	return tree, nil
}

// nearest is the warm tree fewest paths from tree, or nil when none is held.
func (resident *Resident) nearest(tree *Tree) *Tree {
	var nearest *Tree
	distance := 0
	for _, warm := range resident.trees {
		if count := len(changed(warm.files, tree.files)); nearest == nil || count < distance {
			nearest, distance = warm, count
		}
	}
	return nearest
}

// hold keeps tree warm, newest last, the oldest let go past Keep.
func (resident *Resident) hold(tree *Tree) {
	resident.trees = slices.DeleteFunc(resident.trees, func(warm *Tree) bool { return warm.Commit == tree.Commit })
	resident.trees = append(resident.trees, tree)
	if over := len(resident.trees) - resident.keep(); over > 0 {
		resident.trees = resident.trees[over:]
	}
}

// changed lists the paths whose mode or object differ between two trees' files, or that one holds and the other
// doesn't, sorted.
func changed(from, to map[string]tracked) []string {
	paths := []string{}
	for name, file := range to {
		if old, held := from[name]; !held || old.Mode != file.Mode || old.Object != file.Object {
			paths = append(paths, name)
		}
	}
	for name := range from {
		if _, held := to[name]; !held {
			paths = append(paths, name)
		}
	}
	sort.Strings(paths)
	return paths
}

// read fills tree's packages, listing and closures: whole when from is nil, else from from's, asking go again only
// for what the diff reaches.
func (resident *Resident) read(checkout string, tree, from *Tree) error {
	whole := from == nil
	goChanged := whole
	if from != nil {
		tree.From, tree.Changed = from.Commit, changed(from.files, tree.files)
		if resident.stale != "" && slices.Contains(tree.Changed, resident.stale) {
			if old, held := from.files[resident.stale]; held {
				tree.files[resident.stale] = old
			}
		}
		for _, name := range tree.Changed {
			if slices.Contains(moduleFiles, path.Base(name)) {
				whole = true
			}
			if strings.HasSuffix(name, ".go") {
				goChanged = true
			}
		}
	}
	var err error
	if goChanged || whole {
		if tree.Packages, err = builder.TestPackages(checkout); err != nil {
			return err
		}
	} else {
		tree.Packages = from.Packages
	}
	if whole {
		tree.workspace = map[string]string{}
		if err := planner.AddWorkspaceFiles(checkout, tree.workspace); err != nil {
			return err
		}
		for name, file := range tree.workspace {
			tree.workspace[name] = inTree(checkout, file)
		}
	} else {
		tree.workspace = from.workspace
	}
	roots := make([]string, 0, len(tree.Packages))
	for _, test := range tree.Packages {
		roots = append(roots, test.Package)
	}
	relist := roots
	tree.listing, tree.tests = map[string]planner.ListedClosure{}, map[string]testFiles{}
	if !whole {
		relist = reached(from, roots, tree.Changed)
		for name, entry := range from.listing {
			tree.listing[name] = entry
		}
		for name, files := range from.tests {
			tree.tests[name] = files
		}
	}
	tree.Relisted = relist
	listed, tests, failures, err := listClosures(checkout, relist)
	if err != nil {
		return err
	}
	for name, entry := range listed {
		tree.listing[name] = entry
	}
	for name, files := range tests {
		tree.tests[name] = files
	}
	tree.Closures = map[string]Closure{}
	for _, root := range roots {
		if failure, failed := failures[root]; failed {
			tree.Closures[root] = Closure{Error: failure}
			continue
		}
		if !slices.Contains(relist, root) {
			tree.Closures[root] = Closure{entries: from.Closures[root].entries, files: from.Closures[root].files}
			continue
		}
		closure, err := closureOf(checkout, tree, root)
		if err != nil {
			return err
		}
		tree.Closures[root] = closure
	}
	return resident.keyAll(checkout, tree)
}

// reached is the test packages whose closures a change can reach: those from didn't list or couldn't, and those
// holding a listed package whose directory is a changed path's directory or a parent of it (a package's files, and
// what it embeds, sit at or under its directory).
func reached(from *Tree, roots, paths []string) []string {
	directories := map[string]bool{}
	for _, name := range paths {
		for directory := path.Dir(name); ; directory = path.Dir(directory) {
			directories[directory] = true
			if directory == "." || directory == "/" {
				break
			}
		}
	}
	touched := map[string]bool{}
	for name, entry := range from.listing {
		if entry.Dir != "" && !filepath.IsAbs(entry.Dir) && directories[filepath.ToSlash(entry.Dir)] {
			touched[name] = true
		}
	}
	reached := []string{}
	for _, root := range roots {
		closure, held := from.Closures[root]
		if !held || closure.Error != "" || closure.entries == nil || slices.ContainsFunc(closure.entries, func(name string) bool { return touched[name] }) {
			reached = append(reached, root)
		}
	}
	return reached
}

// listClosures lists the roots' closures in one `go list -deps -test -json`, each entry's directory and module file
// tree-relative when it sits in the tree. When go refuses the whole listing (one root's import can't be found), each
// root is listed alone, and a root go refuses alone is named in failures with go's words, as planner.Closure fails.
func listClosures(checkout string, roots []string) (map[string]planner.ListedClosure, map[string]testFiles, map[string]string, error) {
	listing, tests, failures := map[string]planner.ListedClosure{}, map[string]testFiles{}, map[string]string{}
	if len(roots) == 0 {
		return listing, tests, failures, nil
	}
	if err := listInto(checkout, roots, listing, tests); err == nil {
		return listing, tests, failures, nil
	}
	for _, root := range roots {
		if err := listInto(checkout, []string{root}, listing, tests); err != nil {
			failures[root] = err.Error()
		}
	}
	return listing, tests, failures, nil
}

func listInto(checkout string, roots []string, listing map[string]planner.ListedClosure, tests map[string]testFiles) error {
	command := exec.Command("go", append([]string{"list", "-deps", "-test", "-json"}, roots...)...)
	command.Dir = checkout
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("go list -deps -test %s: %w: %s", strings.Join(roots, " "), err, strings.TrimSpace(stderr.String()))
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	for decoder.More() {
		var entry planner.ListedClosure
		if err := decoder.Decode(&entry); err != nil {
			return fmt.Errorf("go list output: %w", err)
		}
		if entry.Dir != "" {
			entry.Dir = inTree(checkout, entry.Dir)
		}
		if entry.Module != nil && entry.Module.GoMod != "" {
			module := *entry.Module
			module.GoMod = inTree(checkout, module.GoMod)
			entry.Module = &module
		}
		if !strings.Contains(entry.ImportPath, " ") {
			if slices.Contains(roots, entry.ImportPath) {
				tests[entry.ImportPath] = testFiles{testEmbed: entry.TestEmbedFiles, xtestEmbed: entry.XTestEmbedFiles}
			}
			entry.TestEmbedFiles, entry.XTestEmbedFiles = nil, nil
		}
		listing[entry.ImportPath] = entry
	}
	return nil
}

// inTree is file relative to checkout, slash-separated, when it sits in it, and file itself when it doesn't.
func inTree(checkout, file string) string {
	root, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		return file
	}
	resolved, err := filepath.EvalSymlinks(filepath.Dir(file))
	if err != nil {
		return file
	}
	relative, err := filepath.Rel(root, filepath.Join(resolved, filepath.Base(file)))
	if err != nil || !filepath.IsLocal(relative) {
		return file
	}
	return filepath.ToSlash(relative)
}

// closureOf names root's closure files from the tree's listing: the package, its test main, and everything go lists
// as either's imports, each named by planner's own AddFiles on the entry placed back in checkout (it looks for a go.sum
// beside each go.mod on disk, as a cold read does), then the workspace's files.
func closureOf(checkout string, tree *Tree, root string) (Closure, error) {
	entries := map[string]bool{}
	for _, name := range []string{root, root + ".test"} {
		entry, held := tree.listing[name]
		if !held {
			continue
		}
		entries[name] = true
		for _, dep := range entry.Deps {
			entries[dep] = true
		}
	}
	closure := Closure{files: map[string]string{}}
	for name := range entries {
		entry, held := tree.listing[name]
		if !held {
			return Closure{}, fmt.Errorf("%s's closure names %s, which the tree's listing doesn't hold", root, name)
		}
		closure.entries = append(closure.entries, name)
		if name == root {
			files := tree.tests[root]
			entry.TestEmbedFiles, entry.XTestEmbedFiles = files.testEmbed, files.xtestEmbed
		}
		if entry.Dir != "" && !filepath.IsAbs(entry.Dir) {
			entry.Dir = filepath.Join(checkout, filepath.FromSlash(entry.Dir))
		}
		if entry.Module != nil && entry.Module.GoMod != "" && !filepath.IsAbs(entry.Module.GoMod) {
			module := *entry.Module
			module.GoMod = filepath.Join(checkout, filepath.FromSlash(module.GoMod))
			entry.Module = &module
		}
		entry.AddFiles(closure.files)
	}
	sort.Strings(closure.entries)
	if len(closure.files) == 0 {
		return Closure{Error: fmt.Sprintf("go list found no files for %s", root)}, nil
	}
	// Every in-tree path was joined onto checkout above, so it comes off by its prefix.
	prefix := filepath.Clean(checkout) + string(filepath.Separator)
	for name, file := range closure.files {
		if relative, found := strings.CutPrefix(file, prefix); found {
			closure.files[name] = filepath.ToSlash(relative)
		}
	}
	for name, file := range tree.workspace {
		closure.files[name] = file
	}
	return closure, nil
}

// keyAll hashes every closure's key from the object memo, reading each object the memo lacks from git first, one
// batch per repository.
func (resident *Resident) keyAll(checkout string, tree *Tree) error {
	wanted := map[string]map[string]bool{}
	for _, closure := range tree.Closures {
		for _, file := range closure.files {
			held, tracked := tree.files[file]
			if filepath.IsAbs(file) || !tracked || held.symlink() {
				continue
			}
			if _, known := resident.objects[held.Object]; known {
				continue
			}
			if wanted[held.Repository] == nil {
				wanted[held.Repository] = map[string]bool{}
			}
			wanted[held.Repository][held.Object] = true
		}
	}
	for repository, set := range wanted {
		objects := make([]string, 0, len(set))
		for object := range set {
			objects = append(objects, object)
		}
		sort.Strings(objects)
		sums, err := readObjects(checkout, repository, objects)
		if err != nil {
			return err
		}
		for object, sum := range sums {
			resident.objects[object] = sum
		}
	}
	for root, closure := range tree.Closures {
		if closure.Error != "" {
			continue
		}
		key, err := resident.closureKey(checkout, tree, closure)
		if err != nil {
			return fmt.Errorf("%s's closure: %w", root, err)
		}
		closure.Key = key
		tree.Closures[root] = closure
	}
	return nil
}

// closureKey is planner.Closure's hash of a closure: its files' (name, sha256) pairs, sorted by name, canonical.
func (resident *Resident) closureKey(checkout string, tree *Tree, closure Closure) (string, error) {
	names := make([]string, 0, len(closure.files))
	for name := range closure.files {
		names = append(names, name)
	}
	sort.Strings(names)
	pairs := make([][2]string, 0, len(names))
	for _, name := range names {
		sum, err := resident.sum(checkout, tree, closure.files[name])
		if err != nil {
			return "", fmt.Errorf("closure file %s: %w", name, err)
		}
		pairs = append(pairs, [2]string{name, sum})
	}
	canonical, err := planner.Canonical(pairs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// sum is a closure file's sha256 as go reads it: a tracked file's from its object, a module's from the module cache
// (whose files never change under a path), once each. A symbolic link go follows, a file git doesn't track, and an
// absolute path outside the module cache are read from disk each time, never kept.
func (resident *Resident) sum(checkout string, tree *Tree, file string) (string, error) {
	if !filepath.IsAbs(file) {
		if held, tracked := tree.files[file]; tracked && !held.symlink() {
			if sum, known := resident.objects[held.Object]; known {
				return sum, nil
			}
			return "", fmt.Errorf("object %s of %s wasn't read", held.Object, file)
		}
		return diskSum(filepath.Join(checkout, filepath.FromSlash(file)))
	}
	cached := resident.modules != "" && strings.HasPrefix(file, resident.modules+string(filepath.Separator))
	if sum, known := resident.outside[file]; cached && known {
		return sum, nil
	}
	sum, err := diskSum(file)
	if err == nil && cached {
		resident.outside[file] = sum
	}
	return sum, err
}

func diskSum(file string) (string, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:]), nil
}

// Sum is a tracked path's sha256 in the tree, from the object memo, read from git when the memo lacks it: what a key
// over a product's read set (#vt46geg) asks for each file, with nothing read from disk. A path the tree doesn't track,
// or tracks as a symbolic link, isn't answered.
func (resident *Resident) Sum(checkout string, tree *Tree, file string) (string, bool, error) {
	resident.mutex.Lock()
	defer resident.mutex.Unlock()
	held, tracked := tree.files[file]
	if !tracked || held.symlink() {
		return "", false, nil
	}
	if sum, known := resident.objects[held.Object]; known {
		return sum, true, nil
	}
	sums, err := readObjects(checkout, held.Repository, []string{held.Object})
	if err != nil {
		return "", false, err
	}
	resident.objects[held.Object] = sums[held.Object]
	return sums[held.Object], true, nil
}

// ClosureFiles are a test package's closure files, each by its name (planner's) to its path: tree-relative in the
// tree, absolute outside it.
func (tree *Tree) ClosureFiles(importPath string) map[string]string {
	files := map[string]string{}
	for name, file := range tree.Closures[importPath].files {
		files[name] = file
	}
	return files
}

// Object is the git object the tree holds for a tracked path (a submodule's file by its own repository's), what a
// key over source chunks (lever 5 of #s0cqqhk) can name a file by without reading it.
func (tree *Tree) Object(file string) (string, bool) {
	held, tracked := tree.files[file]
	return held.Object, tracked
}

// Sums is Sum for one tree in the shape Builder's keys over read sets take (lever 2 of #s0cqqhk): a tracked path's
// sha256, from the memo or git, and false for a path the tree doesn't track or tracks as a symbolic link, or one git
// couldn't give, which the key's caller reads from disk instead.
func (resident *Resident) Sums(checkout string, tree *Tree) func(file string) (string, bool) {
	return func(file string) (string, bool) {
		sum, held, err := resident.Sum(checkout, tree, file)
		return sum, held && err == nil
	}
}
