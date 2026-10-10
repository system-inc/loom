package resident

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// git runs git in directory with a fixed identity, local file transport allowed for the fixture's submodule.
func git(t *testing.T, directory string, arguments ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-c", "protocol.file.allow=always", "-c", "init.defaultBranch=main", "-c", "commit.gpgsign=false"}, arguments...)...)
	command.Dir = directory
	command.Env = append(os.Environ(), "GIT_AUTHOR_NAME=loom", "GIT_AUTHOR_EMAIL=loom@example.com", "GIT_COMMITTER_NAME=loom",
		"GIT_COMMITTER_EMAIL=loom@example.com", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(arguments, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, text := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fixture is a module in a git repository with a submodule, lib, whose files are packages of the module: a imports b
// and lib, and a's test embeds a file under its testdata; c stands alone; e's test imports a. It returns the
// repository, the submodule's own repository (its upstream), and the first commit.
func fixture(t *testing.T) (string, string, string) {
	t.Helper()
	upstream := filepath.Join(t.TempDir(), "lib")
	write(t, upstream, map[string]string{"l.go": "package lib\n\nconst Lib = 1\n"})
	git(t, upstream, "init", "-q")
	git(t, upstream, "add", "-A")
	git(t, upstream, "commit", "-q", "-m", "lib")
	root := filepath.Join(t.TempDir(), "tree")
	write(t, root, map[string]string{
		"go.mod":               "module example.com/resident\n\ngo 1.22\n",
		"go.sum":               "",
		"go.work":              "go 1.22\n\nuse .\n",
		"a/a.go":               "package a\n\nimport (\n\t\"example.com/resident/b\"\n\t\"example.com/resident/lib\"\n)\n\nvar Answer = b.Answer + lib.Lib\n",
		"a/a_test.go":          "package a\n\nimport (\n\t_ \"embed\"\n\t\"testing\"\n)\n\n//go:embed testdata/data.txt\nvar data string\n\nfunc TestA(test *testing.T) {}\n",
		"a/testdata/data.txt":  "one\n",
		"a/testdata/other.txt": "not embedded\n",
		"b/b.go":               "package b\n\nconst Answer = 1\n",
		"c/c.go":               "package c\n\nconst Unrelated = 1\n",
		"c/c_test.go":          "package c\n\nimport \"testing\"\n\nfunc TestC(test *testing.T) {}\n",
		"e/e.go":               "package e\n",
		"e/e_test.go":          "package e_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/resident/a\"\n)\n\nfunc TestE(test *testing.T) { _ = a.Answer }\n",
	})
	git(t, root, "init", "-q")
	git(t, root, "submodule", "add", "-q", upstream, "lib")
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", "first")
	return root, upstream, git(t, root, "rev-parse", "HEAD")
}

// commit commits every change in root and returns the commit.
func commit(t *testing.T, root, message string) string {
	t.Helper()
	git(t, root, "add", "-A")
	git(t, root, "commit", "-q", "-m", message)
	return git(t, root, "rev-parse", "HEAD")
}

// checked keys the commit checked out in root with the resident and cold, and fails on any difference.
func checked(t *testing.T, resident *Resident, root, commit string) *Tree {
	t.Helper()
	tree, err := resident.Key(root, commit)
	if err != nil {
		t.Fatal(err)
	}
	cold, err := Cold(root)
	if err != nil {
		t.Fatal(err)
	}
	differences, files := Check(root, resident, tree, cold)
	if files == 0 {
		t.Fatal("Check read no closure file")
	}
	if len(differences) > 0 {
		t.Fatalf("commit %s: the resident and cold differ:\n%s", commit, strings.Join(differences, "\n"))
	}
	if len(cold.Closures) == 0 {
		t.Fatal("cold keyed no closure, so the comparison compared nothing")
	}
	return tree
}

// The resident's keys equal a cold read's on every tree of a walk where each step reaches closures a different way,
// and each step lists again only the packages its diff reaches.
func TestTheResidentKeysEveryTreeAsColdDoes(t *testing.T) {
	root, upstream, first := fixture(t)
	resident := New()
	tree := checked(t, resident, root, first)
	if tree.From != "" || len(tree.Relisted) != 3 {
		t.Fatalf("the first tree was read whole: from %q, relisted %v", tree.From, tree.Relisted)
	}
	steps := []struct {
		name     string
		edit     func()
		relisted []string
	}{
		{"an import's file", func() { write(t, root, map[string]string{"b/b.go": "package b\n\nconst Answer = 2\n"}) },
			[]string{"example.com/resident/a", "example.com/resident/e"}},
		{"a file a test embeds, under its testdata", func() { write(t, root, map[string]string{"a/testdata/data.txt": "two\n"}) },
			[]string{"example.com/resident/a", "example.com/resident/e"}},
		{"a file beside it no test embeds", func() { write(t, root, map[string]string{"a/testdata/other.txt": "still not\n"}) },
			[]string{"example.com/resident/a", "example.com/resident/e"}},
		{"a file outside every closure", func() { write(t, root, map[string]string{"README": "hello\n"}) }, []string{}},
		{"a new test package", func() {
			write(t, root, map[string]string{"d/d.go": "package d\n", "d/d_test.go": "package d\n\nimport \"testing\"\n\nfunc TestD(test *testing.T) {}\n"})
		}, []string{"example.com/resident/d"}},
		{"a package removed", func() {
			for _, name := range []string{"c/c.go", "c/c_test.go"} {
				if err := os.Remove(filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			}
		}, []string{}},
		{"the submodule moved", func() {
			write(t, upstream, map[string]string{"l.go": "package lib\n\nconst Lib = 2\n"})
			git(t, upstream, "commit", "-q", "-am", "lib two")
			git(t, filepath.Join(root, "lib"), "pull", "-q", "origin", "HEAD")
		}, []string{"example.com/resident/a", "example.com/resident/e"}},
		{"a test's import go can't find", func() {
			write(t, root, map[string]string{"e/e_test.go": "package e_test\n\nimport (\n\t\"testing\"\n\n\t_ \"example.com/resident/missing\"\n)\n\nfunc TestE(test *testing.T) {}\n"})
		}, []string{"example.com/resident/e"}},
		// The tree before the import was lost, exactly: a warm tree zero paths away, so nothing is listed again.
		{"found again", func() {
			write(t, root, map[string]string{"e/e_test.go": "package e_test\n\nimport (\n\t\"testing\"\n\n\t\"example.com/resident/a\"\n)\n\nfunc TestE(test *testing.T) { _ = a.Answer }\n"})
		}, []string{}},
		{"the module's checksums", func() { write(t, root, map[string]string{"go.sum": "\n"}) },
			[]string{"example.com/resident/a", "example.com/resident/d", "example.com/resident/e"}},
	}
	for _, step := range steps {
		step.edit()
		next := commit(t, root, step.name)
		tree = checked(t, resident, root, next)
		if broken := tree.Closures["example.com/resident/e"].Error != ""; broken != strings.Contains(step.name, "can't find") {
			t.Fatalf("%s: e's closure refused %v", step.name, broken)
		}
		if tree.From == "" {
			t.Fatalf("%s: read whole, not against a warm tree", step.name)
		}
		relisted := slices.Clone(tree.Relisted)
		slices.Sort(relisted)
		if !slices.Equal(relisted, step.relisted) {
			t.Errorf("%s: listed %v again, want %v", step.name, relisted, step.relisted)
		}
	}
	if found := tree.Closures["example.com/resident/e"]; found.Error != "" || found.Key == "" {
		t.Fatalf("e's closure after its import is found again: %+v", found)
	}
}

// The mutant: a resident that keeps a stale hash for a changed file keys a closure apart from cold, and Check names
// both the key and the file. Without the mutant the same walk checks clean, so the check could fail and looked at the
// real file.
func TestCheckCatchesAStaleHash(t *testing.T) {
	root, _, first := fixture(t)
	for _, mutant := range []bool{false, true} {
		git(t, root, "checkout", "-q", first)
		resident := New()
		if mutant {
			resident.Stale("b/b.go")
		}
		if _, err := resident.Key(root, first); err != nil {
			t.Fatal(err)
		}
		write(t, root, map[string]string{"b/b.go": "package b\n\nconst Answer = 2\n"})
		next := commit(t, root, "b moves")
		tree, err := resident.Key(root, next)
		if err != nil {
			t.Fatal(err)
		}
		cold, err := Cold(root)
		if err != nil {
			t.Fatal(err)
		}
		found, _ := Check(root, resident, tree, cold)
		differences := strings.Join(found, "\n")
		caught := strings.Contains(differences, "package example.com/resident/a: cold keys its closure") && strings.Contains(differences, "file b/b.go:")
		if caught != mutant {
			t.Fatalf("mutant %v: Check said:\n%s", mutant, differences)
		}
	}
}

// The nearest warm tree is the one fewest paths apart, and only Keep trees are held.
func TestTheNearestWarmTreeIsTheOneFewestPathsApart(t *testing.T) {
	root, _, first := fixture(t)
	resident := New()
	resident.Keep = 2
	if _, err := resident.Key(root, first); err != nil {
		t.Fatal(err)
	}
	write(t, root, map[string]string{"b/b.go": "package b\n\nconst Answer = 2\n", "c/c.go": "package c\n\nconst Unrelated = 2\n"})
	far := commit(t, root, "far")
	if _, err := resident.Key(root, far); err != nil {
		t.Fatal(err)
	}
	git(t, root, "checkout", "-q", first)
	write(t, root, map[string]string{"README": "near\n"})
	near := commit(t, root, "near")
	tree, err := resident.Key(root, near)
	if err != nil {
		t.Fatal(err)
	}
	if tree.From != first || len(tree.Changed) != 1 {
		t.Fatalf("near was read against %s with %v changed, want %s and only README", tree.From, tree.Changed, first)
	}
	if len(resident.trees) != 2 || resident.trees[0].Commit != far || resident.trees[1].Commit != near {
		t.Fatalf("held %d trees, want far and near", len(resident.trees))
	}
}
