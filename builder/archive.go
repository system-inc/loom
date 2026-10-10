package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Everything the store holds for a build is gzip, Go's own (Workshop, Oct 10: a 3,789-case corpus is 39 MB as a tar
// and 1 MB gzipped, a 26 MB test binary 8 MB, each unpacked in 0.13 s), so a runner needs nothing but loom-runner.
// A product is one archive, a gzipped tar of its files, and each chunk of a tree's source another (chunks.go). All are
// deterministic: entries sorted by name, every time the epoch, no owner, a file's mode 0644 or 0755 by its executable
// bit, and a gzip header with no name and no time, so the same files always make the same bytes and the same sha256.

// An archiveEntry is a regular file read from File, or, with Link set, a symbolic link to Link.
type archiveEntry struct {
	Name       string
	File       string
	Executable bool
	Link       string
}

// writeArchive is the gzipped tar of entries, deterministically.
func writeArchive(entries []archiveEntry) ([]byte, error) {
	entries = append([]archiveEntry{}, entries...)
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name < entries[right].Name })
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressor)
	for index, entry := range entries {
		if index > 0 && entries[index-1].Name == entry.Name {
			return nil, fmt.Errorf("%s is in the archive twice", entry.Name)
		}
		header := &tar.Header{Name: entry.Name, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		var file *os.File
		if entry.Link != "" {
			header.Typeflag, header.Linkname, header.Mode = tar.TypeSymlink, entry.Link, 0o777
		} else {
			opened, err := os.Open(entry.File)
			if err != nil {
				return nil, err
			}
			file = opened
			info, err := file.Stat()
			if err != nil {
				file.Close()
				return nil, err
			}
			header.Typeflag, header.Size, header.Mode = tar.TypeReg, info.Size(), 0o644
			if entry.Executable {
				header.Mode = 0o755
			}
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, err
		}
		if file != nil {
			_, err := io.Copy(writer, file)
			file.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", entry.Name, err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	if err := compressor.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// gzipped is content gzipped with no name and no time, as a test binary is stored.
func gzipped(content []byte) ([]byte, error) {
	var buffer bytes.Buffer
	compressor := gzip.NewWriter(&buffer)
	if _, err := compressor.Write(content); err != nil {
		return nil, err
	}
	if err := compressor.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// gunzipped is a gzip blob's content.
func gunzipped(blob []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(blob))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// ProductArchive is the archive of the named buildcache products in cache: each product's files under its key,
// <key>/<file>, and its description beside it, <key>.inputs, as buildcache lays them out. It returns the archive
// and how many files it holds.
func ProductArchive(cache string, products []string) ([]byte, int, error) {
	outputs, files, err := OutputsOf(cache, products)
	if err != nil {
		return nil, 0, err
	}
	entries := make([]archiveEntry, len(outputs))
	for index, output := range outputs {
		entries[index] = archiveEntry{Name: output.Path, File: files[output.Path], Executable: output.Executable}
	}
	archive, err := writeArchive(entries)
	return archive, len(entries), err
}

// ModuleCacheArchive is the archive of a tree's module download cache: every module in its build graph (go mod download
// all, run in the tree, workspace and all, into a scratch GOMODCACHE), as GOMODCACHE/cache/download lays them out,
// which is what GOPROXY=file:// reads, without its sumdb answers or lock files. A runner's read-only go queries read
// modules from it and from nowhere else, never the network. environment is added to go's: where the modules come from.
// Measured Oct 10: adamic's is 63 MB, and go list -deps -test ./... runs from it with GOPROXY=file:// and nothing else.
func ModuleCacheArchive(tree string, environment []string) ([]byte, error) {
	scratch, err := os.MkdirTemp("", "loom-modules-")
	if err != nil {
		return nil, err
	}
	defer func() {
		// go makes its module cache read-only; go clean -modcache is how it is removed.
		clean := exec.Command("go", "clean", "-modcache")
		clean.Env = append(os.Environ(), "GOMODCACHE="+scratch, "GOFLAGS=")
		clean.Run()
		os.RemoveAll(scratch)
	}()
	command := exec.Command("go", "mod", "download", "all")
	command.Dir = tree
	command.Env = append(append(os.Environ(), "GOMODCACHE="+scratch, "GOFLAGS="), environment...)
	if output, err := command.CombinedOutput(); err != nil {
		tail := output
		if len(tail) > 4000 {
			tail = tail[len(tail)-4000:]
		}
		return nil, fmt.Errorf("go mod download all: %v\n%s", err, tail)
	}
	download := filepath.Join(scratch, "cache", "download")
	entries := []archiveEntry{}
	err = filepath.WalkDir(download, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name, err := filepath.Rel(download, file)
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir() && name == "sumdb":
			return filepath.SkipDir
		case entry.IsDir() || strings.HasSuffix(name, ".lock") || !entry.Type().IsRegular():
			return nil
		}
		entries = append(entries, archiveEntry{Name: filepath.ToSlash(name), File: file})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		// A tree that needs no module has none.
		err = nil
	}
	if err != nil {
		return nil, err
	}
	return writeArchive(entries)
}

// checkLink refuses a symbolic link at name whose target could lead out of the directory it is unpacked into: an
// absolute or unclean target, one that climbs above the top, or a .. after a name, which could climb out of a
// directory another link leads into. Nor may a link lead to itself or to a directory it sits in (`.`, `..` from
// one level down): a name another entry spells differently, which a filesystem that folds case or normalization takes
// for the same one, could then reach the link's own directory again a level deeper than its name says, and climb
// out from there (an adversarial review, Oct 10: L -> ., l/M -> ., l/m/x -> ../.. on APFS). Every other link stays
// inside, however links chain.
func checkLink(name, target string) error {
	if target == "" || path.IsAbs(target) || path.Clean(target) != target {
		return fmt.Errorf("%s links to %q, not a clean relative path", name, target)
	}
	climbing := true
	for _, part := range strings.Split(target, "/") {
		if part != ".." {
			climbing = false
		} else if !climbing {
			return fmt.Errorf("%s links to %q, which climbs after a name", name, target)
		}
	}
	resolved := path.Join(path.Dir(name), target)
	if !filepath.IsLocal(filepath.FromSlash(resolved)) {
		return fmt.Errorf("%s links to %q, outside the directory", name, target)
	}
	if resolved == "." || resolved == name || strings.HasPrefix(name, resolved+"/") {
		return fmt.Errorf("%s links to %q, itself or a directory it is in", name, target)
	}
	return nil
}

// errOutside is an archive entry that would land outside its directory.
var errOutside = errors.New("outside its directory")

// maxEntryName and maxEntryDepth bound an entry's name, in bytes, and its levels: Linux's PATH_MAX, and far deeper
// than a tree's (2016af55's deepest is 13 levels, its longest name 196 bytes).
const (
	maxEntryName  = 4096
	maxEntryDepth = 256
)

// Unpacking a tree's source (98,381 files, Oct 10) through an os.Root walked every entry's path from the top three
// times, to look its parents up, to make them and to open the file, and a fourth time to set its mode: 98.6% of the
// 42 s it took on a Mac was those walks' system calls, against 8.5 s for tar -xzf. A parentChain keeps the last
// entry's directories open instead, each one looked up on disk once, so an entry beside the last one is one openat:
// 8.7 s for the same archive, the same 101,536 names, modes, links and bytes.

// A parentChain is the directories of the last entry unpacked, open, top first: names[index] is the directory
// roots[index+1] holds, as the archive spelled it, and roots[0] is the directory unpacked into.
type parentChain struct {
	names []string
	roots []*os.Root
}

// close closes every directory the chain opened, never the top.
func (chain *parentChain) close() {
	chain.cut(0)
}

// cut keeps the first count directories below the top open and closes the rest.
func (chain *parentChain) cut(count int) {
	for _, root := range chain.roots[count+1:] {
		root.Close()
	}
	chain.names, chain.roots = chain.names[:count], chain.roots[:count+1]
}

// parent opens the directory entry name goes in and returns it with name's last element. Each directory on the way
// that isn't already open is looked up on disk in the one above it (with case and normalization folded where the
// filesystem folds them), made when it is missing, and refused when it is a link (or, by the open, not a directory).
// A directory once open stays one: Unpack removes nothing, and a later file or link at its name, however spelled,
// finds it there, so the chain is never looked up again while it stays open.
func (chain *parentChain) parent(name string) (*os.Root, string, error) {
	directory, base := path.Split(name)
	parts := []string{}
	if directory != "" {
		parts = strings.Split(strings.TrimSuffix(directory, "/"), "/")
	}
	kept := 0
	for kept < len(parts) && kept < len(chain.names) && chain.names[kept] == parts[kept] {
		kept++
	}
	chain.cut(kept)
	for index := kept; index < len(parts); index++ {
		above, part := chain.roots[index], parts[index]
		spelled := path.Join(parts[:index+1]...)
		info, err := above.Lstat(part)
		if errors.Is(err, fs.ErrNotExist) {
			if err = above.Mkdir(part, 0o755); err == nil {
				info, err = above.Lstat(part)
			}
		}
		if err != nil {
			return nil, "", err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil, "", fmt.Errorf("entry %q is under the link %s: %w", name, spelled, errOutside)
		}
		// Opened through the one above, it is inside the top whatever is at its name now.
		opened, err := above.OpenRoot(part)
		if err != nil {
			return nil, "", err
		}
		chain.names, chain.roots = append(chain.names, part), append(chain.roots, opened)
	}
	return chain.roots[len(parts)], base, nil
}

// Unpack gunzips and untars archive into directory, which it creates, writing through an os.Root on it. It refuses
// (with nothing promised about what it already wrote, so a caller unpacks into scratch) an entry whose name isn't
// a clean local path, one whose parents on disk pass through a link, one named twice, a link checkLink refuses, any
// entry but a file or a link, and one allowed rejects (nil allows every name). It reads archive as a stream, so a
// tree's source (450 MB gzipped, Oct 10) needn't be held in memory.
func Unpack(archive io.Reader, directory string, allowed func(name string) bool) error {
	reader, err := gzip.NewReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	if err = os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	chain := &parentChain{roots: []*os.Root{root}}
	defer chain.close()
	entries := tar.NewReader(reader)
	seen := map[string]bool{}
	for {
		header, err := entries.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := header.Name
		// Before any parent is opened: the chain holds a descriptor for every level (a review, Oct 10: a name 2,000
		// deep ran out a 512-descriptor limit).
		if depth := strings.Count(name, "/") + 1; len(name) > maxEntryName || depth > maxEntryDepth {
			return fmt.Errorf("entry %q is %d bytes and %d levels deep, over the %d and %d an archive may hold", name[:min(len(name), 200)], len(name), depth, maxEntryName, maxEntryDepth)
		}
		if !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name {
			return fmt.Errorf("entry %q: %w", name, errOutside)
		}
		if seen[name] {
			return fmt.Errorf("entry %q is in the archive twice", name)
		}
		seen[name] = true
		if allowed != nil && !allowed(name) {
			return fmt.Errorf("entry %q isn't one this archive may hold", name)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeSymlink {
			return fmt.Errorf("entry %q is neither a file nor a link (type %q)", name, header.Typeflag)
		}
		if header.Typeflag == tar.TypeSymlink {
			if err = checkLink(name, header.Linkname); err != nil {
				return fmt.Errorf("%w: %w", err, errOutside)
			}
		}
		parent, base, err := chain.parent(name)
		if err != nil {
			return err
		}
		if header.Typeflag == tar.TypeSymlink {
			if err = parent.Symlink(header.Linkname, base); err != nil {
				return err
			}
			continue
		}
		mode := os.FileMode(0o644)
		if header.Mode&0o111 != 0 {
			mode = 0o755
		}
		output, err := parent.OpenFile(base, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		_, err = io.Copy(output, entries)
		if err == nil {
			// The file it just made, by its descriptor: the mode the umask may have narrowed.
			err = output.Chmod(mode)
		}
		if closeErr := output.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("entry %q: %w", name, err)
		}
	}
}
