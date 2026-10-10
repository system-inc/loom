package jsonlines

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type line struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// A ledger write that fails partway is cut back, so no half line sits before the next record. Mutant: no cut back.
func TestAFailedWriteLeavesNoHalfLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	file, _, err := Open[line](path, "another holds it")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := file.Append(line{Name: "one", Count: 1}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	saved := writeLine
	writeLine = func(handle *os.File, content []byte) error {
		handle.Write(content[:len(content)/2])
		return errors.New("no space left on device")
	}
	err = file.Append(line{Name: "two", Count: 2})
	writeLine = saved
	if after, _ := os.ReadFile(path); err == nil || string(after) != string(before) {
		t.Fatalf("after a failed write (%v) the ledger is %q, want %q", err, after, before)
	}
}

// One process holds a ledger at a time: a second Open is refused, saying why, until the first closes it. A crash's
// cut-short last line is dropped from the file when it's opened again, and a reader that takes no lock skips it and
// changes nothing. Rewrite replaces the records whole. Mutants: no lock; the cut-short line kept; Read truncating.
func TestALedgerIsHeldOnceAndACutShortLineIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	file, records, err := Open[line](path, "another holds it, and two would race")
	if err != nil || len(records) != 0 {
		t.Fatalf("a new ledger: %v %v", records, err)
	}
	file.Append(line{Name: "one", Count: 1})
	if second, _, err := Open[line](path, "another holds it, and two would race"); err == nil || !strings.Contains(err.Error(), "another holds it, and two would race") {
		t.Fatalf("a second holder took the ledger (%v)", err)
	} else if second != nil {
		t.Fatal("a refused ledger came back")
	}
	handle, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	handle.WriteString(`{"name":"two","co`)
	handle.Close()
	read, err := Read[line](path)
	if err != nil || len(read) != 1 || read[0] != (line{Name: "one", Count: 1}) {
		t.Fatalf("a lockless read: %v %v", read, err)
	}
	if content, _ := os.ReadFile(path); !strings.HasSuffix(string(content), `"co`) {
		t.Fatal("a lockless read changed the file")
	}
	file.Close()
	file, records, err = Open[line](path, "another holds it")
	if err != nil || len(records) != 1 || records[0].Name != "one" {
		t.Fatalf("reopened: %v %v", records, err)
	}
	defer file.Close()
	if content, _ := os.ReadFile(path); string(content) != "{\"name\":\"one\",\"count\":1}\n" {
		t.Fatalf("the ledger is %q, want its one whole line", content)
	}
	if err := file.Rewrite([]line{{Name: "three", Count: 3}}); err != nil {
		t.Fatal(err)
	}
	if read, err := Read[line](path); err != nil || len(read) != 1 || read[0].Name != "three" {
		t.Fatalf("after a rewrite: %v %v", read, err)
	}
	if missing, err := Read[line](filepath.Join(t.TempDir(), "none.jsonl")); err != nil || len(missing) != 0 {
		t.Fatalf("a ledger not yet written: %v %v", missing, err)
	}
}
