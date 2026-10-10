package placer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// FileLedger is the ledger as a file of JSON lines, one Record each, appended and synced before the placer starts
// what it records. A last line a crash cut short (no newline) is dropped when the file is opened: its write never
// finished, so nothing it would have recorded was started. One placer holds it at a time: two would each start the
// same attempt, and the wire takes a second post of the same plan.
type FileLedger struct {
	path string
	lock *os.File
	MemoryLedger
}

// writeLine writes one record's line; a test swaps it to fail partway.
var writeLine = func(file *os.File, line []byte) error {
	_, err := file.Write(line)
	return err
}

// OpenLedger takes the ledger at path, which need not exist yet, for this process's life: an exclusive lock on
// <path>.lock, refused at once when another placer holds it, then the records.
func OpenLedger(path string) (*FileLedger, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: another placer holds it, and two would start each attempt twice", path)
		}
		return nil, fmt.Errorf("%s: locking: %w", path, err)
	}
	ledger := &FileLedger{path: path, lock: lock}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		ledger.Close()
		return nil, err
	}
	if cut := bytes.LastIndexByte(content, '\n') + 1; cut < len(content) {
		if err := os.Truncate(path, int64(cut)); err != nil {
			ledger.Close()
			return nil, fmt.Errorf("%s: dropping a cut-short last line: %w", path, err)
		}
		content = content[:cut]
	}
	for number, line := range bytes.Split(bytes.TrimSuffix(content, []byte("\n")), []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			ledger.Close()
			return nil, fmt.Errorf("%s:%d: %w", path, number+1, err)
		}
		ledger.MemoryLedger.Append(record)
	}
	return ledger, nil
}

// Close gives the ledger up, for another placer (or a test) to take.
func (ledger *FileLedger) Close() error {
	return ledger.lock.Close()
}

// Append writes the record's line and syncs it, then holds it. A write or sync that fails cuts the file back to
// where it was, so a half line never sits before the next record.
func (ledger *FileLedger) Append(record Record) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(ledger.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	held, err := file.Stat()
	if err != nil {
		return err
	}
	if err := writeLine(file, append(line, '\n')); err != nil {
		return ledger.cutBack(held.Size(), err)
	}
	if err := file.Sync(); err != nil {
		return ledger.cutBack(held.Size(), err)
	}
	return ledger.MemoryLedger.Append(record)
}

func (ledger *FileLedger) cutBack(size int64, cause error) error {
	if err := os.Truncate(ledger.path, size); err != nil {
		return fmt.Errorf("%w; cutting the ledger back to %d bytes: %v", cause, size, err)
	}
	return cause
}

// Compact keeps only the attempts keep says to, rewriting the file through a temporary one renamed over it.
func (ledger *FileLedger) Compact(keep func(Record) bool) error {
	kept := MemoryLedger{}
	var content bytes.Buffer
	for _, record := range ledger.MemoryLedger.records {
		if !keep(record) {
			continue
		}
		line, err := json.Marshal(record)
		if err != nil {
			return err
		}
		content.Write(append(line, '\n'))
		kept.Append(record)
	}
	temporary := ledger.path + ".compact"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(content.Bytes())
	if err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(temporary, ledger.path)
	}
	if err != nil {
		os.Remove(temporary)
		return err
	}
	ledger.MemoryLedger = kept
	return nil
}

// MemoryLedger holds records in this process only: --dry-run's ledger, and FileLedger's index. Each attempt's newest
// record stands for it.
type MemoryLedger struct {
	records map[string]Record
	voids   map[string]Record
}

func attemptKey(future string, attempt int) string {
	return future + "-" + strconv.Itoa(attempt)
}

// Find is the attempt's newest record.
func (ledger *MemoryLedger) Find(future string, attempt int) (Record, bool) {
	record, found := ledger.records[attemptKey(future, attempt)]
	return record, found
}

// LastVoid is the future's newest record that posted a void.
func (ledger *MemoryLedger) LastVoid(future string) (Record, bool) {
	record, found := ledger.voids[future]
	return record, found
}

// Append holds the record.
func (ledger *MemoryLedger) Append(record Record) error {
	if ledger.records == nil {
		ledger.records, ledger.voids = map[string]Record{}, map[string]Record{}
	}
	ledger.records[attemptKey(record.Future, record.Attempt)] = record
	if held, found := ledger.voids[record.Future]; record.Void != "" && (!found || record.Attempt >= held.Attempt) {
		ledger.voids[record.Future] = record
	}
	return nil
}

// Compact keeps only the attempts keep says to.
func (ledger *MemoryLedger) Compact(keep func(Record) bool) error {
	kept := MemoryLedger{}
	for _, record := range ledger.records {
		if keep(record) {
			kept.Append(record)
		}
	}
	*ledger = kept
	return nil
}
