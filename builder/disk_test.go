package builder

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
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

// The floor names the filesystem under it and how much it has, and the temporary directory, which may be memory,
// keeps a smaller floor of its own.
func TestTheFloorNamesTheFilesystemUnderIt(t *testing.T) {
	watched := map[string]Watch{"the cache base": {"/trees", 200 * GB}, "Go's build cache": {"/gocache", 200 * GB}, "the temporary directory": {"/tmp", 20 * GB}}
	free := map[string]uint64{"/trees": 500 * GB, "/gocache": 150 * GB, "/tmp": 30 * GB}
	err := CheckFloor(watched, freeing(free))
	if err == nil || !strings.Contains(err.Error(), "Go's build cache (/gocache) has 150.0 GB free, under its 200 GB floor") {
		t.Fatalf("a short filesystem: %v", err)
	}
	free["/gocache"] = 201 * GB
	if err = CheckFloor(watched, freeing(free)); err != nil {
		t.Fatalf("every filesystem over its floor, the temporary directory at 30 GB over its 20: %v", err)
	}
	free["/tmp"] = 19 * GB
	if err = CheckFloor(watched, freeing(free)); err == nil || !strings.Contains(err.Error(), "the temporary directory (/tmp) has 19.0 GB free, under its 20 GB floor") {
		t.Fatalf("a short temporary directory: %v", err)
	}
}

// Free agrees with df about this machine's temporary directory, to within what a second of other writes moves.
func TestFreeAgreesWithDf(t *testing.T) {
	directory := t.TempDir()
	free, err := Free(directory)
	if err != nil {
		t.Fatal(err)
	}
	output, err := exec.Command("df", "-Pk", directory).Output()
	if err != nil {
		t.Skipf("no df: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	fields := strings.Fields(lines[len(lines)-1])
	kilobytes, err := strconv.ParseUint(fields[3], 10, 64)
	if err != nil {
		t.Fatalf("df said %q", output)
	}
	if difference := math.Abs(float64(free) - float64(kilobytes*1024)); difference > float64(free)/100+1<<20 {
		t.Fatalf("Free says %d bytes, df %d", free, kilobytes*1024)
	}
}

// A floor or a cap in GB that doesn't fit in bytes is refused, never wrapped around to a small number.
func TestGigabytesRefusesWhatDoesntFit(t *testing.T) {
	if bytes, err := Gigabytes(200); err != nil || bytes != 200*GB {
		t.Fatalf("200 GB: %d %v", bytes, err)
	}
	if bytes, err := Gigabytes(1 << 34); err == nil {
		t.Fatalf("2^34 GB came out as %d bytes", bytes)
	}
}

// While the disk is under the floor no job starts: running ones finish, the next waits, and once none is running
// and it is still short, every job left is refused with the reason, and none of them runs.
func TestNoJobStartsWhileTheDiskIsUnderTheFloor(t *testing.T) {
	admissionPoll = time.Millisecond
	t.Cleanup(func() { admissionPoll = time.Second })
	short := errors.New("Go's build cache has 12.0 GB free, under its 200 GB floor")
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
	build := TreeBuild{Out: t.TempDir(), Jobs: 2, Watched: map[string]Watch{"Go's build cache": {"/gocache", 200 * GB}}, Free: freeing(map[string]uint64{"/gocache": 12 * GB})}
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

// goCache writes a cache of ten 100-byte entries used an hour apart from Oct 1, and one used just now, with go's own
// README and trim.txt beside them, and returns it and the old entries, oldest first.
func goCache(t *testing.T) (string, []string, string) {
	t.Helper()
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
	recent := filepath.Join(cache, "ab", fmt.Sprintf("%064x-d", 99))
	os.WriteFile(recent, make([]byte, 100), 0o644)
	return cache, entries, recent
}

// Go's build cache over its cap loses its least recently used entries until it is at three quarters of the cap,
// and nothing but cache entries, and nothing used in the last day, which a running go may be about to read.
func TestGoCacheTrimsTheLeastRecentlyUsedFirst(t *testing.T) {
	cache, entries, recent := goCache(t)
	if removed, err := TrimGoCache(cache, 1100); err != nil || removed != 0 {
		t.Fatalf("a cache at its cap: %d %v", removed, err)
	}
	removed, err := TrimGoCache(cache, 500)
	if err != nil || removed != 800 {
		t.Fatalf("a cache of 1100 over a cap of 500: removed %d, %v", removed, err)
	}
	for index, entry := range entries {
		if _, err := os.Stat(entry); (err == nil) != (index >= 8) {
			t.Errorf("entry %d (used hour %d) kept %v", index, index, err == nil)
		}
	}
	for _, kept := range []string{"README", "trim.txt", recent} {
		if _, err := os.Stat(filepath.Join(cache, strings.TrimPrefix(kept, cache))); err != nil {
			t.Errorf("%s was removed", kept)
		}
	}
	// Over its cap with only recent entries left above the target, it stops at them.
	if removed, err = TrimGoCache(cache, 100); err != nil || removed != 200 {
		t.Fatalf("a cap under what was used today: removed %d, %v", removed, err)
	}
	if _, err = os.Stat(recent); err != nil {
		t.Fatal("an entry used just now was removed")
	}
}

// GOCACHE is often a link, and the trim follows it; GOCACHE=off has nothing to trim; and a trim that finds another
// one's lock leaves the cache alone.
func TestGoCacheTrimFollowsALinkAndNeverRunsTwice(t *testing.T) {
	cache, entries, _ := goCache(t)
	link := filepath.Join(t.TempDir(), "go-build")
	os.Symlink(cache, link)
	lock, _ := os.OpenFile(filepath.Join(cache, "loom-trim.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if removed, err := TrimGoCache(link, 500); err != nil || removed != 0 {
		t.Fatalf("a trim while another holds the lock: removed %d, %v", removed, err)
	}
	lock.Close()
	if removed, err := TrimGoCache(link, 500); err != nil || removed != 800 {
		t.Fatalf("a trim through a link: removed %d, %v", removed, err)
	}
	if _, err := os.Stat(entries[0]); !os.IsNotExist(err) {
		t.Fatal("the oldest entry behind the link is still there")
	}
	if removed, err := TrimGoCache("off", 1); err != nil || removed != 0 {
		t.Fatalf("GOCACHE=off: %d %v", removed, err)
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
