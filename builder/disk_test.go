package builder

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/system-inc/loom/planner"
)

// freeing is a filesystem reading that gives each path the bytes in free, by path.
func freeing(free map[string]uint64) func(string) (uint64, error) {
	return func(path string) (uint64, error) {
		available, known := free[path]
		if !known {
			return 0, errors.New("no such filesystem")
		}
		return available, nil
	}
}

// The floor names the filesystem under it and how much it has.
func TestTheFloorNamesTheFilesystemUnderIt(t *testing.T) {
	paths := map[string]string{"the cache base": "/trees", "Go's build cache": "/gocache", "the temporary directory": "/tmp"}
	free := map[string]uint64{"/trees": 500 * GB, "/gocache": 150 * GB, "/tmp": 300 * GB}
	err := CheckFloor(paths, 200*GB, freeing(free))
	if err == nil || !strings.Contains(err.Error(), "Go's build cache (/gocache) has 150.0 GB free, under the 200 GB floor") {
		t.Fatalf("a short filesystem: %v", err)
	}
	free["/gocache"] = 201 * GB
	if err = CheckFloor(paths, 200*GB, freeing(free)); err != nil {
		t.Fatalf("every filesystem over the floor: %v", err)
	}
	if free, err := Free(t.TempDir()); err != nil || free == 0 {
		t.Fatalf("this machine's temporary directory: %d %v", free, err)
	}
}

// While the disk is under the floor no job starts: running ones finish, the next waits, and once none is running
// and it is still short, every job left is refused with the reason, and none of them runs.
func TestNoJobStartsWhileTheDiskIsUnderTheFloor(t *testing.T) {
	admissionPoll = time.Millisecond
	t.Cleanup(func() { admissionPoll = time.Second })
	short := errors.New("Go's build cache has 12.0 GB free, under the 200 GB floor")
	ran, refused := atomic.Int64{}, atomic.Int64{}
	admitted(5, 2, nil, 0.8, func() error { return short }, func(int) { ran.Add(1) }, func(index int, err error) {
		if errors.Is(err, short) {
			refused.Add(1)
		}
	})
	if ran.Load() != 0 || refused.Load() != 5 {
		t.Fatalf("a disk under the floor: %d ran, %d refused", ran.Load(), refused.Load())
	}
	// Short once the first job is running (it is what fills the disk) until it finishes: the second job waits for it,
	// and then the rest run.
	var calls, finished atomic.Int64
	var firstEnded, secondStarted atomic.Int64
	disk := func() error {
		if calls.Add(1) > 1 && finished.Load() == 0 {
			return short
		}
		return nil
	}
	ran.Store(0)
	admitted(4, 4, nil, 0.8, disk, func(index int) {
		if index == 1 {
			secondStarted.Store(time.Now().UnixNano())
		}
		time.Sleep(20 * time.Millisecond)
		ran.Add(1)
		if index == 0 {
			firstEnded.Store(time.Now().UnixNano())
			finished.Add(1)
		}
	}, func(index int, err error) { t.Errorf("job %d refused: %v", index, err) })
	if ran.Load() != 4 || secondStarted.Load() < firstEnded.Load() {
		t.Fatalf("a disk short while the first job runs: %d ran, the second started before the first ended", ran.Load())
	}
	// A tree's binaries refused for the disk name it in their error, and no go process runs.
	build := TreeBuild{Out: t.TempDir(), Jobs: 2, Floor: 200 * GB, Watched: map[string]string{"Go's build cache": "/gocache"}, Free: freeing(map[string]uint64{"/gocache": 12 * GB})}
	for _, result := range build.Binaries([]planner.ProductTest{{Package: "example.com/a", Directory: "a"}}) {
		if !strings.Contains(result.Error, "not started: Go's build cache (/gocache) has 12.0 GB free") {
			t.Fatalf("a refused binary: %+v", result)
		}
	}
}

