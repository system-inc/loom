package builder

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// A tree's source is chunks (#shnjnqg). Each mutant below must make a test here fail:
//
//	the files left in the order they were listed: TestASourcesChunksHangOnItsFilesAlone
//	a chunk cut where its weight so far reaches the target, not by each file's own hash:
//	TestAOneFileChangeSendsOneChunk
//	a chunk the store holds fresh sent again: TestAOneFileChangeSendsOneChunk
//	a chunk unpacked with every name allowed: TestAChunkHoldsOnlyItsRangeAndItsCount
//	a chunk's count not checked: TestAChunkHoldsOnlyItsRangeAndItsCount
//	chunks whose ranges meet let through: TestAnIndexOfAnotherFormatIsUnfitAndOverlappingChunksPoisoned
//	the index's format not checked: TestAnIndexOfAnotherFormatIsUnfitAndOverlappingChunksPoisoned
//	an entry's depth or length unbounded: TestUnpackRefusesANameTooLongOrTooDeepBeforeOpeningItsParents
//	one blob listed twice let through: TestAnIndexOfAnotherFormatIsUnfitAndOverlappingChunksPoisoned

// withChunkTarget sets chunkTarget for one test.
func withChunkTarget(t *testing.T, target int64) {
	original := chunkTarget
	chunkTarget = target
	t.Cleanup(func() { chunkTarget = original })
}

// wholeArchive is the tree's source as one archive, as SourceArchive made it before the source was chunks: what every
// assembled tree must equal.
func wholeArchive(t *testing.T, tree string) []byte {
	t.Helper()
	command := exec.Command("git", "ls-files", "--recurse-submodules", "-z")
	command.Dir = tree
	listing, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	entries := []archiveEntry{}
	for _, name := range strings.Split(strings.TrimRight(string(listing), "\x00"), "\x00") {
		full := filepath.Join(tree, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(full)
			entries = append(entries, archiveEntry{Name: name, Link: target})
		case info.Mode().IsRegular():
			entries = append(entries, archiveEntry{Name: name, File: full, Executable: info.Mode()&0o111 != 0})
		}
	}
	archive, err := writeArchive(entries)
	if err != nil {
		t.Fatal(err)
	}
	return archive
}

// treeOf describes every entry under directory: its type and mode, a link's target, a file's bytes.
func treeOf(t *testing.T, directory string) map[string]string {
	t.Helper()
	described := map[string]string{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(directory, path)
		info, err := entry.Info()
		if err != nil || name == "." {
			return err
		}
		description := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			description += " -> " + target
		case info.Mode().IsRegular():
			content, _ := os.ReadFile(path)
			description += " " + digest(content)
		}
		described[filepath.ToSlash(name)] = description
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return described
}

// manyFiles is a tree of count files across nested directories, of sizes from a few bytes to a few KiB, with every
// sort hazard in it: a sibling whose name sorts between a directory's entries (d/b-c between d/b/x and d/b/z), an
// executable, a link, and a file at the top.
func manyFiles(count int) map[string]string {
	files := map[string]string{"top.txt": "top\n", "d/b/x": "x\n", "d/b-c": "b-c\n", "d/b/z": "z\n", "run.sh": "#!/bin/sh\necho run\n"}
	for index := range count {
		name := fmt.Sprintf("p%d/q%d/file%03d.txt", index%7, index%3, index)
		files[name] = strings.Repeat(fmt.Sprintf("line %d of %s\n", index, name), 1+(index*37)%120)
	}
	return files
}

// writeFiles writes files under directory in the order given, each at its own time.
func writeFiles(t *testing.T, directory string, files map[string]string, order []string, when time.Time) {
	t.Helper()
	for index, name := range order {
		path := filepath.Join(directory, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(path), 0o755)
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(path, []byte(files[name]), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(path, mode)
		at := when.Add(time.Duration(index) * time.Second)
		os.Chtimes(path, at, at)
	}
}

// The chunks hang on the files alone: the same files, written in another order at other times and listed in any
// order, make the same chunks, each the same bytes.
func TestASourcesChunksHangOnItsFilesAlone(t *testing.T) {
	withChunkTarget(t, 4<<10)
	files := manyFiles(300)
	names := []string{}
	for name := range files {
		names = append(names, name)
	}
	slices.Sort(names)
	first, second := t.TempDir(), t.TempDir()
	writeFiles(t, first, files, names, time.Unix(1_000_000, 0))
	reversed := slices.Clone(names)
	slices.Reverse(reversed)
	writeFiles(t, second, files, reversed, time.Unix(2_000_000, 0))
	want, err := ChunkFiles(first, names)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.Chunks) < 20 {
		t.Fatalf("%d chunks: the test needs many", len(want.Chunks))
	}
	if err = CheckChunks(want.Chunks); err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewSource(7))
	for range 3 {
		shuffled := slices.Clone(names)
		random.Shuffle(len(shuffled), func(left, right int) { shuffled[left], shuffled[right] = shuffled[right], shuffled[left] })
		got, err := ChunkFiles(second, shuffled)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Chunks, want.Chunks) {
			t.Fatalf("the same files listed in another order made other chunks: %d against %d", len(got.Chunks), len(want.Chunks))
		}
		for sum, blob := range want.Blobs {
			if !bytes.Equal(got.Blobs[sum], blob) {
				t.Fatalf("chunk %s's bytes differ", sum)
			}
		}
	}
}

