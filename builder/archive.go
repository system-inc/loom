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
// A product is one archive, a gzipped tar of its files, and a tree's source is another. Both are deterministic:
// entries sorted by name, every time the epoch, no owner, a file's mode 0644 or 0755 by its executable bit, and a
// gzip header with no name and no time, so the same files always make the same bytes and the same sha256.

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

// SourceArchive is the archive of the tree's tracked files, submodules included (git ls-files --recurse-submodules).
// A tracked symbolic link goes in as a link, and one Unpack would refuse fails here, at build time.
func SourceArchive(tree string) ([]byte, error) {
	command := exec.Command("git", "ls-files", "--recurse-submodules", "-z")
	command.Dir = tree
	listing, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files in %s: %w", tree, err)
	}
	entries := []archiveEntry{}
	for _, name := range strings.Split(strings.TrimRight(string(listing), "\x00"), "\x00") {
		if name == "" {
			continue
		}
		full := filepath.Join(tree, filepath.FromSlash(name))
		info, err := os.Lstat(full)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return nil, err
			}
			if err = checkLink(name, target); err != nil {
				return nil, err
			}
			entries = append(entries, archiveEntry{Name: name, Link: target})
		case info.Mode().IsRegular():
			entries = append(entries, archiveEntry{Name: name, File: full, Executable: info.Mode()&0o111 != 0})
		}
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

// checkParents refuses an entry whose parent directories, as the filesystem resolves them (case and normalization
// folded where it folds them), pass through a link: each one is looked up on disk through root, never by name.
func checkParents(root *os.Root, name string) error {
	parts := strings.Split(path.Dir(name), "/")
	for index := range parts {
		if parts[0] == "." {
			return nil
		}
		parent := path.Join(parts[:index+1]...)
		info, err := root.Lstat(filepath.FromSlash(parent))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("entry %q is under the link %s: %w", name, parent, errOutside)
		}
	}
	return nil
}

// errOutside is an archive entry that would land outside its directory.
var errOutside = errors.New("outside its directory")

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
		if !filepath.IsLocal(filepath.FromSlash(name)) || path.Clean(name) != name {
			return fmt.Errorf("entry %q: %w", name, errOutside)
		}
		if err = checkParents(root, name); err != nil {
			return err
		}
		if seen[name] {
			return fmt.Errorf("entry %q is in the archive twice", name)
		}
		seen[name] = true
		if allowed != nil && !allowed(name) {
			return fmt.Errorf("entry %q isn't one this archive may hold", name)
		}
		file := filepath.FromSlash(name)
		if parent := filepath.Dir(file); parent != "." {
			if err = root.MkdirAll(parent, 0o755); err != nil {
				return err
			}
		}
		switch header.Typeflag {
		case tar.TypeReg:
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			output, err := root.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			_, err = io.Copy(output, entries)
			if closeErr := output.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				return fmt.Errorf("entry %q: %w", name, err)
			}
			if err = root.Chmod(file, mode); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err = checkLink(name, header.Linkname); err != nil {
				return fmt.Errorf("%w: %w", err, errOutside)
			}
			if err = root.Symlink(header.Linkname, file); err != nil {
				return err
			}
		default:
			return fmt.Errorf("entry %q is neither a file nor a link (type %q)", name, header.Typeflag)
		}
	}
}
