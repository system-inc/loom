package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/protocol"
)

// A tree's source is chunks, assembled from the nearest kept tree when there is one (#shnjnqg). Each mutant below
// must make a test here fail:
//
//	no kept tree ever used, so every chunk is fetched: TestARunnerFetchesOnlyTheChunksItLacks
//	the chunks to fetch read from the blob cache, not the kept tree: TestARunnerFetchesOnlyTheChunksItLacks
//	a spent tree's chunks the new one lacks left in it: TestATreeMadeFromAKeptOneIsTheWholeArchivesTree
//	the directories a removal empties left: TestATreeMadeFromAKeptOneIsTheWholeArchivesTree
//	a kept tree checked by size and modification time alone, or by its files alone, or its top's names unchecked:
//	TestAChangedKeptTreeIsNeverMadeIntoAnother
//	a changed tree's state kept: TestAChangedKeptTreeIsNeverMadeIntoAnother
//	a kept tree a unit holds made into another: TestAHeldKeptTreeIsNeverSpent
//	a chunk's range unchecked as it unpacks: TestAChunkHoldingAnotherChunksNameIsRefused
//	a spent tree renamed before its state is removed, or a tree assembled at its name:
//	TestAnAssemblyKilledPartwayLeavesNothingTrusted
//	a fetched chunk's hash unchecked, or a cached one's: TestACorruptChunkIsNeverAssembled
//	a kept chunk matched by its blob alone, not its range and count: TestAKeptChunkIsMatchedWholeNotByItsBlob

// describeTree describes every entry under directory but the marker: its type and mode, a link's target, a file's
// bytes.
func describeTree(t *testing.T, directory string) map[string]string {
	t.Helper()
	described := map[string]string{}
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, _ := filepath.Rel(directory, path)
		info, err := entry.Info()
		if err != nil || name == "." || name == sourceMarker {
			return err
		}
		description := info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			description += " -> " + target
		case info.Mode().IsRegular():
			content, _ := os.ReadFile(path)
			description += " " + hashOf(content)
		}
		described[filepath.ToSlash(name)] = description
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return described
}

// sameTree fails the test unless the tree at directory is the one files' whole archive unpacks to.
func sameTree(t *testing.T, directory string, files []tarEntry) {
	t.Helper()
	whole := t.TempDir()
	if err := builder.Unpack(bytes.NewReader(makeTar(t, sortedEntries(files), true)), whole, nil); err != nil {
		t.Fatal(err)
	}
	want, got := describeTree(t, whole), describeTree(t, directory)
	for name, description := range want {
		if got[name] != description {
			t.Errorf("%s is %q, and the whole archive's is %q", name, got[name], description)
		}
	}
	for name, description := range got {
		if _, found := want[name]; !found {
			t.Errorf("%s (%s) isn't in the whole archive's tree", name, description)
		}
	}
}

// openFrom opens blobs by sha256 from a map, as the blob cache would, counting each open.
func openFrom(t *testing.T, blobs map[string][]byte, opened map[string]int) func(context.Context, string) (*os.File, error) {
	directory := t.TempDir()
	return func(_ context.Context, sum string) (*os.File, error) {
		opened[sum]++
		path := filepath.Join(directory, sum)
		if _, err := os.Stat(path); err != nil {
			if err = os.WriteFile(path, blobs[sum], 0o444); err != nil {
				return nil, err
			}
		}
		return os.Open(path)
	}
}

