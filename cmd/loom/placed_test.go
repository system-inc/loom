package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The placer's ledger is read once and then only what was appended: a run's newest record wins, a line still being
// written waits for its newline, and a compaction (a new file renamed over the ledger) is read whole again. Mutants:
// the offset moved past a half line, which loses its record; a renamed file read from the old offset, which keeps a
// time the compaction dropped.
func TestPlacedReadsOnlyWhatTheLedgerAppended(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placed.jsonl")
	write := func(content string, flag int) {
		file, err := os.OpenFile(path, flag|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		file.WriteString(content)
		file.Close()
	}
	write(`{"future":"a","attempt":1,"run":"future-a-1","at":"2026-10-10T18:00:00Z"}`+"\n", os.O_APPEND)
	placed := placedFrom(path)
	if at, found := placed("future-a-1"); !found || at != "2026-10-10T18:00:00Z" {
		t.Fatalf("future-a-1 placed %q %v", at, found)
	}
	if _, found := placed("future-b-1"); found {
		t.Fatal("a run the ledger lacks was placed")
	}
	// A newer record of the same run, and a line the placer is still writing.
	write(`{"future":"a","attempt":1,"run":"future-a-1","at":"2026-10-10T18:05:00Z"}`+"\n"+`{"future":"b","attempt":1,"run":"future-b-1",`, os.O_APPEND)
	if at, _ := placed("future-a-1"); at != "2026-10-10T18:05:00Z" {
		t.Fatalf("future-a-1's newest record reads %q", at)
	}
	if _, found := placed("future-b-1"); found {
		t.Fatal("a half-written line was read")
	}
	write(`"at":"2026-10-10T18:06:00Z"}`+"\n", os.O_APPEND)
	if at, found := placed("future-b-1"); !found || at != "2026-10-10T18:06:00Z" {
		t.Fatalf("the finished line reads %q %v", at, found)
	}
	// A compaction keeps only b, through a file renamed over the ledger, longer than what was read.
	compacted := path + ".compact"
	if err := os.WriteFile(compacted, []byte(`{"future":"b","attempt":1,"run":"future-b-1","at":"2026-10-10T18:06:00Z"}`+"\n"+
		`{"future":"c","attempt":1,"run":"future-c-1","at":"2026-10-10T18:07:00Z","placed":[{"unitKey":"x","name":"padding padding padding padding padding padding","kind":"test","pools":["box-strict"]}]}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(compacted, path); err != nil {
		t.Fatal(err)
	}
	if _, found := placed("future-a-1"); found {
		t.Fatal("a run the compaction dropped is still placed")
	}
	if at, found := placed("future-c-1"); !found || at != "2026-10-10T18:07:00Z" {
		t.Fatalf("after the compaction future-c-1 reads %q %v", at, found)
	}
}
