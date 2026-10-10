package runner

import (
	"context"
	"encoding/gob"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/protocol"
)

// A tree's source is chunks (builder/chunks.go), and a runner assembles the tree from them under its sources
// directory, at the source's sum (builder.SourceSum of its chunk list), with sources.go's every safety: assembled in a
// locked .unpacking- directory, marked whole, synced, then renamed to its name; held by its units; bounded and swept.
// Each chunk's blob comes through the blob cache, hashed, and unpacks as builder.UnpackChunk unpacks it: Unpack's
// every refusal, and any entry outside the chunk's range. The index's ranges never meet (builder.CheckChunks), so no
// chunk writes where another did.
//
// A tree assembled from nothing costs every chunk. One that shares at least half its bytes with a tree the runner
// keeps is made from the nearest such tree instead: no unit holding it, and checked unchanged since it was made, it is
// renamed into the new tree's unpacking, the entries of its chunks the new tree lacks are removed (and the directories
// that leaves empty), and only the new tree's other chunks are fetched and unpacked in. A tree one chunk away from a
// kept one costs that chunk. The kept tree is spent; going back to it is one chunk away again.
//
// Not hard links: an inode shared between trees would carry a test's write into a source file to every tree holding
// it, and shares its mode, which must be the archive's. Not reflinks: ext4 and tmpfs, where Linux runners keep their
// roots, have none. Not a copy: 98,459 files cost about what unpacking them does.
//
// A tree's state, <sum>.state, is written once it is whole and named: each entry's type, mode, size, inode, change and
// modification times, and the names at its top. A test that writes, makes, removes, renames or chmods anything in the
// tree changes the change time of that entry or of its directory, which no process can set back, so a tree whose
// state no longer matches is never made into another one, and its state is removed. On tmpfs a write through a shared
// memory map changes no time, so no tree kept there is made into another (keptTreesUnchecked). A state is removed before its
// tree is spent or removed, and written only after its tree is named, so it never describes another tree.

// keptTreesUnchecked reports whether trees kept under path can't be checked unchanged (sharedWritesUnseen: on tmpfs),
// so none is made into another there. Tests set it.
var keptTreesUnchecked = sharedWritesUnseen

// sourceStateSuffix names a tree's state beside it.
const sourceStateSuffix = ".state"

// A sourceState is what a tree was when it was assembled.
type sourceState struct {
	Chunks  []builder.SourceChunk
	Top     []string
	Entries []stateEntry
}

// A stateEntry is one entry under a tree's top, as lstat saw it.
type stateEntry struct {
	Name     string
	Mode     fs.FileMode
	Size     int64
	Inode    uint64
	Changed  int64
	Modified int64
}

func entryOf(name string, info fs.FileInfo) stateEntry {
	inode, changed := inodeAndChange(info)
	return stateEntry{Name: name, Mode: info.Mode(), Size: info.Size(), Inode: inode, Changed: changed, Modified: info.ModTime().UnixNano()}
}

func (cache sourceCache) statePath(sum string) string {
	return filepath.Join(cache.directory, sum+sourceStateSuffix)
}

// stateOf is the state of the tree at directory, assembled from chunks, read before it is named: once it is, a unit
// may run tests in it, and what they write must never be taken for what was assembled. Renaming the tree changes no
// entry under its top.
func stateOf(directory string, chunks []builder.SourceChunk) (sourceState, error) {
	state := sourceState{Chunks: chunks}
	tops, err := os.ReadDir(directory)
	if err != nil {
		return state, err
	}
	for _, top := range tops {
		state.Top = append(state.Top, top.Name())
	}
	err = filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(directory, path)
		if err != nil || name == "." {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		state.Entries = append(state.Entries, entryOf(filepath.ToSlash(name), info))
		return nil
	})
	return state, err
}