// Trees A and B share their first and last chunks; B's middle one drops a directory two deep and makes another.
var (
	treeA = []tarEntry{
		{name: "a/b.txt", kind: tar.TypeReg, content: "b\n"},
		{name: "a/link", kind: tar.TypeSymlink, linkname: "b.txt"},
		{name: "a/run.sh", kind: tar.TypeReg, content: "#!/bin/sh\n", mode: 0o755},
		{name: "m/x/w.txt", kind: tar.TypeReg, content: "w\n"},
		{name: "m/x/y/z.txt", kind: tar.TypeReg, content: "z\n"},
		{name: "top.txt", kind: tar.TypeReg, content: "top\n"},
		{name: "z/end.txt", kind: tar.TypeReg, content: "end\n"},
	}
	treeB = []tarEntry{
		{name: "a/b.txt", kind: tar.TypeReg, content: "b\n"},
		{name: "a/link", kind: tar.TypeSymlink, linkname: "b.txt"},
		{name: "a/run.sh", kind: tar.TypeReg, content: "#!/bin/sh\n", mode: 0o755},
		{name: "m/new/n.txt", kind: tar.TypeReg, content: "n\n"},
		{name: "top.txt", kind: tar.TypeReg, content: "top\n"},
		{name: "z/end.txt", kind: tar.TypeReg, content: "end\n"},
	}
	treeCuts = []string{"m/", "t"}
)

// assembleTree assembles chunks under cache from the nearest kept tree, if any, and releases it; it returns the tree's
// directory and how it was made.
func assembleTree(t *testing.T, cache sourceCache, chunks []builder.SourceChunk, open func(context.Context, string) (*os.File, error)) (string, assembly, []string) {
	t.Helper()
	held, err := cache.hold(builder.SourceSum(chunks))
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()
	base, why := cache.nearest(chunks)
	done, err := cache.assemble(context.Background(), held, chunks, base, open)
	if err != nil || !held.ready || done.stateErr != nil {
		t.Fatalf("assembling: %v %v", err, done.stateErr)
	}
	return held.directory, done, why
}

// A tree one chunk from a kept tree is made from it, unpacking that chunk alone, and is exactly the tree its whole
// archive makes: the kept tree's entries the new one lacks gone, and the directories that left empty, and a new
// directory made. Going back is the same, one chunk again.
func TestATreeMadeFromAKeptOneIsTheWholeArchivesTree(t *testing.T) {
	chunksA, blobs := makeChunks(t, treeA, treeCuts...)
	chunksB, blobsB := makeChunks(t, treeB, treeCuts...)
	for sum, blob := range blobsB {
		blobs[sum] = blob
	}
	opened := map[string]int{}
	open := openFrom(t, blobs, opened)
	cache := newSourceCache(t.TempDir())
	directory, done, _ := assembleTree(t, cache, chunksA, open)
	if done.base != "" || done.unpacked != 3 {
		t.Fatalf("the first tree: %+v", done)
	}
	sameTree(t, directory, treeA)
	directory, done, why := assembleTree(t, cache, chunksB, open)
	if done.base != builder.SourceSum(chunksA) || done.kept != 2 || done.unpacked != 1 || done.removed != 2 || opened[chunksB[1].Blob] != 1 {
		t.Fatalf("B from A: %+v, %q", done, why)
	}
	sameTree(t, directory, treeB)
	if _, err := os.Stat(filepath.Join(cache.directory, builder.SourceSum(chunksA))); err == nil {
		t.Error("the spent tree is still at its name")
	}
	directory, done, _ = assembleTree(t, cache, chunksA, open)
	if done.base != builder.SourceSum(chunksB) || done.unpacked != 1 || opened[chunksA[1].Blob] != 2 || opened[chunksA[0].Blob] != 1 {
		t.Fatalf("A from B: %+v %v", done, opened)
	}
	sameTree(t, directory, treeA)
}

