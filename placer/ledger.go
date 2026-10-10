package placer

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
)

// FileLedger is the ledger as a file of JSON lines, one Record each, appended and synced before the placer starts
// what it records. A last line a crash cut short (no newline) is dropped when the file is opened: its write never
// finished, so nothing it would have recorded was started.
type FileLedger struct {
	path string
	MemoryLedger
}

// OpenLedger reads the ledger at path, which need not exist yet.
func OpenLedger(path string) (*FileLedger, error) {
	ledger := &FileLedger{path: path}
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return ledger, nil
	}
	if err != nil {
		return nil, err
	}
	if cut := bytes.LastIndexByte(content, '\n') + 1; cut < len(content) {
		if err := os.Truncate(path, int64(cut)); err != nil {
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
			return nil, fmt.Errorf("%s:%d: %w", path, number+1, err)
		}
		ledger.MemoryLedger.Append(record)
	}
	return ledger, nil
}

// Append writes the record's line and syncs it, then holds it.
func (ledger *FileLedger) Append(record Record) error {
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(ledger.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return ledger.MemoryLedger.Append(record)
}

// MemoryLedger holds records in this process only: --dry-run's ledger, and FileLedger's index.
type MemoryLedger struct {
	records map[string]Record
	voids   map[string]Record
}

func attemptKey(future string, attempt int) string {
	return future + "-" + strconv.Itoa(attempt)
}

// Find is the attempt's record.
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
	if record.Void != "" {
		ledger.voids[record.Future] = record
	}
	return nil
}