// writeState writes tree sum's state, once the tree is named, through a file in scratch, the assembly's own locked
// directory, so a runner killed partway leaves nothing a sweep keeps.
func (cache sourceCache) writeState(sum string, state sourceState, scratch string) error {
	partial, err := os.CreateTemp(scratch, "state-")
	if err != nil {
		return err
	}
	err = gob.NewEncoder(partial).Encode(state)
	if closeErr := partial.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(partial.Name(), cache.statePath(sum))
	}
	if err != nil {
		os.Remove(partial.Name())
	}
	return err
}

// readState reads tree sum's state.
func (cache sourceCache) readState(sum string) (sourceState, error) {
	file, err := os.Open(cache.statePath(sum))
	if err != nil {
		return sourceState{}, err
	}
	defer file.Close()
	var state sourceState
	err = gob.NewDecoder(file).Decode(&state)
	return state, err
}

// changed says how the tree at directory differs from state, "" when it doesn't.
func changed(directory string, state sourceState) string {
	tops, err := os.ReadDir(directory)
	if err != nil {
		return err.Error()
	}
	names := []string{}
	for _, top := range tops {
		names = append(names, top.Name())
	}
	if !slices.Equal(names, state.Top) {
		return fmt.Sprintf("its top holds %q, not %q", names, state.Top)
	}
	for _, recorded := range state.Entries {
		info, err := os.Lstat(filepath.Join(directory, filepath.FromSlash(recorded.Name)))
		if err != nil {
			return err.Error()
		}
		if now := entryOf(recorded.Name, info); now != recorded {
			return fmt.Sprintf("%s changed since it was assembled", recorded.Name)
		}
	}
	return ""
}

// A sourceBase is a kept tree a new one may be made from. nearest picks it unlocked; claim holds it exclusively, only
// once the chunks it lacks are fetched, so a unit of that tree waits on it for no more than the assembly itself.
type sourceBase struct {
	sum       string
	directory string
	state     sourceState
	shared    int64
	lock      *os.File
}

// release lets a claimed base go unspent.
func (base *sourceBase) release() {
	if base != nil && base.lock != nil {
		base.lock.Close()
		base.lock = nil
	}
}

// nearest is the kept tree sharing the most bytes of chunks with chunks, at least half of them, whole, and that no unit
// holds now (its lock tried and let go at once); nil when none is. It holds nothing: assemble claims it, and checks it
// unchanged, only once the chunks it lacks are fetched. why says what it passed over.
func (cache sourceCache) nearest(chunks []builder.SourceChunk) (base *sourceBase, why []string) {
	if keptTreesUnchecked(cache.directory) {
		return nil, []string{"no kept tree is made into another here: on this filesystem (tmpfs) a write through a shared memory map changes a file and no time a check could see"}
	}
	wanted := map[builder.SourceChunk]bool{}
	total := int64(0)
	for _, chunk := range chunks {
		wanted[chunk] = true
		total += chunk.Bytes
	}
	states, _ := filepath.Glob(filepath.Join(cache.directory, "*"+sourceStateSuffix))
	candidates := []*sourceBase{}
	for _, path := range states {
		sum := strings.TrimSuffix(filepath.Base(path), sourceStateSuffix)
		if !protocol.Sha256Pattern.MatchString(sum) {
			continue
		}
		state, err := cache.readState(sum)
		if err != nil {
			why = append(why, fmt.Sprintf("tree %s's state: %v", sum, err))
			continue
		}
		candidate := &sourceBase{sum: sum, directory: filepath.Join(cache.directory, sum), state: state}
		for _, chunk := range state.Chunks {
			if wanted[chunk] {
				candidate.shared += chunk.Bytes
			}
		}
		if candidate.shared*2 >= total && candidate.shared > 0 {
			candidates = append(candidates, candidate)
		}
	}
	sort.SliceStable(candidates, func(left, right int) bool { return candidates[left].shared > candidates[right].shared })
	for _, candidate := range candidates {
		lockPath := filepath.Join(cache.directory, candidate.sum+".lock")
		lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
		if err != nil {
			continue
		}
		if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
			lock.Close()
			why = append(why, fmt.Sprintf("tree %s is held by a unit", candidate.sum))
			continue
		}
		lock.Close()
		if !(&heldSource{sum: candidate.sum, directory: candidate.directory}).whole() {
			continue
		}
		return candidate, why
	}
	return nil, why
}

