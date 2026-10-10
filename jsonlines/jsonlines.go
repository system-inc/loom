// Package jsonlines is a daemon's ledger file: one JSON record a line, each appended and synced before what it records
// is done, held by one process at a time. The placer's ledger (placer.FileLedger) and Workshop's tree builder's
// (treebuilder) are each one.
package jsonlines

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// A File is a ledger file of records of type T, held by this process for its life.
type File[T any] struct {
	path string
	lock *os.File
}

// writeLine writes one record's line; a test swaps it to fail partway.
var writeLine = func(file *os.File, line []byte) error {
	_, err := file.Write(line)
	return err
}

// Open takes the file at path, which need not exist yet, for this process's life, and returns its records: an
// exclusive lock on <path>.lock, refused at once when another process holds it (held says why that matters, for the
// error), then the records. A last line a crash cut short (no newline) is dropped from the file: its write never
// finished, so nothing it would have recorded was done.
func Open[T any](path, held string) (*File[T], []T, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil, fmt.Errorf("%s: %s", path, held)
		}
		return nil, nil, fmt.Errorf("%s: locking: %w", path, err)
	}
	file := &File[T]{path: path, lock: lock}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil, nil
	}
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	if cut := bytes.LastIndexByte(content, '\n') + 1; cut < len(content) {
		if err := os.Truncate(path, int64(cut)); err != nil {
			file.Close()
			return nil, nil, fmt.Errorf("%s: dropping a cut-short last line: %w", path, err)
		}
	}
	records, err := parse[T](path, content)
	if err != nil {
		file.Close()
		return nil, nil, err
	}
	return file, records, nil
}

// Read reads the records of a file another process may hold and be appending to, taking no lock and changing
// nothing: a last line not yet whole is left out. A file that doesn't exist yet has none.
func Read[T any](path string) ([]T, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return parse[T](path, content)
}

// parse reads every whole line of content as a record.
func parse[T any](path string, content []byte) ([]T, error) {
	content = content[:bytes.LastIndexByte(content, '\n')+1]
	records := []T{}
	for number, line := range bytes.Split(bytes.TrimSuffix(content, []byte("\n")), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record T
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, number+1, err)
		}
		records = append(records, record)
	}
	return records, nil
}

// Close gives the file up, for another process (or a test) to take.
func (file *File[T]) Close() error {
	return file.lock.Close()
}

// Append writes the record's line and syncs it. A write or sync that fails cuts the file back to where it was, so a
// half line never sits before the next record.
func (file *File[T]) Append(record T) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	handle, err := os.OpenFile(file.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer handle.Close()
	held, err := handle.Stat()
	if err != nil {
		return err
	}
	if err := writeLine(handle, append(line, '\n')); err != nil {
		return file.cutBack(held.Size(), err)
	}
	if err := handle.Sync(); err != nil {
		return file.cutBack(held.Size(), err)
	}
	return nil
}

func (file *File[T]) cutBack(size int64, cause error) error {
	if err := os.Truncate(file.path, size); err != nil {
		return fmt.Errorf("%w; cutting %s back to %d bytes: %v", cause, file.path, size, err)
	}
	return cause
}

// Rewrite replaces the file's records with records, through a temporary file renamed over it, so a crash leaves the
// old file or the new one, never half of either.
func (file *File[T]) Rewrite(records []T) error {
	var content bytes.Buffer
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			return err
		}
		content.Write(append(line, '\n'))
	}
	temporary := file.path + ".compact"
	handle, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = handle.Write(content.Bytes())
	if err == nil {
		err = handle.Sync()
	}
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary, file.path)
	}
	if err != nil {
		os.Remove(temporary)
		return err
	}
	return nil
}
