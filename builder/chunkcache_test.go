package builder

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A warm tree gzips only the chunks it changed (#s0cqqhk lever 5). Each mutant below must make a test here fail:
//
//	an entry's blob left out of its chunk's key (a stale chunk served for a changed file):
//	TestAWarmSourceIsTheColdSourceByteForByte
//	an entry's executable bit left out of the key: TestAWarmSourceIsTheColdSourceByteForByte
//	a kept chunk never taken: TestAWarmSourceIsTheColdSourceByteForByte
//	a kept chunk's sha256 not checked as it is read: TestAKeptChunkThatIsntWholeIsBuiltAgain
//	the archiver left out of the key: TestAChunkAnotherArchiverKeptIsNeverTaken
//	a chunk holding a file Blobs doesn't name kept anyway: TestBlobsDecidesWhatIsKept
//	Trim removing the most recently used first: TestTrimKeepsTheMostRecentlyUsed

// sameSource fails the test unless warm is cold: the same chunks, and each one's blob the same bytes.
func sameSource(t *testing.T, step string, cold, warm Source) {
	t.Helper()
	if !slices.Equal(cold.Chunks, warm.Chunks) {
		t.Fatalf("%s: the warm source's chunks aren't the cold one's", step)
	}
	for _, chunk := range cold.Chunks {
		if !bytes.Equal(cold.Blobs[chunk.Blob], warm.Blobs[chunk.Blob]) {
			t.Fatalf("%s: chunk %s's blob isn't the cold one's bytes", step, chunk.Blob)
		}
	}
}

// cachedTree is trackedTree with many files beside its own, committed, cut into chunks of 4 KiB.
func cachedTree(t *testing.T) string {
	t.Helper()
	withChunkTarget(t, 4<<10)
	top := trackedTree(t)
	files := manyFiles(200)
	for name, content := range files {
		if name == "top.txt" {
			continue
		}
		os.MkdirAll(filepath.Dir(filepath.Join(top, filepath.FromSlash(name))), 0o755)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		os.WriteFile(filepath.Join(top, filepath.FromSlash(name)), []byte(content), mode)
	}
	gitCommit(t, top)
	return top
}

// A warm source is the cold one, byte for byte, through every kind of change a commit makes to a chunk: a file's
// content at the same size (the same cut, another blob), its executable bit (the same blob), and a file made a link
// beside a new file. Every chunk the change left alone is taken from the cache and
// built by nothing, a clone of the tree elsewhere takes them all, and none is taken that the cold build wouldn't make.
func TestAWarmSourceIsTheColdSourceByteForByte(t *testing.T) {
	top := cachedTree(t)
	cache := &ChunkCache{Directory: t.TempDir()}
	cold, err := SourceChunks(top, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cold.Chunks) < 20 {
		t.Fatalf("%d chunks", len(cold.Chunks))
	}
	first, err := CachedSourceChunks(top, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	sameSource(t, "the first warm build", cold, first)
	if first.Cached != 0 {
		t.Errorf("an empty cache held %d chunks", first.Cached)
	}
	again, err := CachedSourceChunks(top, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	sameSource(t, "the second", cold, again)
	if again.Cached != len(cold.Chunks) {
		t.Errorf("the same tree took %d of its %d chunks from the cache", again.Cached, len(cold.Chunks))
	}
	clone := filepath.Join(t.TempDir(), "clone")
	gitIn(t, t.TempDir(), "clone", "-q", "--recurse-submodules", top, clone)
	elsewhere, err := CachedSourceChunks(clone, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	sameSource(t, "a clone", cold, elsewhere)
	if elsewhere.Cached != len(cold.Chunks) {
		t.Errorf("a clone of the tree took %d of its %d chunks from the cache", elsewhere.Cached, len(cold.Chunks))
	}
	changed := "p0/q0/file021.txt"
	content, _ := os.ReadFile(filepath.Join(top, changed))
	for _, change := range []struct {
		name   string
		change func()
	}{
		{"its content at the same size", func() {
			os.WriteFile(filepath.Join(top, changed), bytes.ReplaceAll(content, []byte("line"), []byte("LINE")), 0o644)
		}},
		{"its executable bit", func() { os.Chmod(filepath.Join(top, changed), 0o755) }},
		{"made a link", func() {
			os.Remove(filepath.Join(top, changed))
			os.WriteFile(filepath.Join(top, "p0/q0/file021-target.txt"), nil, 0o644)
			os.Symlink("file021-target.txt", filepath.Join(top, changed))
		}},
	} {
		previous := map[string]bool{}
		for _, chunk := range cold.Chunks {
			previous[chunk.Blob] = true
		}
		change.change()
		gitCommit(t, top)
		cold, err = SourceChunks(top, nil)
		if err != nil {
			t.Fatal(err)
		}
		warm, err := CachedSourceChunks(top, nil, cache)
		if err != nil {
			t.Fatal(err)
		}
		sameSource(t, change.name, cold, warm)
		unchanged := 0
		for _, chunk := range cold.Chunks {
			if previous[chunk.Blob] {
				unchanged++
			}
		}
		if warm.Cached != unchanged || unchanged < len(cold.Chunks)-6 {
			t.Errorf("%s: %d chunks taken from the cache, and %d of %d were unchanged", change.name, warm.Cached, unchanged, len(cold.Chunks))
		}
	}
}

// A kept chunk that isn't whole, torn or rotted, is built again, never shipped: the source stays the cold one.
func TestAKeptChunkThatIsntWholeIsBuiltAgain(t *testing.T) {
	top := cachedTree(t)
	cache := &ChunkCache{Directory: t.TempDir()}
	cold, err := CachedSourceChunks(top, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := filepath.Glob(filepath.Join(cache.Directory, "*"+chunkCacheSuffix))
	if len(kept) != len(cold.Chunks) {
		t.Fatalf("%d chunks kept of %d", len(kept), len(cold.Chunks))
	}
	rotted, _ := os.ReadFile(kept[0])
	rotted[len(rotted)-9] ^= 0x01
	os.WriteFile(kept[0], rotted, 0o644)
	torn, _ := os.ReadFile(kept[1])
	os.WriteFile(kept[1], torn[:len(torn)/2], 0o644)
	warm, err := CachedSourceChunks(top, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	sameSource(t, "a rotted and a torn chunk kept", cold, warm)
	if warm.Cached != len(cold.Chunks)-2 {
		t.Errorf("%d of %d chunks taken from the cache, with two not whole", warm.Cached, len(cold.Chunks))
	}
}

// A chunk kept by another archiver (another Go release's gzip, or another writeArchive) is never taken, so the bytes
// are always what this binary's cold build makes.
func TestAChunkAnotherArchiverKeptIsNeverTaken(t *testing.T) {
	top := cachedTree(t)
	cache := &ChunkCache{Directory: t.TempDir()}
	original := archiverName
	archiverName = func() string { return "go0.0 another" }
	_, err := CachedSourceChunks(top, nil, cache)
	archiverName = original
	if err != nil {
		t.Fatal(err)
	}
	warm, err := CachedSourceChunks(top, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	if warm.Cached != 0 {
		t.Errorf("%d chunks another archiver kept were taken", warm.Cached)
	}
}

// Blobs names each file's blob, git's by default: one given (the Resident's, from memory) keys the chunks as git's
// does, and a chunk holding a file it doesn't name is built and not kept.
func TestBlobsDecidesWhatIsKept(t *testing.T) {
	top := cachedTree(t)
	_, tracked, _, err := trackedManifest(top)
	if err != nil {
		t.Fatal(err)
	}
	unnamed := "p0/q0/file021.txt"
	cache := &ChunkCache{Directory: t.TempDir(), Blobs: func(path string) (string, bool) {
		if path == unnamed {
			return "", false
		}
		record, recorded := tracked[path]
		return record.object, recorded
	}}
	cold, err := CachedSourceChunks(top, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	warm, err := CachedSourceChunks(top, nil, cache)
	if err != nil {
		t.Fatal(err)
	}
	sameSource(t, "with Blobs given", cold, warm)
	if warm.Cached != len(cold.Chunks)-1 {
		t.Errorf("%d of %d chunks taken from the cache, one holding a file Blobs doesn't name", warm.Cached, len(cold.Chunks))
	}
	byGit, err := CachedSourceChunks(top, nil, &ChunkCache{Directory: cache.Directory})
	if err != nil {
		t.Fatal(err)
	}
	if byGit.Cached != len(cold.Chunks)-1 {
		t.Errorf("git's blobs took %d of the %d chunks Blobs kept", byGit.Cached, len(cold.Chunks)-1)
	}
}

// Trim keeps the most recently used chunks within Bytes, and removes a staging file a write left over an hour ago.
func TestTrimKeepsTheMostRecentlyUsed(t *testing.T) {
	cache := &ChunkCache{Directory: t.TempDir(), Bytes: 250}
	now := time.Now()
	for name, minutes := range map[string]int{"newest": 0, "new": 1, "old": 2, "older": 3} {
		file := filepath.Join(cache.Directory, name+chunkCacheSuffix)
		os.WriteFile(file, bytes.Repeat([]byte("x"), 100), 0o644)
		at := now.Add(-time.Duration(minutes) * time.Minute)
		os.Chtimes(file, at, at)
	}
	staging := filepath.Join(cache.Directory, ".staging-1")
	os.WriteFile(staging, nil, 0o644)
	os.Chtimes(staging, now.Add(-2*time.Hour), now.Add(-2*time.Hour))
	if err := cache.Trim(); err != nil {
		t.Fatal(err)
	}
	left, _ := filepath.Glob(filepath.Join(cache.Directory, "*"))
	for index := range left {
		left[index] = filepath.Base(left[index])
	}
	slices.Sort(left)
	if want := []string{"new" + chunkCacheSuffix, "newest" + chunkCacheSuffix}; !slices.Equal(left, want) {
		t.Errorf("Trim left %v, not %v", left, want)
	}
}
