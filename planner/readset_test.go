package planner

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
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