// A one-file change sends one chunk, the one holding the file, when the file keeps its size. One that changes its size
// can move the cut after it (a chance of the change over chunkTarget: this one, 38 bytes in 4 KiB, does), and sends at
// most that chunk and the next, and so does a file made beside it (one grown past chunkTarget makes three:
// TestEveryOneFileChangeTouchesAtMostThreeChunks). Nothing else moves, though the file is near the
// front of the tree.
func TestAOneFileChangeSendsOneChunk(t *testing.T) {
	withChunkTarget(t, 4<<10)
	files := manyFiles(300)
	tree := gitTree(t, files)
	source, err := SourceChunks(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	fake, store := serve(t)
	index := TreeIndex{Tree: strings.Repeat("1", 40), Go: "go1.27.1", Packages: map[string]TreePackage{}}
	if _, _, err = PublishTree(store, &index, t.TempDir(), t.TempDir(), &source, nil); err != nil {
		t.Fatal(err)
	}
	if source.Sent != len(source.Chunks) || len(source.Chunks) < 20 {
		t.Fatalf("the first tree sent %d of %d chunks", source.Sent, len(source.Chunks))
	}
	changed := "p0/q0/file021.txt"
	holder := slices.IndexFunc(source.Chunks, func(chunk SourceChunk) bool { return chunk.Holds(changed) })
	for step, change := range []struct {
		name, content string
	}{
		{changed, strings.ReplaceAll(files[changed], "line", "LINE")},
		{changed, files[changed] + "one more line, and the file is longer\n"},
		// A file made beside it, which a cut by the weight so far would carry into every chunk after it.
		{"p0/q0/file021b.txt", files[changed]},
	} {
		os.WriteFile(filepath.Join(tree, filepath.FromSlash(change.name)), []byte(change.content), 0o644)
		gitCommit(t, tree)
		again, err := SourceChunks(tree, nil)
		if err != nil {
			t.Fatal(err)
		}
		fake.ResetRequests()
		index = TreeIndex{Tree: strings.Repeat(fmt.Sprint(step+2), 40), Go: "go1.27.1", Packages: map[string]TreePackage{}}
		if _, _, err = PublishTree(store, &index, t.TempDir(), t.TempDir(), &again, nil); err != nil {
			t.Fatal(err)
		}
		// The manifest's HEAD, commit and files change with every commit, each a chunk of its own
		// (TestTheManifestsChunksMoveOnlyWithTheirRepository).
		manifest := 0
		for _, chunk := range again.Chunks {
			if strings.HasPrefix(chunk.First, TrackedDirectory+"/") && !slices.Contains(source.Chunks, chunk) {
				manifest++
			}
		}
		if most := min(step+1, 2); manifest != 3 || again.Sent-manifest < 1 || again.Sent-manifest > most || fake.Count("PUT", "blobs/") != again.Sent {
			t.Fatalf("change %d sent %d chunks (%d blob PUTs, %d the manifest's) of %d, not 1 to %d and the manifest's 3", step, again.Sent, fake.Count("PUT", "blobs/"), manifest, len(again.Chunks), most)
		}
		for _, chunk := range again.Chunks {
			if strings.HasPrefix(chunk.First, TrackedDirectory+"/") {
				continue
			}
			if !slices.Contains(source.Chunks, chunk) && (chunk.First < source.Chunks[holder].First || chunk.Last > source.Chunks[holder+1].Last) {
				t.Errorf("change %d: chunk %q to %q is new, outside the changed file's chunk and the next", step, chunk.First, chunk.Last)
			}
		}
	}
}

// The chunks, each unpacked into one directory, make exactly the tree the whole archive made: every name, type,
// mode, link and byte, and no other directory.
func TestTheChunksUnpackToTheWholeArchivesTree(t *testing.T) {
	withChunkTarget(t, 2<<10)
	files := manyFiles(120)
	tree := gitTree(t, files)
	os.Chmod(filepath.Join(tree, "run.sh"), 0o755)
	os.Symlink("../top.txt", filepath.Join(tree, "d", "up"))
	os.Symlink("x", filepath.Join(tree, "d", "b", "y"))
	gitCommit(t, tree)
	source, err := SourceChunks(tree, nil)
	if err != nil {
		t.Fatal(err)
	}
	whole, assembled := t.TempDir(), t.TempDir()
	if err = Unpack(bytes.NewReader(wholeArchive(t, tree)), whole, nil); err != nil {
		t.Fatal(err)
	}
	for _, chunk := range source.Chunks {
		if err = UnpackChunk(bytes.NewReader(source.Blobs[chunk.Blob]), assembled, chunk); err != nil {
			t.Fatal(err)
		}
	}
	want, got := treeOf(t, whole), treeOf(t, assembled)
	// The manifest is the source's beside the tracked files (TestTheManifestIsGitsOwnOutput reads it).
	for _, name := range []string{TrackedDirectory, TrackedDirectory + "/HEAD", TrackedDirectory + "/commit", TrackedDirectory + "/files"} {
		if _, ok := got[name]; !ok {
			t.Fatalf("the chunks' tree has no %s", name)
		}
		delete(got, name)
	}
	if len(want) < 120 || !maps(want, got) {
		t.Fatalf("the chunks' tree differs from the whole archive's: %d entries against %d", len(got), len(want))
	}
	// A link that would leave fails the build, as the whole archive's did.
	os.Symlink("../../outside", filepath.Join(tree, "d", "out"))
	gitCommit(t, tree)
	if _, err = SourceChunks(tree, nil); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a link out of the tree: %v", err)
	}
}

