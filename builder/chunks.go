package builder

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/system-inc/loom/planner"
)

// Kirk's design (Oct 10 13:54Z): a tree's source is chunks, each a gzipped tar named by its sha256, so a change makes
// new chunks only where it changed. Workshop uploads only the chunks the store lacks, and a runner fetches only the
// chunks it doesn't already hold. Git's idea without the git server, on the CDN, keyless.
//
// A chunk is a run of the tree's tracked paths in byte order, the order the whole archive had, so it covers a range,
// first to last, and no two chunks' ranges meet. Where a run ends depends only on each path and its size, never on
// the order a listing gave or on any other file: after a path whose hash, read as a fraction, falls under its weight
// (its size and a tar header) over chunkTarget, around any path that weighs chunkTarget alone, and around each file of
// the tree's manifest (tracked.go). A one-file change
// that keeps the file's size so changes the one chunk holding it; one that changes its size can move the cut after it
// (two chunks), and one that grows it past chunkTarget makes it a chunk of its own, cutting its old chunk in two
// (three). Measured on
// 2016af55's source (Oct 10): see chunkTarget.

// chunkTarget is the weight a chunk holds on average: 1 MiB. Measured on 2016af55's source (98,459 files and links,
// 449.9 MB as one archive): 836 chunks, 450.1 MB in all, the median 100 KB gzipped, the largest 65 MB (one file, a
// heap snapshot); a one-file change in internal/ fetches a median 0.36 MB, in stage1/cohere/ 0.23 MB, in stage3/
// 0.23 MB. Half a MiB made 1,323 chunks for half that; 2 MiB, 463 for three to six times it.
var chunkTarget int64 = 1 << 20

// tarHeaderWeight is what each entry adds to its chunk beside its bytes: a tar header, and a file to make.
const tarHeaderWeight = 512

// A SourceChunk is one chunk of a tree's source, as its index lists it: its blob, the first and last paths it holds,
// how many entries (files and links) it holds, and its blob's size.
type SourceChunk struct {
	Blob  string `json:"blob"`
	First string `json:"first"`
	Last  string `json:"last"`
	Files int    `json:"files"`
	Bytes int64  `json:"bytes"`
}

// Holds reports whether name falls in the chunk's range.
func (chunk SourceChunk) Holds(name string) bool {
	return chunk.First <= name && name <= chunk.Last
}

// A Source is a tree's source as chunks: Chunks in path order, as the index lists them, and each one's blob by its
// sha256. PublishTree counts in Sent and SentBytes the chunks it had to send.
type Source struct {
	Chunks    []SourceChunk
	Blobs     map[string][]byte
	Sent      int
	SentBytes int64
}

// Bytes is the source's blobs' size, gzipped.
func (source Source) Bytes() int64 {
	total := int64(0)
	for _, chunk := range source.Chunks {
		total += chunk.Bytes
	}
	return total
}

