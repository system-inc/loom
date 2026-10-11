package placer

import (
	"strconv"

	"github.com/system-inc/loom/jsonlines"
)

// FileLedger is the ledger as a file of JSON lines (jsonlines), one Record each, appended and synced before the placer
// starts what it records. A last line a crash cut short is dropped when the file is opened: its write never finished,
// so nothing it would have recorded was started. One placer holds it at a time: two would each start the same attempt,
// and the wire takes a second post of the same plan.
type FileLedger struct {
	file *jsonlines.File[Record]
	MemoryLedger
}

// OpenLedger takes the ledger at path, which need not exist yet, for this process's life, refused at once when
// another placer holds it, then its records.
func OpenLedger(path string) (*FileLedger, error) {
	file, records, err := jsonlines.Open[Record](path, "another placer holds it, and two would start each attempt twice")
	if err != nil {
		return nil, err
	}
	ledger := &FileLedger{file: file}
	for _, record := range records {
		ledger.MemoryLedger.Append(record)
	}
	return ledger, nil
}

// Close gives the ledger up, for another placer (or a test) to take.
func (ledger *FileLedger) Close() error {
	return ledger.file.Close()
}

// Append writes the record's line and syncs it, then holds it. A record whose line didn't reach the file isn't held.
func (ledger *FileLedger) Append(record Record) error {
	if err := ledger.file.Append(record); err != nil {
		return err
	}
	return ledger.MemoryLedger.Append(record)
}

// Compact keeps only the attempts keep says to, rewriting the file whole.
func (ledger *FileLedger) Compact(keep func(Record) bool) error {
	kept, records := MemoryLedger{}, []Record{}
	for _, record := range ledger.MemoryLedger.records {
		if keep(record) {
			records = append(records, record)
			kept.Append(record)
		}
	}
	if err := ledger.file.Rewrite(records); err != nil {
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

// Running are the attempts whose run was started and hasn't been seen to end or been stopped: what the placer may stop
// once no live change lists their future (#drrnnkh).
func (ledger *MemoryLedger) Running() []Record {
	running := []Record{}
	for _, record := range ledger.records {
		if len(record.Placed) > 0 && record.StartFailed == "" && record.Exit == "" && record.Stopped == "" && record.Void == "" {
			running = append(running, record)
		}
	}
	return running
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