func maps(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for name, description := range left {
		if right[name] != description {
			return false
		}
	}
	return true
}

// A chunk unpacks only names in its range, and exactly as many as its index says: a chunk holding another chunk's
// name would write where that one did.
func TestAChunkHoldsOnlyItsRangeAndItsCount(t *testing.T) {
	blob := tarGzip(t, entry{name: "b/one", body: "1"}, entry{name: "c/two", body: "2"})
	chunk := SourceChunk{Blob: digest(blob), First: "b", Last: "c/two", Files: 2, Bytes: int64(len(blob))}
	if err := UnpackChunk(bytes.NewReader(blob), t.TempDir(), chunk); err != nil {
		t.Fatalf("an honest chunk: %v", err)
	}
	for name, dishonest := range map[string]SourceChunk{
		"a name before its range":  {Blob: chunk.Blob, First: "b/p", Last: "c/two", Files: 2},
		"a name after its range":   {Blob: chunk.Blob, First: "b", Last: "c/one", Files: 2},
		"more names than it says":  {Blob: chunk.Blob, First: "b", Last: "c/two", Files: 1},
		"fewer names than it says": {Blob: chunk.Blob, First: "b", Last: "c/two", Files: 3},
	} {
		if err := UnpackChunk(bytes.NewReader(blob), t.TempDir(), dishonest); err == nil {
			t.Errorf("%s: unpacked", name)
		}
	}
}

// An index of another format is unfit, never read as this one; chunks out of order, empty, or whose ranges meet are
// the store's poison.
func TestAnIndexOfAnotherFormatIsUnfitAndOverlappingChunksPoisoned(t *testing.T) {
	sum := strings.Repeat("a", 64)
	for name, content := range map[string]string{
		"the whole archive's": `{"tree":"t","source":"` + sum + `","packages":{}}`,
		"another format's":    `{"format":1,"tree":"t","source":[{"blob":"` + sum + `","first":"a","last":"b","files":1,"bytes":1}],"packages":{}}`,
		// Format 2's runners ran npm themselves, so its source holds no npm packages.
		"format 2's":       `{"format":2,"tree":"t","source":[{"blob":"` + sum + `","first":"a","last":"b","files":1,"bytes":1}],"packages":{}}`,
		"a later format's": `{"format":4,"tree":"t","source":[],"packages":{}}`,
	} {
		if _, err := ParseTree("k", []byte(content)); !errors.Is(err, ErrIndexFormat) || strings.Contains(err.Error(), "poisoned") {
			t.Errorf("%s index: %v", name, err)
		}
	}
	chunk := func(first, last string, files int) SourceChunk {
		return SourceChunk{Blob: keyOf(first + last), First: first, Last: last, Files: files, Bytes: 1}
	}
	for name, chunks := range map[string][]SourceChunk{
		"overlapping":           {chunk("a", "c", 1), chunk("b", "d", 1)},
		"meeting at a name":     {chunk("a", "b", 1), chunk("b", "d", 1)},
		"out of order":          {chunk("c", "d", 1), chunk("a", "b", 1)},
		"empty":                 {chunk("a", "b", 0)},
		"a range backwards":     {chunk("b", "a", 1)},
		"none":                  {},
		"a blob not a sha256":   {{Blob: "x", First: "a", Last: "a", Files: 1}},
		"one blob listed twice": {{Blob: keyOf("x"), First: "a", Last: "a", Files: 1, Bytes: 1}, {Blob: keyOf("x"), First: "b", Last: "b", Files: 1, Bytes: 1}},
	} {
		index := TreeIndex{Format: TreeIndexFormat, Tree: "t", Source: chunks, Packages: map[string]TreePackage{}}
		encoded, _ := index.encode()
		if _, err := ParseTree("k", encoded); err == nil || !strings.Contains(err.Error(), "poisoned") {
			t.Errorf("%s chunks: %v", name, err)
		}
	}
	index := TreeIndex{Format: TreeIndexFormat, Tree: "t", Source: []SourceChunk{chunk("a", "b", 1), chunk("b/c", "d", 2)}, Packages: map[string]TreePackage{}}
	encoded, _ := index.encode()
	if _, err := ParseTree("k", encoded); err != nil {
		t.Errorf("an honest index: %v", err)
	}
}

