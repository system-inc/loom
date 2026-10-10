package builder

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitTree(t *testing.T, files map[string]string) string {
	t.Helper()
	tree := t.TempDir()
	for name, content := range files {
		os.MkdirAll(filepath.Dir(filepath.Join(tree, name)), 0o755)
		os.WriteFile(filepath.Join(tree, name), []byte(content), 0o644)
	}
	for _, arguments := range [][]string{{"init", "-q"}, {"add", "."}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "tree"}} {
		if output, err := exec.Command("git", append([]string{"-C", tree}, arguments...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", arguments, err, output)
		}
	}
	return tree
}

func TestEachTreeBuildsInItsOwnCacheAndOnlyTheNewestAreKept(t *testing.T) {
	base := t.TempDir()
	before := gitTree(t, map[string]string{"go.mod": "module m\n", "include/x.h": "#define X 1\n"})
	after := gitTree(t, map[string]string{"go.mod": "module m\n", "include/x.h": "#define X 2\n"})
	same := gitTree(t, map[string]string{"go.mod": "module m\n", "include/x.h": "#define X 1\n"})

	first, err := TreeCache(base, before, 2)
	if err != nil {
		t.Fatal(err)
	}
	// A product built for the first tree, keyed without the header it read.
	os.MkdirAll(filepath.Join(first, strings.Repeat("a", 64)), 0o755)
	second, err := TreeCache(base, after, 2)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("two trees with a different header share a cache, so a product keyed without the header is served stale")
	}
	if _, err = os.Stat(filepath.Join(second, strings.Repeat("a", 64))); err == nil {
		t.Fatal("the second tree's cache holds the first tree's product")
	}
	if again, _ := TreeCache(base, same, 2); again != first {
		t.Fatalf("two commits with the same content got different caches: %s, %s", again, first)
	}

	// A third tree: the oldest cache goes, a directory that isn't a tree's is left alone.
	os.MkdirAll(filepath.Join(base, "not-a-tree-cache"), 0o755)
	time.Sleep(10 * time.Millisecond)
	third := gitTree(t, map[string]string{"go.mod": "module m\n", "include/x.h": "#define X 3\n"})
	if _, err = TreeCache(base, after, 2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	newest, err := TreeCache(base, third, 2)
	if err != nil {
		t.Fatal(err)
	}
	for path, kept := range map[string]bool{newest: true, second: true, first: false, filepath.Join(base, "not-a-tree-cache"): true} {
		if _, err := os.Stat(path); (err == nil) != kept {
			t.Errorf("%s kept %v, want %v", filepath.Base(path), err == nil, kept)
		}
	}
}

func TestATreeWithUncommittedChangesHasNoCache(t *testing.T) {
	tree := gitTree(t, map[string]string{"go.mod": "module m\n"})
	os.WriteFile(filepath.Join(tree, "go.mod"), []byte("module changed\n"), 0o644)
	if _, err := TreeCache(t.TempDir(), tree, 2); err == nil || !strings.Contains(err.Error(), "changes to tracked files") {
		t.Fatalf("a dirty tree: %v", err)
	}
}