// SourceChunks is the tree's tracked files, submodules included (git ls-files --recurse-submodules), and its manifest
// (TrackedManifest: what git answers about each repository in it, which a source with no .git can't ask), as chunks. A
// tracked symbolic link goes in as a link, and one Unpack would refuse fails here, at build time, as does a tree
// whose manifest can't be made, and one whose files aren't what its manifest records (checkSource). Git is asked
// through planner.LocalGit, as the manifest and the tree's hash are.
func SourceChunks(tree string) (Source, error) {
	listing, err := planner.LocalGit(tree, "ls-files", "--recurse-submodules", "-z").Output()
	if err != nil {
		return Source{}, fmt.Errorf("git ls-files in %s: %w", tree, err)
	}
	names := []string{}
	for _, name := range strings.Split(strings.TrimRight(string(listing), "\x00"), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	manifest, tracked, err := trackedManifest(tree)
	if err != nil {
		return Source{}, err
	}
	if err = checkSource(tree, names, tracked); err != nil {
		return Source{}, err
	}
	return chunkSource(tree, names, manifest)
}

// A sourceFile is one path and its weight. One alone is a chunk of its own, whatever it weighs.
type sourceFile struct {
	entry  archiveEntry
	weight int64
	alone  bool
}

// ChunkFiles is the named paths of tree, in whatever order they come, as chunks: each regular file and symbolic link
// (a link Unpack would refuse fails here), in byte order, cut where cutAfter says. Each chunk is archived as the whole
// source was, deterministically, all of them at once.
func ChunkFiles(tree string, names []string) (Source, error) {
	return chunkSource(tree, names, nil)
}

// chunkSource is ChunkFiles with the manifest's files beside the named paths, each a chunk of its own: a repository's
// files list then changes only its own chunk, when that repository moves, and the HEAD that changes on every commit
// takes no tracked file's chunk with it. A tracked path where the manifest goes is refused.
func chunkSource(tree string, names []string, manifest []archiveEntry) (Source, error) {
	files := make([]sourceFile, 0, len(names)+len(manifest))
	for _, entry := range manifest {
		files = append(files, sourceFile{entry: entry, weight: int64(len(entry.Content)) + tarHeaderWeight, alone: true})
	}
	for _, name := range names {
		if len(manifest) > 0 && (name == TrackedDirectory || strings.HasPrefix(name, TrackedDirectory+"/")) {
			return Source{}, fmt.Errorf("the tree tracks %s, where its source carries its manifest", name)
		}
		full := filepath.Join(tree, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if err != nil {
			return Source{}, fmt.Errorf("%s: %w", name, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return Source{}, err
			}
			if err = checkLink(name, target); err != nil {
				return Source{}, err
			}
			files = append(files, sourceFile{entry: archiveEntry{Name: name, Link: target}, weight: int64(len(target)) + tarHeaderWeight})
		case info.Mode().IsRegular():
			files = append(files, sourceFile{entry: archiveEntry{Name: name, File: full, Executable: info.Mode()&0o111 != 0}, weight: info.Size() + tarHeaderWeight})
		}
	}
	runs, err := chunkRuns(files)
	if err != nil {
		return Source{}, err
	}
	source := Source{Chunks: make([]SourceChunk, len(runs)), Blobs: map[string][]byte{}}
	blobs := make([][]byte, len(runs))
	err = each(len(runs), runtime.NumCPU(), func(index int) error {
		entries := make([]archiveEntry, len(runs[index]))
		for position, file := range runs[index] {
			entries[position] = file.entry
		}
		blob, err := writeArchive(entries)
		if err != nil {
			return err
		}
		blobs[index] = blob
		source.Chunks[index] = SourceChunk{Blob: digest(blob), First: entries[0].Name, Last: entries[len(entries)-1].Name, Files: len(entries), Bytes: int64(len(blob))}
		return nil
	})
	if err != nil {
		return Source{}, err
	}
	for index, chunk := range source.Chunks {
		source.Blobs[chunk.Blob] = blobs[index]
	}
	return source, nil
}

// chunkRuns sorts files by name and cuts them into runs: before a file that weighs chunkTarget alone or is to be alone,
// and after one cutAfter picks. A name listed twice is refused.
func chunkRuns(files []sourceFile) ([][]sourceFile, error) {
	files = append([]sourceFile{}, files...)
	sort.Slice(files, func(left, right int) bool { return files[left].entry.Name < files[right].entry.Name })
	runs := [][]sourceFile{}
	start := 0
	for index, file := range files {
		if index > 0 && files[index-1].entry.Name == file.entry.Name {
			return nil, fmt.Errorf("%s is in the source twice", file.entry.Name)
		}
		if (file.weight >= chunkTarget || file.alone) && index > start {
			runs, start = append(runs, files[start:index]), index
		}
		if cutAfter(file) {
			runs, start = append(runs, files[start:index+1]), index+1
		}
	}
	if start < len(files) {
		runs = append(runs, files[start:])
	}
	return runs, nil
}

// cutAfter reports whether a chunk ends after file: always for one that weighs chunkTarget alone or is to be alone, and
// otherwise when its path's hash, read as a fraction of the whole, is under its weight over chunkTarget. That is the
// chance a chunk ends there, so a chunk weighs chunkTarget on average, and it hangs on the file's own path and size
// alone.
func cutAfter(file sourceFile) bool {
	if file.weight >= chunkTarget || file.alone {
		return true
	}
	sum := sha256.Sum256([]byte("loom-source-chunk\n" + file.entry.Name))
	return binary.BigEndian.Uint64(sum[:8]) < uint64(file.weight)*(math.MaxUint64/uint64(chunkTarget))
}

// CheckChunks refuses a chunk list that isn't in path order with ranges that never meet, an empty chunk, a range whose
// first path is after its last, a blob that isn't a sha256, or one blob listed twice: two chunks that could both hold
// a path would let one write where another already had, and one blob can hold only one range's paths.
func CheckChunks(chunks []SourceChunk) error {
	if len(chunks) == 0 {
		return fmt.Errorf("its source has no chunks")
	}
	listed := map[string]bool{}
	for index, chunk := range chunks {
		switch {
		case listed[chunk.Blob]:
			return fmt.Errorf("its source lists chunk %s twice", chunk.Blob)
		case !productKeyPattern.MatchString(chunk.Blob):
			return fmt.Errorf("its source chunk %d is blob %q", index, chunk.Blob)
		case chunk.Files < 1 || chunk.First == "" || chunk.First > chunk.Last:
			return fmt.Errorf("its source chunk %s holds %d entries from %q to %q", chunk.Blob, chunk.Files, chunk.First, chunk.Last)
		case index > 0 && chunks[index-1].Last >= chunk.First:
			return fmt.Errorf("its source chunks %s (to %q) and %s (from %q) overlap", chunks[index-1].Blob, chunks[index-1].Last, chunk.Blob, chunk.First)
		}
		listed[chunk.Blob] = true
	}
	return nil
}

// SourceSum names a tree's source by its chunks: the sha256 of their list, so two indexes with the same chunks name
// the same source, and a runner keeps one tree under it.
func SourceSum(chunks []SourceChunk) string {
	encoded, _ := json.Marshal(chunks)
	sum := sha256.Sum256(append([]byte("loom-source-v2\n"), encoded...))
	return hex.EncodeToString(sum[:])
}

// UnpackChunk unpacks one chunk's blob into directory as Unpack does, refusing any entry outside the chunk's range
// and a chunk holding other than the entries its index says. Unpack's every other refusal holds, and since it looks
// each parent up on disk, an entry under a link another chunk made is refused too.
func UnpackChunk(blob io.Reader, directory string, chunk SourceChunk) error {
	count := 0
	err := Unpack(blob, directory, func(name string) bool {
		count++
		return chunk.Holds(name)
	})
	if err != nil {
		return fmt.Errorf("chunk %s: %w", chunk.Blob, err)
	}
	if count != chunk.Files {
		return fmt.Errorf("chunk %s holds %d entries and its index says %d", chunk.Blob, count, chunk.Files)
	}
	return nil
}
