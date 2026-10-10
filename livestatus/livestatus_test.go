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

// A writer writes at most once a second: an update within the second waits for it, every update in between lands in
// that one write, and Close writes the last at once and nothing after. Mutants: every update written at once; the
// put-off write never made; Close losing the last update.
func TestAWriterWritesAtMostOnceASecond(t *testing.T) {
	path := ServePath(t.TempDir())
	clock := time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC)
	var scheduled []func()
	writer := &Writer{path: path, now: func() time.Time { return clock }, after: func(delay time.Duration, write func()) *time.Timer {
		if delay <= 0 || delay > MinimumSpacing {
			t.Errorf("a write put off by %v", delay)
		}
		scheduled = append(scheduled, write)
		return time.NewTimer(time.Hour)
	}}
	writer.start(Status{Kind: KindServe, Worker: "home-000001"})
	units := func() int {
		status, err := Read(path)
		if err != nil {
			t.Fatal(err)
		}
		return status.Totals.Units
	}
	if status, _ := Read(path); status.Worker != "home-000001" || status.Pid != os.Getpid() {
		t.Fatalf("the first status: %+v", status)
	}
	clock = clock.Add(300 * time.Millisecond)
	writer.Update(func(status *Status) { status.Totals.Units = 1 })
	writer.Update(func(status *Status) { status.Totals.Units = 2 })
	if units() != 0 || len(scheduled) != 1 {
		t.Fatalf("within the second: %d units on disk, %d writes put off", units(), len(scheduled))
	}
	clock = clock.Add(700 * time.Millisecond)
	scheduled[0]()
	if units() != 2 {
		t.Fatalf("the put-off write: %d units", units())
	}
	clock = clock.Add(2 * time.Second)
	writer.Update(func(status *Status) { status.Totals.Units = 3 })
	if units() != 3 || len(scheduled) != 1 {
		t.Fatalf("a second later: %d units, %d writes put off", units(), len(scheduled))
	}
	clock = clock.Add(100 * time.Millisecond)
	writer.Update(func(status *Status) { status.Totals.Units = 4 })
	if err := writer.Close(); err != nil || units() != 4 {
		t.Fatalf("closed (%v): %d units", err, units())
	}
	writer.Update(func(status *Status) { status.Totals.Units = 5 })
	scheduled[len(scheduled)-1]()
	if units() != 4 {
		t.Fatalf("after Close: %d units", units())
	}
	// A nil writer takes every call.
	var none *Writer
	none.Update(func(status *Status) { status.Totals.Units = 9 })
	if none.Close() != nil || none.Status().Kind != "" {
		t.Fatal("a nil writer")
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