// A kept tree a test changed is never made into another: a file rewritten to its own length with its modification time
// set back, a file made in a directory, one made at the top, a mode changed. The new tree is assembled from nothing,
// and the changed one's state is removed, so it is never checked again.
func TestAChangedKeptTreeIsNeverMadeIntoAnother(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, tree string){
		"a file rewritten, its time set back": func(t *testing.T, tree string) {
			path := filepath.Join(tree, "a", "b.txt")
			info, _ := os.Stat(path)
			os.WriteFile(path, []byte("B\n"), 0o644)
			os.Chtimes(path, info.ModTime(), info.ModTime())
		},
		"a file made in a directory": func(t *testing.T, tree string) {
			os.WriteFile(filepath.Join(tree, "a", "made.txt"), []byte("x"), 0o644)
		},
		"a file made at the top": func(t *testing.T, tree string) {
			os.WriteFile(filepath.Join(tree, "made.txt"), []byte("x"), 0o644)
		},
		"a mode changed": func(t *testing.T, tree string) {
			os.Chmod(filepath.Join(tree, "a", "run.sh"), 0o644)
		},
	} {
		t.Run(name, func(t *testing.T) {
			chunksA, blobs := makeChunks(t, treeA, treeCuts...)
			chunksB, blobsB := makeChunks(t, treeB, treeCuts...)
			for sum, blob := range blobsB {
				blobs[sum] = blob
			}
			open := openFrom(t, blobs, map[string]int{})
			cache := newSourceCache(t.TempDir())
			treeDirectory, _, _ := assembleTree(t, cache, chunksA, open)
			// A unit's test, a while after the tree was made.
			time.Sleep(20 * time.Millisecond)
			change(t, treeDirectory)
			directory, done, why := assembleTree(t, cache, chunksB, open)
			if done.base != "" || done.unpacked != 3 || !strings.Contains(strings.Join(why, "\n"), "isn't made into another") {
				t.Fatalf("B from a changed A: %+v, %q", done, why)
			}
			sameTree(t, directory, treeB)
			if _, err := os.Stat(cache.statePath(builder.SourceSum(chunksA))); err == nil {
				t.Error("the changed tree's state is kept")
			}
			// It still serves its own units, as it is.
			if _, err := os.Stat(filepath.Join(treeDirectory, sourceMarker)); err != nil {
				t.Error("the changed tree was removed")
			}
		})
	}
}

// A kept tree a unit holds is never spent: the new tree is assembled from nothing.
func TestAHeldKeptTreeIsNeverSpent(t *testing.T) {
	chunksA, blobs := makeChunks(t, treeA, treeCuts...)
	chunksB, blobsB := makeChunks(t, treeB, treeCuts...)
	for sum, blob := range blobsB {
		blobs[sum] = blob
	}
	open := openFrom(t, blobs, map[string]int{})
	cache := newSourceCache(t.TempDir())
	assembleTree(t, cache, chunksA, open)
	unit, err := cache.hold(builder.SourceSum(chunksA))
	if err != nil || !unit.ready {
		t.Fatal(err)
	}
	defer unit.release()
	_, done, why := assembleTree(t, cache, chunksB, open)
	if done.base != "" || !strings.Contains(strings.Join(why, "\n"), "held by a unit") {
		t.Fatalf("B beside a held A: %+v %q", done, why)
	}
	if !unit.whole() {
		t.Fatal("the held tree was spent")
	}
}

// A kept chunk is kept only when the new index lists it whole, its blob, range and count: one whose range the new
// index draws narrower around the same blob is unpacked again and refused, as a fresh assembly refuses it, never left
// in place with a file outside every chunk's range. (The review's proof, Oct 10.)
func TestAKeptChunkIsMatchedWholeNotByItsBlob(t *testing.T) {
	chunksA, blobs := makeChunks(t, treeA, treeCuts...)
	open := openFrom(t, blobs, map[string]int{})
	cache := newSourceCache(t.TempDir())
	assembleTree(t, cache, chunksA, open)
	narrowed := append([]builder.SourceChunk{}, chunksA...)
	narrowed[0].Last, narrowed[0].Files = "a/b.txt", 1
	if err := builder.CheckChunks(narrowed); err != nil {
		t.Fatal(err)
	}
	held, _ := cache.hold(builder.SourceSum(narrowed))
	defer held.release()
	base, why := cache.nearest(narrowed)
	if base == nil {
		t.Fatalf("A isn't near the narrowed index: %q", why)
	}
	_, err := cache.assemble(context.Background(), held, narrowed, base, open)
	if err == nil || !strings.Contains(err.Error(), "isn't one this archive may hold") {
		t.Fatalf("a narrowed range around a kept blob: %v", err)
	}
	if _, err = os.Lstat(held.directory); err == nil {
		t.Error("the narrowed tree is at its name")
	}
}

