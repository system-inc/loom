package planner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
)

// readSetFixture is a tree whose package p links a submodule's Go package (sub, by a replace) and declares the whole
// submodule read (reads p sub), as adamic's units declare cohere. The submodule holds data p's tests read, a file
// they don't, a directory they list, and a nested submodule (deep) with a file of its own. bump commits an edit inside
// the submodule and moves the tree's gitlink to it, as a cohere bump does.
type readSetFixture struct {
	tree, gateTools, sub string
	unit                 Unit
}

func newReadSetFixture(t *testing.T) readSetFixture {
	t.Helper()
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "protocol.file.allow")
	t.Setenv("GIT_CONFIG_VALUE_0", "always")
	root := t.TempDir()
	fixture := readSetFixture{tree: filepath.Join(root, "tree"), gateTools: filepath.Join(root, "tools"), sub: filepath.Join(root, "sub")}
	deep := filepath.Join(root, "deep")
	writeFiles(t, deep, map[string]string{"deep.txt": "deep\n"})
	gitIn(t, root, "init", "-q", deep)
	gitIn(t, deep, "add", ".")
	gitIn(t, deep, "commit", "-q", "-m", "deep")
	writeFiles(t, fixture.sub, map[string]string{"go.mod": "module example.com/shim\n\ngo 1.22\n", "shim.go": "package shim\n",
		"data.txt": "data\n", "other.txt": "other\n", "dir/a.txt": "a\n"})
	gitIn(t, root, "init", "-q", fixture.sub)
	gitIn(t, fixture.sub, "submodule", "add", "-q", deep, "deep")
	gitIn(t, fixture.sub, "add", ".")
	gitIn(t, fixture.sub, "commit", "-q", "-m", "one")
	writeFiles(t, fixture.tree, map[string]string{
		"go.mod":      "module example.com/readset\n\ngo 1.22\n\nrequire example.com/shim v0.0.0\n\nreplace example.com/shim => ./sub\n",
		"p/p.go":      "package p\n\nimport _ \"example.com/shim\"\n",
		"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestP(t *testing.T) {}\n",
	})
	gitIn(t, root, "init", "-q", fixture.tree)
	gitIn(t, fixture.tree, "submodule", "add", "-q", fixture.sub, "sub")
	gitIn(t, fixture.tree, "submodule", "update", "-q", "--init", "--recursive")
	gitIn(t, fixture.tree, "add", ".")
	writeFiles(t, fixture.gateTools, map[string]string{"cloud/fast-gate/executors.txt": "reads p sub\n"})
	fixture.unit = Unit{Kind: "test", Package: "example.com/readset/p", Directory: "p", Environment: GateEnvironment, Products: []string{}}
	return fixture
}

func gitIn(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	if output, err := exec.Command("git", append([]string{"-C", directory, "-c", "user.name=t", "-c", "user.email=t@t"}, arguments...)...).CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", arguments, err, output)
	}
}

// bump writes files inside the tree's submodule checkout (a name under deep/ lands in the nested one), commits them
// there, and records the new commits in the tree, as a cohere bump does.
func (fixture readSetFixture) bump(t *testing.T, files map[string]string) {
	t.Helper()
	checkout := filepath.Join(fixture.tree, "sub")
	writeFiles(t, checkout, files)
	for name := range files {
		if strings.HasPrefix(name, "deep/") {
			gitIn(t, filepath.Join(checkout, "deep"), "add", ".")
			gitIn(t, filepath.Join(checkout, "deep"), "commit", "-q", "-m", "bump")
		}
	}
	gitIn(t, checkout, "add", ".")
	gitIn(t, checkout, "commit", "-q", "-m", "bump")
	gitIn(t, fixture.tree, "add", "sub")
}