// claim holds base exclusively, so no unit holds it, and checks it is still whole and unchanged since it was
// assembled; it says why when it can't, and a changed one's state is removed, so it is never checked again.
func (cache sourceCache) claim(base *sourceBase) string {
	lockPath := filepath.Join(cache.directory, base.sum+".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err.Error()
	}
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		lock.Close()
		return fmt.Sprintf("tree %s is held by a unit", base.sum)
	}
	// A lock file another removal unlinked meanwhile holds nothing: the tree's holders lock the one at the path.
	locked, lockedErr := lock.Stat()
	named, namedErr := os.Stat(lockPath)
	if lockedErr != nil || namedErr != nil || !os.SameFile(locked, named) {
		lock.Close()
		return fmt.Sprintf("tree %s was removed", base.sum)
	}
	base.lock = lock
	difference := "it isn't whole"
	if (&heldSource{sum: base.sum, directory: base.directory}).whole() {
		difference = changed(filepath.Join(base.directory, sourceTreeName), base.state)
	}
	if difference != "" {
		os.Remove(cache.statePath(base.sum))
		base.release()
		return fmt.Sprintf("tree %s isn't made into another: %s", base.sum, difference)
	}
	return ""
}

// sourceBytes is chunks' size, gzipped.
func sourceBytes(chunks []builder.SourceChunk) int64 {
	total := int64(0)
	for _, chunk := range chunks {
		total += chunk.Bytes
	}
	return total
}

// An assembly is how a tree was assembled, for the unit's record.
type assembly struct {
	base     string
	passed   string // why the base it was given wasn't claimed
	kept     int
	removed  int
	unpacked int
	stateErr error
}

// assemble makes the tree held from chunks: from base when there is one and claim takes it (spending it), each chunk
// the base lacks opened through open and unpacked in, then marked, synced and renamed to its name, and its state
// recorded. A base it can't claim is passed over, and the tree assembled from nothing, open fetching what it lacks. A tree
// another unit assembled meanwhile is taken as it is. assembleContext bounds the wait for another unit's assembly and
// the assembly itself.
func (cache sourceCache) assemble(assembleContext context.Context, held *heldSource, chunks []builder.SourceChunk, base *sourceBase,
	open func(assembleContext context.Context, sum string) (*os.File, error)) (assembly, error) {
	defer base.release()
	done := assembly{}
	value, _ := sourceLocks.LoadOrStore(held.directory, make(chan struct{}, 1))
	slot := value.(chan struct{})
	select {
	case slot <- struct{}{}:
		defer func() { <-slot }()
	case <-assembleContext.Done():
		return done, fmt.Errorf("waiting for another unit's assembly of it: %w", assembleContext.Err())
	}
	if held.whole() {
		held.ready = true
		return done, nil
	}
	if _, err := os.Lstat(held.directory); err == nil {
		if held.whole() {
			held.ready = true
			return done, nil
		}
		if err := cache.moveAway(assembleContext, held.directory); err != nil {
			return done, fmt.Errorf("removing an unmarked source at %s: %w", held.directory, err)
		}
	}
	// Held while it assembles: an unpacking whose lock is free is a dead runner's, swept with whatever it holds.
	lock, err := lockedTemporary(assembleContext, cache.directory, unpackingPrefix+held.sum+"-", true)
	if err != nil {
		return done, err
	}
	defer lock.Close()
	scratch := lock.Name()
	// Removed while still locked, whatever it holds: nothing, once the source is named.
	defer removeDirectory(scratch)
	// The source's directory, renamed to its name whole: its tree, and its marker beside it, never in it.
	source := filepath.Join(scratch, "source")
	tree := filepath.Join(source, sourceTreeName)
	kept := map[builder.SourceChunk]bool{}
	if base != nil {
		if done.passed = cache.claim(base); done.passed != "" {
			base = nil
		}
	}
	if base != nil {
		if kept, err = cache.spend(base, chunks, tree, &done); err != nil {
			return done, fmt.Errorf("making it from tree %s: %w", base.sum, err)
		}
	}
	for _, chunk := range chunks {
		if kept[chunk] {
			continue
		}
		file, err := open(assembleContext, chunk.Blob)
		if err != nil {
			return done, err
		}
		err = builder.UnpackChunk(contextReader{assembleContext, file}, tree, chunk)
		file.Close()
		if err != nil {
			return done, err
		}
		done.unpacked++
	}
	if err = os.MkdirAll(tree, 0o755); err == nil {
		err = os.WriteFile(filepath.Join(source, sourceMarker), markerContent(held.sum), 0o444)
	}
	if err != nil {
		return done, err
	}
	state, err := stateOf(tree, chunks)
	if err != nil {
		return done, err
	}
	// Everything under the name is on disk before the name is.
	syscall.Sync()
	if err = os.Rename(source, held.directory); err != nil {
		if held.whole() {
			held.ready = true
			return done, nil
		}
		return done, err
	}
	held.ready = true
	// A tree with no state serves its units, and is only never made into another.
	done.stateErr = cache.writeState(held.sum, state, scratch)
	return done, nil
}