// A chunk holding a name outside its range, another chunk's, is refused, and nothing is left at the tree's name.
func TestAChunkHoldingAnotherChunksNameIsRefused(t *testing.T) {
	chunks, blobs := makeChunks(t, treeA, treeCuts...)
	// The middle chunk claims its range and holds the last chunk's file too.
	dishonest := makeTar(t, sortedEntries([]tarEntry{treeA[3], treeA[4], treeA[6]}), true)
	blobs[hashOf(dishonest)] = dishonest
	chunks[1] = builder.SourceChunk{Blob: hashOf(dishonest), First: chunks[1].First, Last: chunks[1].Last, Files: 3, Bytes: int64(len(dishonest))}
	cache := newSourceCache(t.TempDir())
	held, _ := cache.hold(builder.SourceSum(chunks))
	defer held.release()
	_, err := cache.assemble(context.Background(), held, chunks, nil, openFrom(t, blobs, map[string]int{}))
	if err == nil || !strings.Contains(err.Error(), "isn't one this archive may hold") {
		t.Fatalf("a chunk holding another's name: %v", err)
	}
	if _, err = os.Lstat(held.directory); err == nil {
		t.Error("a refused tree is at its name")
	}
	if entries, _ := filepath.Glob(filepath.Join(cache.directory, unpackingPrefix+"*")); len(entries) != 0 {
		t.Errorf("a refused assembly left %q", entries)
	}
}