// An entry deeper than maxEntryDepth or longer than maxEntryName is refused before any of its parents is made or
// opened: the open directories grow with its depth.
func TestUnpackRefusesANameTooLongOrTooDeepBeforeOpeningItsParents(t *testing.T) {
	for name, entryName := range map[string]string{
		"2,000 levels deep": strings.Repeat("a/", 2000) + "f",
		"257 levels deep":   strings.Repeat("a/", 256) + "f",
		"4,097 bytes long":  "a/" + strings.Repeat("b", 4095),
	} {
		directory := t.TempDir()
		err := Unpack(bytes.NewReader(tarGzip(t, entry{name: entryName, body: "x"})), directory, nil)
		if err == nil || !strings.Contains(err.Error(), "levels deep") {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Lstat(filepath.Join(directory, "a")); err == nil {
			t.Errorf("%s: a parent was made", name)
		}
	}
	directory := t.TempDir()
	if err := Unpack(bytes.NewReader(tarGzip(t, entry{name: strings.Repeat("a/", 255) + "f", body: "x"})), directory, nil); err != nil {
		t.Errorf("256 levels deep: %v", err)
	}
}

// Every one-file change, to each of 400 files, touches at most three chunks: one when the file keeps its size, two
// when its size moves the cut after it, three when it grows past chunkTarget, a chunk of its own cutting its old one in
// two. (The review's proof, Oct 10.)
func TestEveryOneFileChangeTouchesAtMostThreeChunks(t *testing.T) {
	withChunkTarget(t, 8192)
	directory, names := t.TempDir(), []string{}
	for index := range 400 {
		name := fmt.Sprintf("d%02d/f%03d.txt", index%17, index)
		os.MkdirAll(filepath.Join(directory, fmt.Sprintf("d%02d", index%17)), 0o755)
		os.WriteFile(filepath.Join(directory, name), []byte(strings.Repeat("x", 100+index*7%900)), 0o644)
		names = append(names, name)
	}
	base, err := ChunkFiles(directory, names)
	if err != nil {
		t.Fatal(err)
	}
	had := map[string]bool{}
	for _, chunk := range base.Chunks {
		had[chunk.Blob] = true
	}
	most := map[string]int{"the same size": 1, "three times the size": 2, "past chunkTarget": 3}
	worst := map[string]int{}
	for _, name := range names {
		path := filepath.Join(directory, name)
		original, _ := os.ReadFile(path)
		for change, content := range map[string]string{"the same size": strings.Repeat("y", len(original)),
			"three times the size": strings.Repeat("x", len(original)*3), "past chunkTarget": strings.Repeat("x", 9000)} {
			os.WriteFile(path, []byte(content), 0o644)
			changed, err := ChunkFiles(directory, names)
			if err != nil {
				t.Fatal(err)
			}
			made := 0
			for _, chunk := range changed.Chunks {
				if !had[chunk.Blob] {
					made++
				}
			}
			worst[change] = max(worst[change], made)
		}
		os.WriteFile(path, original, 0o644)
	}
	for change, limit := range most {
		if worst[change] < 1 || worst[change] > limit {
			t.Errorf("a change to %s made up to %d new chunks, not 1 to %d", change, worst[change], limit)
		}
	}
	t.Logf("%d chunks; the most new chunks a one-file change made: %v", len(base.Chunks), worst)
}
