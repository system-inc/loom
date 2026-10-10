package builder

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// An Index is Workshop's record of what the action store holds (#k62gwdt). Workshop is the store's only writer, so
// what it has written is the store's contents: an action whose ref the index holds is stored, decided with no read,
// and a blob the index holds is never sent again. An index miss asks the store once and records the answer, so a
// new index seeds itself on its first build. The file is append-only lines, `ref <productKey> <manifest sha256>` and
// `blob <sha256>`, and one builder holds it at a time (an exclusive flock). A daily listing of the store checks it.
type Index struct {
	mutex sync.Mutex
	file  *os.File
	refs  map[string]string
	blobs map[string]bool
}

// OpenIndex loads the index in directory (creating it) and locks it for this builder, or fails at once when another
// builder holds it.
func OpenIndex(directory string) (*Index, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "index"), os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("another builder holds the index in %s: %w", directory, err)
	}
	index := &Index{file: file, refs: map[string]string{}, blobs: map[string]bool{}}
	scanner := bufio.NewScanner(file)
	for line := 1; scanner.Scan(); line++ {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 3 && fields[0] == "ref" && productKeyPattern.MatchString(fields[1]) && productKeyPattern.MatchString(fields[2]):
			index.refs[fields[1]] = fields[2]
		case len(fields) == 2 && fields[0] == "blob" && productKeyPattern.MatchString(fields[1]):
			index.blobs[fields[1]] = true
		default:
			file.Close()
			return nil, fmt.Errorf("index line %d is %q, not a ref or a blob", line, scanner.Text())
		}
	}
	if err = scanner.Err(); err != nil {
		file.Close()
		return nil, err
	}
	return index, nil
}

// Close releases the index's lock.
func (index *Index) Close() error {
	return index.file.Close()
}

// Ref is the manifest the store holds for key, as far as the index knows.
func (index *Index) Ref(key string) (string, bool) {
	index.mutex.Lock()
	defer index.mutex.Unlock()
	manifest, held := index.refs[key]
	return manifest, held
}

// Blob reports whether the store holds the blob, as far as the index knows.
func (index *Index) Blob(sum string) bool {
	index.mutex.Lock()
	defer index.mutex.Unlock()
	return index.blobs[sum]
}

// Counts are how many refs and blobs the index holds.
func (index *Index) Counts() (refs, blobs int) {
	index.mutex.Lock()
	defer index.mutex.Unlock()
	return len(index.refs), len(index.blobs)
}

// AddRef records an action ref the store now holds.
func (index *Index) AddRef(key, manifest string) error {
	index.mutex.Lock()
	defer index.mutex.Unlock()
	if held, known := index.refs[key]; known {
		if held != manifest {
			return fmt.Errorf("the index holds refs/action/%s as %s, not %s", key, held, manifest)
		}
		return nil
	}
	if _, err := fmt.Fprintf(index.file, "ref %s %s\n", key, manifest); err != nil {
		return err
	}
	index.refs[key] = manifest
	return nil
}

// AddBlob records a blob the store now holds.
func (index *Index) AddBlob(sum string) error {
	index.mutex.Lock()
	defer index.mutex.Unlock()
	if index.blobs[sum] {
		return nil
	}
	if _, err := fmt.Fprintf(index.file, "blob %s\n", sum); err != nil {
		return err
	}
	index.blobs[sum] = true
	return nil
}