// TestHelperAssembleUntilKilled is TestAnAssemblyKilledPartwayLeavesNothingTrusted's runner: it assembles tree B from
// the kept tree A and stops in its first chunk's fetch until it is killed.
func TestHelperAssembleUntilKilled(t *testing.T) {
	root := os.Getenv("LOOM_ASSEMBLE_UNTIL_KILLED")
	if root == "" {
		t.Skip("runs only as TestAnAssemblyKilledPartwayLeavesNothingTrusted's child")
	}
	content, err := os.ReadFile(filepath.Join(root, "chunks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var chunks []builder.SourceChunk
	json.Unmarshal(content, &chunks)
	cache := newSourceCache(root)
	held, err := cache.hold(builder.SourceSum(chunks))
	if err != nil {
		t.Fatal(err)
	}
	base, _ := cache.nearest(chunks)
	if base == nil {
		t.Fatal("no kept tree")
	}
	cache.assemble(context.Background(), held, chunks, base, func(context.Context, string) (*os.File, error) {
		os.WriteFile(filepath.Join(root, "fetching"), nil, 0o644)
		select {}
	})
}

// A runner killed partway through making B from A leaves nothing trusted: nothing at B's name, A spent (it was inside
// B's unpacking), no state naming a tree that isn't there, and an unpacking whose lock is free, which the next sweep
// removes. B is then assembled whole.
func TestAnAssemblyKilledPartwayLeavesNothingTrusted(t *testing.T) {
	chunksA, blobs := makeChunks(t, treeA, treeCuts...)
	chunksB, blobsB := makeChunks(t, treeB, treeCuts...)
	for sum, blob := range blobsB {
		blobs[sum] = blob
	}
	root := t.TempDir()
	cache := newSourceCache(root)
	open := openFrom(t, blobs, map[string]int{})
	assembleTree(t, cache, chunksA, open)
	encoded, _ := json.Marshal(chunksB)
	os.WriteFile(filepath.Join(root, "chunks.json"), encoded, 0o644)
	child := exec.Command(os.Args[0], "-test.run=^TestHelperAssembleUntilKilled$", "-test.count=1")
	child.Env = append(os.Environ(), "LOOM_ASSEMBLE_UNTIL_KILLED="+root)
	var output bytes.Buffer
	child.Stdout, child.Stderr = &output, &output
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Minute)
	for {
		if _, err := os.Stat(filepath.Join(root, "fetching")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			child.Process.Kill()
			child.Wait()
			t.Fatalf("the child never reached its fetch:\n%s", output.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	child.Process.Kill()
	child.Wait()
	sumA, sumB := builder.SourceSum(chunksA), builder.SourceSum(chunksB)
	for _, name := range []string{sumB, sumA} {
		if _, err := os.Lstat(filepath.Join(cache.directory, name)); err == nil {
			t.Errorf("%s is at its name after the kill", name)
		}
	}
	if states, _ := filepath.Glob(filepath.Join(cache.directory, "*"+sourceStateSuffix)); len(states) != 0 {
		t.Errorf("states outlived their trees: %q", states)
	}
	leftovers, _ := filepath.Glob(filepath.Join(cache.directory, unpackingPrefix+sumB+"-*"))
	if len(leftovers) != 1 || !lockFree(leftovers[0]) {
		t.Fatalf("the killed assembly's unpacking: %q", leftovers)
	}
	cache.sweep()
	if _, err := os.Stat(leftovers[0]); err == nil {
		t.Fatal("the sweep left the killed assembly's unpacking")
	}
	directory, done, _ := assembleTree(t, cache, chunksB, open)
	if done.base != "" || done.unpacked != 3 {
		t.Fatalf("B after the kill: %+v", done)
	}
	sameTree(t, directory, treeB)
}

// otherTree is the fixture's tree with internal/uses/uses.go changed, under its own key: one chunk of three differs.
func otherTree(t *testing.T, fixture *prebuiltFixture) *prebuiltTree {
	t.Helper()
	files := fixtureFiles()
	for index := range files {
		if files[index].name == "internal/uses/uses.go" {
			files[index].content += "\n// another tree\n"
		}
	}
	other := *fixture.tree
	other.index.Source = fixture.store.putChunks(t, files, fixtureCuts...)
	other.source = builder.SourceSum(other.index.Source)
	other.index.Tree = strings.Repeat("8", 40)
	other.rekey(t)
	return &other
}

func (fixture *prebuiltFixture) unitOf(tree *prebuiltTree, run string) protocol.Unit {
	unit := fixture.unit(run)
	unit.Test.Tree = tree.key
	return unit
}

// evict empties the fixture runner's blob cache.
func (fixture *prebuiltFixture) evict(t *testing.T) {
	entries, _ := os.ReadDir(fixture.blobs())
	for _, entry := range entries {
		if protocol.Sha256Pattern.MatchString(entry.Name()) {
			if err := os.Remove(filepath.Join(fixture.blobs(), entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A runner fetches only the chunks it lacks: a tree one chunk from the one it keeps fetches that chunk. With its blob
// cache emptied, it still makes a tree from the kept one, whose files the cache's chunks never were, fetching only the
// chunk it lacks. With neither, every chunk.
func TestARunnerFetchesOnlyTheChunksItLacks(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	if result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t)); result.Status != protocol.StatusPassed {
		t.Fatalf("tree A: %s; errors %q", result.Status, errorPhases(events))
	}
	other := otherTree(t, fixture)
	for step, test := range []struct {
		tree   *prebuiltTree
		evict  bool
		chunks int
		from   string
	}{
		{other, false, 1, "from kept tree " + fixture.tree.source},
		{fixture.tree, true, 1, "from kept tree " + other.source},
		{other, true, 1, "from kept tree " + fixture.tree.source},
	} {
		if test.evict {
			fixture.evict(t)
		}
		before := fixture.store.blobGets()
		result, events, _ := runUnit(t, fixture.unitOf(test.tree, "^TestA$"), fixture.options(t))
		runner := strings.Join(outputLines(events, "runner"), "\n")
		// Evicted, the binary and the product come from the store again too.
		want := test.chunks
		if test.evict {
			want += 2
		}
		if result.Status != protocol.StatusPassed || fixture.store.blobGets()-before != want || !strings.Contains(runner, test.from+", 1 chunks to have") {
			t.Fatalf("step %d: %s, %d blob GETs, not %d; errors %q\n%s", step, result.Status, fixture.store.blobGets()-before, want, errorPhases(events), runner)
		}
	}
	// With no tree kept and no blob, every chunk, and the binary, the product and the module cache, trimmed with the
	// trees.
	newSourceCache(filepath.Join(fixture.directory, "root")).trim(0)
	fixture.evict(t)
	before := fixture.store.blobGets()
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if result.Status != protocol.StatusPassed || fixture.store.blobGets()-before != 6 || !strings.Contains(runner, "from no kept tree, 3 chunks to have") {
		t.Fatalf("from nothing: %s, %d blob GETs; errors %q\n%s", result.Status, fixture.store.blobGets()-before, errorPhases(events), runner)
	}
}

// A chunk whose bytes don't hash to its name is never assembled, fetched or cached: the unit is Loom's, and nothing is
// at the tree's name. A cached copy that no longer hashes is fetched again.
func TestACorruptChunkIsNeverAssembled(t *testing.T) {
	fixture := newPrebuiltFixture(t)
	chunk := fixture.tree.index.Source[1].Blob
	honest := fixture.store.objects["blobs/"+chunk]
	fixture.store.put("blobs/"+chunk, makeTar(t, []tarEntry{{name: "internal/lower/testdata/fixture.txt", kind: tar.TypeReg, content: "poisoned\n"}}, true))
	result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "blob "+chunk+" hashes to ") {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	if _, err := os.Lstat(filepath.Join(fixture.directory, "root", sourceDirectoryName, fixture.tree.source)); err == nil {
		t.Fatal("a tree with a corrupt chunk is at its name")
	}
	fixture.store.put("blobs/"+chunk, honest)
	if result, events, _ = runUnit(t, fixture.unit("^TestA$"), fixture.options(t)); result.Status != protocol.StatusPassed {
		t.Fatalf("%s; errors %q", result.Status, errorPhases(events))
	}
	// A cached chunk corrupted on disk, with no tree kept: fetched again, never unpacked.
	newSourceCache(filepath.Join(fixture.directory, "root")).trim(0)
	cached := filepath.Join(fixture.blobs(), chunk)
	os.Chmod(cached, 0o644)
	if err := os.WriteFile(cached, honest[:len(honest)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	result, events, _ = runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
	runner := strings.Join(outputLines(events, "runner"), "\n")
	if result.Status != protocol.StatusPassed || !strings.Contains(runner, "1 of them in place of a cached copy that didn't hash to its name") {
		t.Fatalf("%s; errors %q\n%s", result.Status, errorPhases(events), runner)
	}
}

// An index whose chunks overlap is the store's poison, and one of the whole archive's format is unfit: each is Loom's,
// and nothing is fetched.
func TestOverlappingChunksAndAnOldIndexAreLooms(t *testing.T) {
	for name, test := range map[string]struct {
		change func(t *testing.T, tree *prebuiltTree)
		named  string
	}{
		"overlapping chunks": {func(t *testing.T, tree *prebuiltTree) {
			tree.index.Source[1].First = tree.index.Source[0].Last
			tree.publish(t)
		}, "overlap: the store is poisoned"},
		"the whole archive's index": {func(t *testing.T, tree *prebuiltTree) {
			content, _ := json.Marshal(tree.index)
			old := map[string]any{}
			json.Unmarshal(content, &old)
			delete(old, "format")
			old["source"] = tree.index.Source[0].Blob
			content, _ = json.Marshal(old)
			tree.store.put("trees/"+tree.key+".json", content)
		}, "unfit"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newPrebuiltFixture(t)
			test.change(t, fixture.tree)
			result, events, _ := runUnit(t, fixture.unit("^TestA$"), fixture.options(t))
			errors := errorPhases(events)
			if result.Status != protocol.StatusBroken || !strings.Contains(errors, test.named) || !strings.Contains(errors, "Loom's, never the change's") || fixture.store.blobGets() != 0 {
				t.Fatalf("%s, %d blob GETs; errors %q", result.Status, fixture.store.blobGets(), errors)
			}
		})
	}
}
