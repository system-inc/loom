package livestatus

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A status is replaced whole: a reader racing a thousand writes reads every time either the old status or the new,
// never part of one, and nothing is left beside the file. Mutant: written in place (os.WriteFile), which a reader
// catches empty or half written.
func TestAStatusIsReplacedWhole(t *testing.T) {
	path := ServePath(t.TempDir())
	big := Status{Kind: KindServe, Worker: "cloud-1a2b3c", Pool: "box-strict"}
	for index := range RecentKept {
		big.AddRecent(Recent{Run: "run-" + strings.Repeat("r", 100), Unit: "unit-" + strings.Repeat("u", 100), Verdict: "passed", Seconds: float64(index)})
	}
	if err := Write(path, big); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		for index := range 1000 {
			big.Totals.Units = index
			if err := Write(path, big); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	done := make(chan struct{})
	go func() { group.Wait(); close(done) }()
	reads := 0
	for {
		select {
		case <-done:
			entries, _ := os.ReadDir(filepath.Dir(path))
			if len(entries) != 1 {
				t.Fatalf("beside the status: %v", entries)
			}
			if reads == 0 {
				t.Fatal("no read raced the writes")
			}
			return
		default:
		}
		if _, err := Read(path); err != nil {
			t.Fatalf("read %d caught a write: %v", reads, err)
		}
		reads++
	}
}

// Whatever a status is fed, its file stays under MaximumBytes: the lists cut to their lengths, the texts to theirs.
// Mutant: nothing bounded, so the file is refused as too large and the status is never written.
func TestAStatusIsBounded(t *testing.T) {
	path := UnitPath(t.TempDir())
	long := strings.Repeat("x", 10_000)
	status := Status{Kind: KindServe, Worker: long, Unfit: long, Unit: &Unit{Run: long, Unit: long, Package: long, Phase: PhaseTesting}, Tree: &Tree{Key: long, Phase: long}}
	for index := range 500 {
		status.Recent = append(status.Recent, Recent{Run: long, Unit: long, Verdict: "passed", Seconds: float64(index)})
		status.Unit.Fetches = append(status.Unit.Fetches, Fetch{What: long + string(rune('a'+index%26)), From: FromStore, Count: 1, Bytes: 1})
	}
	if err := Write(path, status); err != nil {
		t.Fatalf("a big status wasn't written: %v", err)
	}
	info, _ := os.Stat(path)
	read, err := Read(path)
	switch {
	case err != nil:
		t.Fatal(err)
	case info.Size() > MaximumBytes:
		t.Fatalf("%d bytes", info.Size())
	case len(read.Recent) != RecentKept || len(read.Unit.Fetches) != FetchesKept:
		t.Fatalf("%d recent, %d fetches", len(read.Recent), len(read.Unit.Fetches))
	case len([]rune(read.Worker)) != MaximumText || len([]rune(read.Unit.Unit)) != MaximumText || len([]rune(read.Tree.Key)) != MaximumText:
		t.Fatalf("texts of %d, %d, %d runes", len([]rune(read.Worker)), len([]rune(read.Unit.Unit)), len([]rune(read.Tree.Key)))
	}
	// AddFetch and AddRecent hold the same bounds as they go.
	unit := Unit{}
	for index := range 40 {
		unit.AddFetch(string(rune('a'+index)), FromStore, 10)
	}
	unit.AddFetch("a", FromStore, 5)
	unit.AddFetch("a", FromCache, 7)
	if len(unit.Fetches) != FetchesKept || unit.Fetches[0] != (Fetch{What: "a", From: FromStore, Count: 2, Bytes: 15}) || unit.Fetched() != FetchesKept*10+5 {
		t.Fatalf("fetches %v, fetched %d", unit.Fetches, unit.Fetched())
	}
	recent := Status{}
	for index := range 40 {
		recent.AddRecent(Recent{Unit: string(rune('a' + index))})
	}
	if len(recent.Recent) != RecentKept || recent.Recent[0].Unit != string(rune('a'+39)) {
		t.Fatalf("recent %v", recent.Recent)
	}
}

// A recordedWrite is one write a test's writer made: when, and the totals it wrote.
type recordedWrite struct {
	at    time.Time
	units int
}

// recorder writes each status to its file and records it; with a gate, each write waits for the gate first, a stalled
// disk.
type recorder struct {
	mutex  sync.Mutex
	writes []recordedWrite
	gate   chan struct{}
}

func (recorder *recorder) write(path string, status Status) error {
	if recorder.gate != nil {
		<-recorder.gate
	}
	recorder.mutex.Lock()
	recorder.writes = append(recorder.writes, recordedWrite{at: time.Now(), units: status.Totals.Units})
	recorder.mutex.Unlock()
	return Write(path, status)
}

func (recorder *recorder) seen() []recordedWrite {
	recorder.mutex.Lock()
	defer recorder.mutex.Unlock()
	return append([]recordedWrite(nil), recorder.writes...)
}

// A writer writes at most once per its spacing (a second, here 200 ms): updates within it wait, all of them landing in
// the next write; Close writes the last at once and nothing is written after. Mutants: every update written at once;
// the put-off write never made; Close losing the last update.
func TestAWriterWritesAtMostOnceASecond(t *testing.T) {
	path := ServePath(t.TempDir())
	const spacing = 200 * time.Millisecond
	recorder := &recorder{}
	writer := newWriter(path, Status{Kind: KindServe, Worker: "home-000001"}, recorder.write, spacing, time.Second)
	for units := 1; units <= 10; units++ {
		writer.Update(func(status *Status) { status.Totals.Units = units })
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(3 * spacing)
	writes := recorder.seen()
	if len(writes) < 2 || len(writes) > 3 || writes[len(writes)-1].units != 10 {
		t.Fatalf("%d writes for 11 updates in %v, the last of %d units", len(writes), 3*spacing, writes[len(writes)-1].units)
	}
	for index := 1; index < len(writes); index++ {
		if gap := writes[index].at.Sub(writes[index-1].at); gap < spacing-10*time.Millisecond {
			t.Fatalf("writes %d and %d %v apart", index-1, index, gap)
		}
	}
	if status, err := Read(path); err != nil || status.Worker != "home-000001" || status.Pid != os.Getpid() || status.Totals.Units != 10 {
		t.Fatalf("on disk: %+v %v", status, err)
	}
	// 11 is written at once, its spacing passed; 12 comes within the next spacing, so only Close writes it.
	writer.Update(func(status *Status) { status.Totals.Units = 11 })
	time.Sleep(20 * time.Millisecond)
	writer.Update(func(status *Status) { status.Totals.Units = 12 })
	started := time.Now()
	if err := writer.Close(); err != nil || time.Since(started) > spacing/2 {
		t.Fatalf("Close: %v after %v", err, time.Since(started))
	}
	if status, _ := Read(path); status.Totals.Units != 12 {
		t.Fatalf("closed with %d units on disk", status.Totals.Units)
	}
	count := len(recorder.seen())
	writer.Update(func(status *Status) { status.Totals.Units = 13 })
	time.Sleep(2 * spacing)
	if status, _ := Read(path); status.Totals.Units != 12 || len(recorder.seen()) != count || writer.Close() != nil {
		t.Fatalf("after Close: %d units, %d writes", status.Totals.Units, len(recorder.seen()))
	}
	// A nil writer takes every call.
	var none *Writer
	none.Update(func(status *Status) { status.Totals.Units = 9 })
	if none.Close() != nil || none.Status().Kind != "" {
		t.Fatal("a nil writer")
	}
}

// A stalled disk never holds an update: a hundred updates return while the write is stuck, the next write carries
// the last of them, and Close gives up waiting after its bound, saying so. Mutant: the status written under the lock.
func TestAStalledDiskNeverHoldsAnUpdate(t *testing.T) {
	path := UnitPath(t.TempDir())
	recorder := &recorder{gate: make(chan struct{})}
	writer := newWriter(path, Status{Kind: KindUnit, Unit: &Unit{Run: "r", Unit: "u"}}, recorder.write, 10*time.Millisecond, 100*time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	updated := make(chan struct{})
	go func() {
		for units := 1; units <= 100; units++ {
			writer.Update(func(status *Status) { status.Totals.Units = units })
		}
		close(updated)
	}()
	select {
	case <-updated:
	case <-time.After(2 * time.Second):
		close(recorder.gate)
		t.Fatal("updates waited on a stalled write")
	}
	started := time.Now()
	if err := writer.Close(); err == nil || !strings.Contains(err.Error(), "still running") || time.Since(started) > time.Second {
		t.Fatalf("Close on a stalled disk: %v after %v", err, time.Since(started))
	}
	close(recorder.gate)
	time.Sleep(100 * time.Millisecond)
	if status, err := Read(path); err != nil || status.Totals.Units != 100 {
		t.Fatalf("once the disk came back: %+v %v", status, err)
	}
}

// A new writer removes a crashed write's temporary file older than a minute beside its status, and leaves a younger
// one, which may be another's write in flight, and another status's. Mutant: no sweep.
func TestANewWriterSweepsStalePartials(t *testing.T) {
	path := ServePath(t.TempDir())
	os.MkdirAll(filepath.Dir(path), 0o755)
	stale := filepath.Join(filepath.Dir(path), ".partial-serve.json-111")
	young := filepath.Join(filepath.Dir(path), ".partial-serve.json-222")
	other := filepath.Join(filepath.Dir(path), ".partial-unit.json-333")
	for _, partial := range []string{stale, young, other} {
		os.WriteFile(partial, []byte(`{"kind":`), 0o644)
	}
	old := time.Now().Add(-2 * time.Minute)
	os.Chtimes(stale, old, old)
	os.Chtimes(other, old, old)
	NewWriter(path, Status{Kind: KindServe}).Close()
	for partial, kept := range map[string]bool{stale: false, young: true, other: true} {
		if _, err := os.Stat(partial); (err == nil) != kept {
			t.Errorf("%s: kept %v, want %v", filepath.Base(partial), err == nil, kept)
		}
	}
}

// Read takes a missing file as fs.ErrNotExist, and one cut short, not a status, naming no kind or past the bound as
// an error, never a status. Mutants: a cut-short file read as an empty status; no bound on the read.
func TestReadIsTolerant(t *testing.T) {
	directory := t.TempDir()
	if _, err := Read(filepath.Join(directory, "none.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing file: %v", err)
	}
	path := filepath.Join(directory, "serve.json")
	if err := Write(path, Status{Kind: KindServe, Worker: "w", Totals: Totals{Units: 3}}); err != nil {
		t.Fatal(err)
	}
	whole, _ := os.ReadFile(path)
	for name, content := range map[string]string{
		"cut short": string(whole[:len(whole)/2]),
		"empty":     "",
		"not json":  "serve: busy\n",
		"no kind":   `{"worker":"w"}`,
		"too large": `{"kind":"serve","worker":"` + strings.Repeat("w", MaximumBytes) + `"}`,
	} {
		os.WriteFile(path, []byte(content), 0o644)
		if status, err := Read(path); err == nil {
			t.Errorf("%s read as %+v", name, status)
		} else if name == "too large" && !errors.Is(err, ErrTooLarge) {
			t.Errorf("too large: %v", err)
		}
	}
	os.WriteFile(path, whole, 0o644)
	if status, err := Read(path); err != nil || status.Totals.Units != 3 {
		t.Fatalf("the whole file: %+v %v", status, err)
	}
	if !Alive(os.Getpid()) || Alive(0) {
		t.Fatal("Alive")
	}
}
