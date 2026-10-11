package builder

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

// A tree's source was gzipped whole on every build (#s0cqqhk): adamic's 842 chunks, 457 MB gzipped from about 1 GB,
// 1.75 s of Workshop's 3.1 s source step, though a typical branch changes one chunk or three. A chunk's blob is a pure
// function of its entries, each one's name, kind (a file, an executable or a link), a link's target and a file's
// content, archived by writeArchive, and checkSource has just held every file to the blob its commit records. So the
// blobs' git names stand in for the contents: a chunk whose entries name the same blobs, kinds and targets as one
// built before is that chunk, byte for byte. A ChunkCache keeps each chunk it built on Workshop's own disk under that
// key, and a warm tree gzips only the chunks it changed.
//
// The key carries the archiver too (archiverName: this binary's Go release, and what its writeArchive makes of a fixed
// set of entries), so a cache another release wrote, whose gzip could make other bytes of the same files, is never
// read as this one's. A kept chunk carries its blob's sha256, checked as it is read, so a torn or rotted file is built
// again rather than shipped.

// chunkCacheFormat names how a chunk is keyed and kept here: a change to either is a new one.
const chunkCacheFormat = "loom-source-chunk-cache-v1"

// chunkCacheSuffix ends each kept chunk's file name, <key>.chunk.
const chunkCacheSuffix = ".chunk"

// A ChunkCache keeps a tree's built source chunks by what they hold, on the builder's own disk.
type ChunkCache struct {
	// Directory holds each chunk as <key>.chunk: its blob's sha256 in hex, a newline, and the blob.
	Directory string
	// Blobs names the git blob the file at path holds. nil means its commit's (git ls-tree -r, submodules recursed),
	// which checkSource has just held the checkout to. Another (the Resident's, from memory) must answer only for the
	// file as it is now: a chunk holding a file it doesn't answer for is built, and not kept.
	Blobs func(path string) (string, bool)
	// Bytes is the most Trim leaves in Directory: zero keeps everything.
	Bytes int64
}

// archiverName is this binary's archiver, by its Go release and the sha256 of what writeArchive makes of a fixed set
// of entries (a file, an empty one, an executable, a link and a name too long for a plain tar header), or "" when it
// can't be made, which keeps nothing.
var archiverName = sync.OnceValue(func() string {
	directory, err := os.MkdirTemp("", "loom-archiver-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(directory)
	executable := filepath.Join(directory, "run")
	if err = os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		return ""
	}
	archive, err := writeArchive([]archiveEntry{
		{Name: "a/file", Content: bytes.Repeat([]byte("loom source chunk\n"), 300)},
		{Name: "a/empty"},
		{Name: "a/run", File: executable, Executable: true},
		{Name: "a/link", Link: "file"},
		{Name: "a/" + strings.Repeat("long/", 30) + "name", Content: []byte{0, 1, 2, 255}},
	})
	if err != nil {
		return ""
	}
	return runtime.Version() + " " + digest(archive)
})

// key is the name a chunk of entries is kept under, and false when one of its files has no blob blobOf names, or the
// archiver has no name: such a chunk is built and not kept.
func (cache *ChunkCache) key(entries []archiveEntry, blobOf func(path string) (string, bool)) (string, bool) {
	archiver := archiverName()
	if cache == nil || archiver == "" {
		return "", false
	}
	sum := sha256.New()
	fmt.Fprintf(sum, "%s\n%s\n", chunkCacheFormat, archiver)
	for _, entry := range entries {
		switch {
		case entry.Link != "":
			fmt.Fprintf(sum, "%s\x00link\x00%s\x00", entry.Name, entry.Link)
		case entry.File == "":
			fmt.Fprintf(sum, "%s\x00content\x00%s\x00", entry.Name, digest(entry.Content))
		default:
			blob, named := blobOf(entry.Name)
			if !named || !objectName(blob) {
				return "", false
			}
			kind := "file"
			if entry.Executable {
				kind = "executable"
			}
			fmt.Fprintf(sum, "%s\x00%s\x00%s\x00", entry.Name, kind, blob)
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), true
}

// get is the chunk kept under key and its blob's sha256, or false when none is, or the one kept isn't whole.
func (cache *ChunkCache) get(key string) ([]byte, string, bool) {
	file := filepath.Join(cache.Directory, key+chunkCacheSuffix)
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, "", false
	}
	recorded, blob, found := bytes.Cut(content, []byte("\n"))
	if !found || digest(blob) != string(recorded) {
		return nil, "", false
	}
	// Trim removes the least recently used first.
	now := time.Now()
	os.Chtimes(file, now, now)
	return blob, string(recorded), true
}

// put keeps blob under key, whole or not at all: written beside its place and renamed into it. A chunk that can't be
// kept is built again next time, so a failed write costs time and never the build.
func (cache *ChunkCache) put(key string, blob []byte) {
	if err := os.MkdirAll(cache.Directory, 0o755); err != nil {
		return
	}
	staging, err := os.CreateTemp(cache.Directory, ".staging-")
	if err != nil {
		return
	}
	_, err = staging.Write(append([]byte(digest(blob)+"\n"), blob...))
	if closeErr := staging.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(staging.Name(), filepath.Join(cache.Directory, key+chunkCacheSuffix))
	}
	if err != nil {
		os.Remove(staging.Name())
	}
}

// Trim removes kept chunks, least recently used first, until Directory holds no more than Bytes, and staging files a
// write left behind over an hour ago. Zero Bytes keeps every chunk.
func (cache *ChunkCache) Trim() error {
	entries, err := os.ReadDir(cache.Directory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	type kept struct {
		path     string
		size     int64
		modified time.Time
	}
	chunks, total := []kept{}, int64(0)
	for _, entry := range entries {
		info, err := entry.Info()
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		file := filepath.Join(cache.Directory, entry.Name())
		switch {
		case strings.HasPrefix(entry.Name(), ".staging-") && time.Since(info.ModTime()) > time.Hour:
			if err = os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		case info.Mode().IsRegular() && strings.HasSuffix(entry.Name(), chunkCacheSuffix):
			chunks = append(chunks, kept{file, info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	if cache.Bytes <= 0 {
		return nil
	}
	sort.Slice(chunks, func(left, right int) bool { return chunks[left].modified.Before(chunks[right].modified) })
	for _, chunk := range chunks {
		if total <= cache.Bytes {
			break
		}
		if err = os.Remove(chunk.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		total -= chunk.size
	}
	return nil
}