// spend makes base, held exclusively, the start of a new tree at tree: its state and its marker removed, so a runner
// killed from here on leaves at its name only a source never trusted, or nothing, never a state or a marker naming
// another tree; then its tree renamed there, its emptied directory and its lock file removed, and the entries of each
// of its chunks chunks lacks removed, deepest first, then the directories that leaves empty. It returns the chunks the
// tree still holds.
func (cache sourceCache) spend(base *sourceBase, chunks []builder.SourceChunk, tree string, done *assembly) (map[builder.SourceChunk]bool, error) {
	if err := os.Remove(cache.statePath(base.sum)); err != nil {
		return nil, err
	}
	if err := os.Remove(filepath.Join(base.directory, sourceMarker)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(tree), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(filepath.Join(base.directory, sourceTreeName), tree); err != nil {
		return nil, err
	}
	if err := os.Remove(base.directory); err != nil {
		return nil, err
	}
	os.Remove(filepath.Join(cache.directory, base.sum+".lock"))
	base.release()
	done.base = base.sum
	// A chunk is kept whole or not at all: its blob, its range and its count, so a range the new index draws
	// narrower around the same blob unpacks again, refused as a fresh assembly would refuse it.
	wanted := map[builder.SourceChunk]bool{}
	for _, chunk := range chunks {
		wanted[chunk] = true
	}
	kept := map[builder.SourceChunk]bool{}
	for _, chunk := range base.state.Chunks {
		if wanted[chunk] {
			kept[chunk] = true
			done.kept++
		}
	}
	root, err := os.OpenRoot(tree)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	directories := []string{}
	for index := len(base.state.Entries) - 1; index >= 0; index-- {
		entry := base.state.Entries[index]
		if entry.Mode.IsDir() {
			directories = append(directories, entry.Name)
			continue
		}
		holder := sort.Search(len(base.state.Chunks), func(position int) bool { return base.state.Chunks[position].Last >= entry.Name })
		if holder < len(base.state.Chunks) && base.state.Chunks[holder].Holds(entry.Name) && kept[base.state.Chunks[holder]] {
			continue
		}
		if err := root.Remove(entry.Name); err != nil {
			return nil, err
		}
		done.removed++
	}
	// Deepest first, as they were gathered: a directory a kept entry is in isn't empty, and stays.
	for _, directory := range directories {
		if err := root.Remove(directory); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return nil, err
		}
	}
	return kept, nil
}