func (fixture readSetFixture) key(t *testing.T) (KeyParts, string) {
	t.Helper()
	parts, err := KeyFor(fixture.tree, fixture.gateTools, fixture.unit, Tools{Go: "go1.27.0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	key, err := UnitKey(parts)
	if err != nil {
		t.Fatal(err)
	}
	return parts, key
}

// record records set under the unit's code key on the tree as it stands.
func (fixture readSetFixture) record(t *testing.T, set ReadSet) string {
	t.Helper()
	parts, pairs, err := baseKey(fixture.tree, fixture.gateTools, fixture.unit, Tools{Go: "go1.27.0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	codeKey, err := CodeKey(parts, pairs.files)
	if err != nil {
		t.Fatal(err)
	}
	id, err := RecordReadSet(ReadSetsDirectory, codeKey, parts, set)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func useReadSets(t *testing.T) {
	t.Helper()
	saved := ReadSetsDirectory
	ReadSetsDirectory = t.TempDir()
	t.Cleanup(func() { ReadSetsDirectory = saved })
}

// With no read set a unit keys the submodule's commit, so any bump moves it (coarse, as today). With one, a bump
// moves the key only when it changes what the set names: a file's content, a missed path appearing, a listed
// directory's tracked names, a path inside a nested submodule. Mutants that each fail it: the gitlink still keyed
// beside the set; a set path keyed by name and not content; listings left out of the key; a missed path keyed as
// nothing; the nested submodule's files left out of the index.
func TestAReadSetKeysOnlyTheSubmodulePathsItNames(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	parts, coarse := fixture.key(t)
	if parts.ReadSet != "" {
		t.Fatalf("a unit with no read set was keyed on %q", parts.ReadSet)
	}
	fixture.bump(t, map[string]string{"other.txt": "other, bumped\n"})
	if _, moved := fixture.key(t); moved == coarse {
		t.Fatal("with no read set, a submodule bump left the key where it was: the gitlink isn't keyed")
	}
	id := fixture.record(t, ReadSet{Paths: []string{"sub/data.txt", "sub/missing.txt", "sub/deep/deep.txt"}, Listings: []string{"sub/dir"}})
	parts, base := fixture.key(t)
	if parts.ReadSet != id {
		t.Fatalf("keyed on read set %q, want the recorded %s", parts.ReadSet, id)
	}
	for _, edit := range []struct {
		name  string
		files map[string]string
		moves bool
	}{
		{"a file the set doesn't name", map[string]string{"other.txt": "other, bumped again\n"}, false},
		{"a new file outside every listed directory", map[string]string{"elsewhere/new.txt": "new\n"}, false},
		{"a file the set names", map[string]string{"data.txt": "data, bumped\n"}, true},
		{"a missed path appearing", map[string]string{"missing.txt": "here now\n"}, true},
		{"a new name in a listed directory", map[string]string{"dir/b.txt": "b\n"}, true},
		{"an edit to a listed directory's file", map[string]string{"dir/a.txt": "a, bumped\n"}, false},
		{"a file the set names in the nested submodule", map[string]string{"deep/deep.txt": "deep, bumped\n"}, true},
	} {
		fixture.bump(t, edit.files)
		parts, key := fixture.key(t)
		if parts.ReadSet != id {
			t.Fatalf("after %s: keyed on read set %q, want %s", edit.name, parts.ReadSet, id)
		}
		if moved := key != base; moved != edit.moves {
			t.Errorf("bumping %s: the key moved %v, want %v", edit.name, moved, edit.moves)
		}
		base = key
	}
}

// A set's paths key by the objects the submodule's index records, never by reading the files, so a set of cohere's 66k
// TypeScript files keys in a map lookup each (a file read each cost about 1.8 s a unit). An edit the submodule hasn't
// staged doesn't move the key; a committed one does. Mutant that fails it: a set path keyed by reading its file.
func TestAReadSetKeysTheIndexsObjectsNotTheFiles(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	fixture.record(t, ReadSet{Paths: []string{"sub/data.txt"}})
	_, base := fixture.key(t)
	writeFiles(t, filepath.Join(fixture.tree, "sub"), map[string]string{"data.txt": "unstaged\n"})
	if _, key := fixture.key(t); key != base {
		t.Fatal("an edit the submodule's index doesn't hold moved the key: the set's files were read")
	}
	fixture.bump(t, map[string]string{"data.txt": "committed\n"})
	if _, key := fixture.key(t); key == base {
		t.Fatal("a committed edit to a set's file left the key where it was")
	}
}

// A read set is the code key's: an edit to the unit's closure (or its declared reads, products, tools, env or
// selection) is another code key, with no set, so the unit keys the submodule's commit again until a traced run
// records one. Mutant that fails it: a set looked up by the unit's package and selection alone.
func TestAReadSetIsKeptPerCodeKey(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	id := fixture.record(t, ReadSet{Paths: []string{"sub/data.txt"}, Listings: []string{}})
	if parts, _ := fixture.key(t); parts.ReadSet != id {
		t.Fatalf("keyed on read set %q, want %s", parts.ReadSet, id)
	}
	writeFiles(t, fixture.tree, map[string]string{"p/p.go": "package p\n\nimport _ \"example.com/shim\"\n\nvar Changed = 1\n"})
	if parts, _ := fixture.key(t); parts.ReadSet != "" {
		t.Fatalf("a changed closure was keyed on the old code's read set %q", parts.ReadSet)
	}
	fixture.unit.Run = "^TestP$"
	if parts, _ := fixture.key(t); parts.ReadSet != "" {
		t.Fatalf("another selection was keyed on another unit's read set %q", parts.ReadSet)
	}
}

// Each product test is its own unit with its own read set, though ProductKeys keys a package's product tests from one
// closure: the test whose set is recorded is keyed on it, its sibling on the gitlink. Mutant that fails it: the set
// looked up once per package.
func TestEachProductTestIsKeyedOnItsOwnReadSet(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	writeFiles(t, fixture.tree, map[string]string{"p/p_test.go": "package p\n\nimport \"testing\"\n\nfunc TestProduct_A(t *testing.T) {}\n\nfunc TestProduct_B(t *testing.T) {}\n"})
	unit := Unit{Kind: "product", Package: fixture.unit.Package, Directory: "p", Environment: GateEnvironment}
	parts, pairs, err := baseKey(fixture.tree, fixture.gateTools, unit, Tools{Go: "go1.27.0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parts.Select = Select{Run: "^TestProduct_A$"}
	codeKey, err := CodeKey(parts, pairs.files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordReadSet(ReadSetsDirectory, codeKey, parts, ReadSet{Paths: []string{"sub/data.txt"}}); err != nil {
		t.Fatal(err)
	}
	tests := []ProductTest{{Package: unit.Package, Directory: "p", Test: "TestProduct_A"}, {Package: unit.Package, Directory: "p", Test: "TestProduct_B"}}
	keys := func() map[ProductTest]string {
		t.Helper()
		keys, err := ProductKeys(fixture.tree, fixture.gateTools, Tools{Go: "go1.27.0"}, tests, nil)
		if err != nil {
			t.Fatal(err)
		}
		return keys
	}
	before := keys()
	fixture.bump(t, map[string]string{"other.txt": "other, bumped\n"})
	after := keys()
	if before[tests[0]] != after[tests[0]] {
		t.Error("a bump outside TestProduct_A's read set moved its key")
	}
	if before[tests[1]] == after[tests[1]] {
		t.Error("TestProduct_B, with no read set, kept its key over a submodule bump")
	}
}

// A unit that reads nothing in the submodules and declares none keeps the key it had when its empty set is recorded,
// so measuring every unit moves none whose reads it doesn't change. Mutant that fails it: every recorded set keyed.
func TestAnEmptyReadSetLeavesAKeyWithNoSubmoduleWhereItWas(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	writeFiles(t, fixture.gateTools, map[string]string{"cloud/fast-gate/executors.txt": "# none\n"})
	_, before := fixture.key(t)
	fixture.record(t, ReadSet{})
	if parts, after := fixture.key(t); after != before || parts.ReadSet != "" {
		t.Fatalf("an empty read set moved a key that held no submodule (%q)", parts.ReadSet)
	}
}

// The store grows a code key's set and never shrinks it, and names each set by its content; a set file whose content
// isn't its name is refused.
func TestRecordReadSetGrowsTheSetAndChecksItsName(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	codeKey := strings.Repeat("a", 64)
	first, err := RecordReadSet(directory, codeKey, KeyParts{Kind: "test", Package: "p"}, ReadSet{Paths: []string{"sub/b", "sub/a", "sub/a"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := RecordReadSet(directory, codeKey, KeyParts{Kind: "test", Package: "p"}, ReadSet{Paths: []string{"sub/c"}, Listings: []string{"sub"}})
	if err != nil {
		t.Fatal(err)
	}
	set, id, found, err := UnitReadSet(directory, codeKey)
	if err != nil || !found || id != second || first == second {
		t.Fatalf("the code key's set is %s (found %v, %v), want the grown %s", id, found, err, second)
	}
	if want := (ReadSet{Paths: []string{"sub/a", "sub/b", "sub/c"}, Listings: []string{"sub"}}); !reflect.DeepEqual(set, want) {
		t.Fatalf("the grown set is %+v, want %+v", set, want)
	}
	os.WriteFile(filepath.Join(directory, "sets", second+".json"), []byte(`{"paths":["sub/a"],"listings":[]}`), 0o644)
	if _, _, _, err := UnitReadSet(directory, codeKey); err == nil {
		t.Fatal("a set file whose content isn't its name was read")
	}
}

// symlink commits a symlink at name (tree-relative) to target, in the submodule or in the superproject.
func (fixture readSetFixture) symlink(t *testing.T, name, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(fixture.tree, filepath.FromSlash(name))); err != nil {
		t.Fatal(err)
	}
	if inSub, found := strings.CutPrefix(name, "sub/"); found {
		checkout := filepath.Join(fixture.tree, "sub")
		gitIn(t, checkout, "add", inSub)
		gitIn(t, checkout, "commit", "-q", "-m", "link")
		gitIn(t, fixture.tree, "add", "sub")
		return
	}
	gitIn(t, fixture.tree, "add", name)
}

// The review's proofs (unit-reads review, finding 1): a read through a symlink keys where the link leads. A file
// opened or stat'ed through a submodule symlink, through a symlinked directory, through a superproject symlink into the
// submodule, or past a symlink by .. (which the kernel resolves from where the link leads, not from its name): each is
// measured by the name the run used, resolved through the tree's links, and an open by the file the kernel opened too
// (the descriptor's decoded path), so a change to the file it reached moves the key. Mutants that each fail it: a set
// path keyed by its name without following its links; the opened descriptor's path not recorded; a superproject path
// that resolves into a submodule left out of the set.
func TestAReadThroughASymlinkKeysWhereItLeads(t *testing.T) {
	for _, check := range []struct {
		name, link, target, call, opened, read, changed string
	}{
		{"an open through a submodule symlink to a file", "sub/link", "data.txt", "open", "../sub/link", "sub/data.txt", "data.txt"},
		{"a stat through a submodule symlink to a file", "sub/link", "data.txt", "stat", "../sub/link", "", "data.txt"},
		{"a stat through a symlinked directory", "sub/dl", "dir", "stat", "../sub/dl/a.txt", "", "dir/a.txt"},
		{"an open through a superproject symlink into the submodule", "p/testdata", "../sub/dir", "open", "testdata/a.txt", "sub/dir/a.txt", "dir/a.txt"},
		{"a stat through a superproject symlink into the submodule", "p/testdata", "../sub/dir", "stat", "testdata/a.txt", "", "dir/a.txt"},
		{"an open by .. past a symlink", "sub/dl", "dir/inner", "open", "../sub/dl/../a.txt", "sub/dir/a.txt", "dir/a.txt"},
	} {
		t.Run(check.name, func(t *testing.T) {
			useReadSets(t)
			fixture := newReadSetFixture(t)
			fixture.bump(t, map[string]string{"dir/inner/x.txt": "x\n"})
			fixture.symlink(t, check.link, check.target)
			coarse, _ := fixture.key(t)
			trace := `9 newfstatat(AT_FDCWD<` + filepath.Join(fixture.tree, "p") + `>, "` + check.opened + `", {st_mode=S_IFREG|0644, ...}, 0) = 0` + "\n"
			if check.call == "open" {
				trace = `9 openat(AT_FDCWD<` + filepath.Join(fixture.tree, "p") + `>, "` + check.opened + `", O_RDONLY|O_CLOEXEC) = 3<` +
					filepath.Join(fixture.tree, check.read) + ">\n"
			}
			first, err := CheckTrace(fixture.tree, fixture.gateTools, coarse, strings.NewReader(trace))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RecordReadSet(ReadSetsDirectory, first.CodeKey, coarse, first.Measured); err != nil {
				t.Fatal(err)
			}
			keyed, before := fixture.key(t)
			if keyed.ReadSet == "" {
				t.Fatalf("no read set keyed the unit; measured %+v", first.Measured)
			}
			// Keyed on what it reached, not on the submodule's commit: a bump of a file it never reached keeps the key.
			fixture.bump(t, map[string]string{"other.txt": "unrelated\n"})
			if _, unrelated := fixture.key(t); unrelated != before {
				t.Fatalf("a bump of a file the run never reached moved the key (measured %+v)", first.Measured)
			}
			fixture.bump(t, map[string]string{check.changed: "changed\n"})
			if _, after := fixture.key(t); after == before {
				t.Fatalf("sub/%s, read through %s, changed and the key stayed (measured %+v)", check.changed, check.link, first.Measured)
			}
		})
	}
}

// The review's proof (finding 3): a file a submodule doesn't track, found there by the run (what an install put in
// cohere), has no tracked state, so the set names the submodule and the key holds its commit, as it did before read
// sets; a bump of the submodule (its lockfile, outside the set) then moves the key. A run keyed on a set that doesn't
// hold that commit and finds such a file is beyond its key. Keeping the commit is the sounder choice than voiding every
// such read: a void would recur on every run of a unit that reads installed files and never key it, where the commit
// keys it exactly as soundly as the submodule's key always did. Mutants that each fail it: a found lookup not counted
// as found; the submodule's commit left out of the key; an untracked file a set without the commit holds as absent.
func TestAnUntrackedSubmoduleFileKeysTheSubmodulesCommit(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	fixture.bump(t, map[string]string{".gitignore": "node_modules/\n"})
	writeFiles(t, filepath.Join(fixture.tree, "sub"), map[string]string{"node_modules/x/index.js": "v1\n"})
	trace := `9 newfstatat(AT_FDCWD<` + filepath.Join(fixture.tree, "p") + `>, "../sub/node_modules/x/index.js", {st_mode=S_IFREG|0644, ...}, 0) = 0` + "\n" +
		`9 openat(AT_FDCWD<` + filepath.Join(fixture.tree, "p") + `>, "../sub/data.txt", O_RDONLY) = 3` + "\n"
	check := func(parts KeyParts) TraceCheck {
		t.Helper()
		result, err := CheckTrace(fixture.tree, fixture.gateTools, parts, strings.NewReader(trace))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	coarse, _ := fixture.key(t)
	first := check(coarse)
	if want := []string{"sub"}; !reflect.DeepEqual(first.Measured.Gitlinks, want) {
		t.Fatalf("measured %+v, want the submodule named for its untracked file", first.Measured)
	}
	if _, err := RecordReadSet(ReadSetsDirectory, first.CodeKey, coarse, first.Measured); err != nil {
		t.Fatal(err)
	}
	keyed, before := fixture.key(t)
	if again := check(keyed); len(again.Findings) != 0 {
		t.Fatalf("a run keyed on its submodule's commit is beyond it: %+v", again.Findings)
	}
	fixture.bump(t, map[string]string{"package-lock.json": "x@2\n"})
	if _, after := fixture.key(t); after == before {
		t.Fatal("a submodule bump that changes what it installs left the key of a unit reading an installed file")
	}
	// Another selection is another code key, here keyed on a set that names the path but not the commit.
	fixture.unit.Run = "^TestP$"
	parts, pairs, err := baseKey(fixture.tree, fixture.gateTools, fixture.unit, Tools{Go: "go1.27.0"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	codeKey, err := CodeKey(parts, pairs.files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordReadSet(ReadSetsDirectory, codeKey, parts, ReadSet{Paths: []string{"sub/data.txt", "sub/node_modules/x/index.js"}}); err != nil {
		t.Fatal(err)
	}
	narrow, _ := fixture.key(t)
	beyond := check(narrow).Beyond()
	if len(beyond) != 1 || beyond[0].Path != "sub/node_modules/x/index.js" || !strings.HasPrefix(beyond[0].State, "untracked in sub") {
		t.Fatalf("an untracked file read on a key without its submodule's commit: beyond %+v", beyond)
	}
}

// The review's proof (finding 6): two records at once under one code key, each with its own run's path, keep both.
// Mutant that fails it: the records not locked.
func TestConcurrentRecordsKeepEveryPath(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	for round := 0; round < 50; round++ {
		codeKey := fmt.Sprintf("%064d", round)
		var group sync.WaitGroup
		for _, name := range []string{"sub/a", "sub/b", "sub/c"} {
			group.Add(1)
			go func() {
				defer group.Done()
				if _, err := RecordReadSet(directory, codeKey, KeyParts{Kind: "test"}, ReadSet{Paths: []string{name}}); err != nil {
					t.Error(err)
				}
			}()
		}
		group.Wait()
		if set, _, _, err := UnitReadSet(directory, codeKey); err != nil || len(set.Paths) != 3 {
			t.Fatalf("round %d: three records at once kept %v (%v)", round, set.Paths, err)
		}
	}
}

// Read sets are off unless a command names their directory, so the shipped planner keys every unit as before them.
// Mutant that fails it: the planner keyed on ~/.loom/read-sets by default.
func TestReadSetsAreOffByDefault(t *testing.T) {
	if ReadSetsDirectory != "" {
		t.Fatalf("read sets are on by default, from %s", ReadSetsDirectory)
	}
}

// A verdict under a key on a read set is reused only once a traced run of that key came back clean (unit-reads
// review, finding 2). A set is measured: a run whose reads depend on what it read (A says which B to read) could pass
// under a key whose set lacks the B it read, and a later change to only that B would reuse the pass. A run that read
// only what its key holds reads the same wherever the key agrees, so one clean trace at the key makes every verdict
// under it good. A key with no set reuses as before. Mutants that each fail it: a set-keyed verdict reused with no
// clean trace; a clean trace never letting it be reused.
func TestAVerdictOnAReadSetIsReusedOnlyAfterACleanTrace(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	tools := Tools{Go: "go1.27.0"}
	plan := func(index MemoryIndex) PlannedResult {
		t.Helper()
		results, err := PlanTree(fixture.tree, fixture.gateTools, tools, index, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) != 1 {
			t.Fatalf("planned %d units, want p's", len(results))
		}
		return results[0]
	}
	passed := func(result PlannedResult) MemoryIndex {
		return MemoryIndex{result.UnitKey: Verdict{UnitKey: result.UnitKey, Status: "passed", Run: "earlier"}}
	}
	coarse := plan(MemoryIndex{})
	if again := plan(passed(coarse)); coarse.KeyParts.ReadSet != "" || again.Decision != "reuse" {
		t.Fatalf("a unit with no read set: decision %s (%s), want reuse", again.Decision, again.Reason)
	}
	parts, pairs, err := baseKey(fixture.tree, fixture.gateTools, Unit{Kind: "test", Package: fixture.unit.Package, Directory: "p",
		Environment: GateEnvironment, Products: []string{}}, tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	codeKey, err := CodeKey(parts, pairs.files)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordReadSet(ReadSetsDirectory, codeKey, parts, ReadSet{Paths: []string{"sub/data.txt"}}); err != nil {
		t.Fatal(err)
	}
	keyed := plan(MemoryIndex{})
	if keyed.KeyParts.ReadSet == "" {
		t.Fatal("the recorded set didn't key the unit")
	}
	if unchecked := plan(passed(keyed)); unchecked.Decision != "run" || !strings.Contains(unchecked.Reason, "come back clean") {
		t.Fatalf("a set-keyed pass with no clean trace: decision %s (%s), want run", unchecked.Decision, unchecked.Reason)
	}
	if err := RecordTraceClean(ReadSetsDirectory, keyed.UnitKey); err != nil {
		t.Fatal(err)
	}
	if checked := plan(passed(keyed)); checked.Decision != "reuse" {
		t.Fatalf("a set-keyed pass whose key traced clean: decision %s (%s), want reuse", checked.Decision, checked.Reason)
	}
	if err := ForgetTraceClean(ReadSetsDirectory, keyed.UnitKey); err != nil {
		t.Fatal(err)
	}
	if forgotten := plan(passed(keyed)); forgotten.Decision != "run" {
		t.Fatalf("a key whose clean trace was taken back still reuses: %s", forgotten.Decision)
	}
}

// The second review's proof (finding 1): a stat that misses through `sub/dl/../x`, dl a link to dir/inner, probed
// sub/dir/x, as the kernel walks `..` from where the link led. The name stays uncleaned for the index to resolve the
// same way, so when sub/dir/x appears the key moves, and a stat of the same name on that tree reads it present. Mutant
// that fails it: a name with `..` after a name cleaned as text.
func TestAMissThroughDotDotPastALinkKeysThePathTheKernelProbed(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	fixture.bump(t, map[string]string{"dir/inner/keep.txt": "k\n"})
	fixture.symlink(t, "sub/dl", "dir/inner")
	coarse, _ := fixture.key(t)
	trace := `9 newfstatat(AT_FDCWD<` + filepath.Join(fixture.tree, "p") + `>, "../sub/dl/../x", 0x7ffd, 0) = -1 ENOENT (No such file or directory)` + "\n"
	first, err := CheckTrace(fixture.tree, fixture.gateTools, coarse, strings.NewReader(trace))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RecordReadSet(ReadSetsDirectory, first.CodeKey, coarse, first.Measured); err != nil {
		t.Fatal(err)
	}
	_, before := fixture.key(t)
	fixture.bump(t, map[string]string{"x": "where text cleaning would look\n"})
	if _, unrelated := fixture.key(t); unrelated != before {
		t.Fatalf("sub/x, which the kernel never probed, moved the key (measured %+v)", first.Measured)
	}
	fixture.bump(t, map[string]string{"dir/x": "now here\n"})
	if _, after := fixture.key(t); after == before {
		t.Fatalf("sub/dir/x, the path the kernel probed, appeared and the key stayed (measured %+v)", first.Measured)
	}
}

// A listing names every entry the run saw (the second review's finding 3): when the listed directory holds an entry
// the submodule doesn't track and the run didn't make, the key holds the submodule's commit, as for any untracked file
// the run found. A listing of a directory holding only tracked entries, the run's own files, or the submodule's .git
// keys as before, the submodule's own root included, which is measured though a reads line declares it. Mutants that
// each fail it: a listing's untracked entries never looked at; the run's own files counted as untracked; the
// submodule's .git counted as untracked; a declared submodule's root never measured.
func TestAListingThatSawUntrackedEntriesKeysTheSubmodulesCommit(t *testing.T) {
	useReadSets(t)
	fixture := newReadSetFixture(t)
	listing := func(directory string) string {
		return `9 getdents64(5<` + filepath.Join(fixture.tree, directory) + `>, 0x55 /* 4 entries */, 32768) = 96` + "\n"
	}
	measure := func(trace string) ReadSet {
		t.Helper()
		parts, _ := fixture.key(t)
		check, err := CheckTrace(fixture.tree, fixture.gateTools, parts, strings.NewReader(trace))
		if err != nil {
			t.Fatal(err)
		}
		return check.Measured
	}
	made := `9 openat(AT_FDCWD<` + fixture.tree + `>, "sub/dir/out.txt", O_WRONLY|O_CREAT|O_TRUNC, 0644) = 3` + "\n"
	writeFiles(t, filepath.Join(fixture.tree, "sub"), map[string]string{"dir/out.txt": "the run's own\n"})
	for _, clean := range []string{"sub/dir", "sub"} {
		// The submodule's own root, though a reads line declares it, is measured: a set replaces the declaration.
		if set := measure(made + listing(clean)); len(set.Gitlinks) != 0 || !slices.Contains(set.Listings, clean) {
			t.Fatalf("a listing of %s's tracked entries, the run's own and .git: %+v, want it measured and no commit", clean, set)
		}
	}
	writeFiles(t, filepath.Join(fixture.tree, "sub"), map[string]string{"dir/installed.js": "untracked\n"})
	if set := measure(made + listing("sub/dir")); !reflect.DeepEqual(set.Gitlinks, []string{"sub"}) {
		t.Fatalf("a listing that saw an untracked entry: %+v, want the submodule's commit", set)
	}
}