// A tree's directory goes once its index is up, file by file, and nothing outside it: a link inside is removed as
// itself, never followed. It is kept when the index didn't go up, and only a tree's directory under the base goes.
func TestATreesDirectoryIsRemovedOnceItsIndexIsUp(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "kept"), []byte("outside"), 0o644)
	directory := filepath.Join(base, strings.Repeat("a", 40))
	for _, name := range []string{"out/a.test", "cache/" + keyOf("p") + "/tool", "cache/" + keyOf("p") + ".inputs", "logs/product-0.log", "held-1/x/y"} {
		os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0o755)
		os.WriteFile(filepath.Join(directory, name), []byte(name), 0o644)
	}
	os.MkdirAll(filepath.Join(directory, "empty", "deeper"), 0o755)
	os.Symlink(outside, filepath.Join(directory, "cache", "link-out"))
	if err := TreeDone(base, directory, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "out", "a.test")); err != nil {
		t.Fatal("a tree whose index didn't go up was removed")
	}
	if err := TreeDone(base, directory, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("the tree's directory is still there: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(outside, "kept")); err != nil || string(content) != "outside" {
		t.Fatal("a link inside the tree was followed out of it")
	}
	for _, refused := range []string{base, outside, filepath.Join(base, "not-a-tree"), filepath.Join(base, strings.Repeat("b", 40), strings.Repeat("c", 40))} {
		os.MkdirAll(refused, 0o755)
		if err := RemoveTree(base, refused); err == nil {
			t.Errorf("%s was taken for a tree's directory", refused)
		}
	}
}

// Go's build cache over its cap loses its least recently used entries until it is at three quarters of the cap,
// and nothing but cache entries.
func TestGoCacheTrimsTheLeastRecentlyUsedFirst(t *testing.T) {
	cache := t.TempDir()
	os.MkdirAll(filepath.Join(cache, "ab"), 0o755)
	os.WriteFile(filepath.Join(cache, "README"), make([]byte, 5000), 0o644)
	os.WriteFile(filepath.Join(cache, "trim.txt"), []byte("1"), 0o644)
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	entries := []string{}
	for index := range 10 {
		name := filepath.Join(cache, "ab", fmt.Sprintf("%064x-%c", index, "ad"[index%2]))
		os.WriteFile(name, make([]byte, 100), 0o644)
		used := start.Add(time.Duration(index) * time.Hour)
		os.Chtimes(name, used, used)
		entries = append(entries, name)
	}
	if removed, err := TrimGoCache(cache, 1000); err != nil || removed != 0 {
		t.Fatalf("a cache at its cap: %d %v", removed, err)
	}
	removed, err := TrimGoCache(cache, 500)
	if err != nil || removed != 700 {
		t.Fatalf("a cache of 1000 over a cap of 500: removed %d, %v", removed, err)
	}
	for index, entry := range entries {
		if _, err := os.Stat(entry); (err == nil) != (index >= 7) {
			t.Errorf("entry %d (used hour %d) kept %v", index, index, err == nil)
		}
	}
	for _, own := range []string{"README", "trim.txt"} {
		if _, err := os.Stat(filepath.Join(cache, own)); err != nil {
			t.Errorf("go's own %s was removed", own)
		}
	}
}

// A removal stopped partway (here, the third file refuses to go) never leaves a tree directory TreeCache would take
// back with products half gone, which buildcache would count as hits: the tree's directory was renamed away first,
// and the next TreeCache gives the tree a fresh directory and finishes the removal.
func TestAnInterruptedCleanupLeavesNoHollowTree(t *testing.T) {
	base := t.TempDir()
	tree := gitTree(t, map[string]string{"go.mod": "module m\n"})
	directory, err := TreeCache(base, tree, 2)
	if err != nil {
		t.Fatal(err)
	}
	product := keyOf("p")
	for _, name := range []string{"cache/" + product + "/tool", "cache/" + product + "/data", "cache/" + product + ".inputs", "out/a.test"} {
		os.MkdirAll(filepath.Dir(filepath.Join(directory, name)), 0o755)
		os.WriteFile(filepath.Join(directory, name), []byte(name), 0o644)
	}
	removed := 0
	remove = func(path string) error {
		if removed++; removed == 3 {
			return errors.New("killed")
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { remove = os.Remove })
	if err = RemoveTree(base, directory); err == nil {
		t.Fatal("the stopped removal reported no error")
	}
	if _, err = os.Lstat(filepath.Join(directory, "cache", product)); !os.IsNotExist(err) {
		t.Fatalf("a stopped removal left the tree's product directory where TreeCache finds it: %v", err)
	}
	remove = os.Remove
	again, err := TreeCache(base, tree, 2)
	if err != nil || again != directory {
		t.Fatalf("the tree's directory again: %s %v", again, err)
	}
	if entries, _ := os.ReadDir(again); len(entries) != 0 {
		t.Fatalf("the tree's directory came back holding %s", entries[0].Name())
	}
	if entries, _ := os.ReadDir(base); len(entries) != 1 {
		t.Fatalf("the stopped removal wasn't finished: %d entries under the base", len(entries))
	}
}
