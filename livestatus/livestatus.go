// Package livestatus is what a Loom process on a box is doing right now, in a small file `loom top` reads: serve's
// unit in hand, its totals and its recent units (ServePath), the runner's view of that unit (UnitPath: its phase, what
// it fetched and from where, its tests so far), and Workshop's tree build in progress (TreePath). A Writer replaces
// its file whole (a temporary file renamed over it) from a goroutine of its own, never more than once a second but for
// the one last write as it closes, and keeps it under MaximumBytes; Read takes a missing, cut short or oversized file
// as an error, never a crash. Nothing here is a record: a status lives only as long as the process writing it, which is why it carries its
// pid, and nothing reads it but a person's terminal.
package livestatus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Directory is the status files' directory under a serve root or a tree builder's cache.
const Directory = "loom-live"

// ServePath is serve's status under its root; UnitPath the runner's, for the unit it runs there; TreePath the tree
// builder's under its cache.
func ServePath(root string) string { return filepath.Join(root, Directory, "serve.json") }
func UnitPath(root string) string  { return filepath.Join(root, Directory, "unit.json") }
func TreePath(cache string) string { return filepath.Join(cache, Directory, "tree.json") }

// The bounds that keep a status small whatever it is fed: a file past MaximumBytes is never written nor read.
const (
	MaximumBytes   = 64 << 10
	RecentKept     = 12
	FetchesKept    = 16
	MaximumText    = 160
	MinimumSpacing = time.Second
)

// The kinds of status.
const (
	KindServe = "serve"
	KindUnit  = "unit"
	KindTree  = "tree"
)

// A unit's phases, as serve and the runner name them.
const (
	PhaseStarting       = "starting"
	PhaseFetchingRunner = "fetching runner"
	PhaseOnRunner       = "on runner"
	PhaseFetching       = "fetching"
	PhaseUnpacking      = "unpacking"
	PhasePreparing      = "preparing"
	PhaseTesting        = "testing"
	PhaseFinished       = "finished"
)

// Where a fetched blob came from.
const (
	FromStore = "store"
	FromCache = "cache"
)

// A Status is one process's live state. Kind says which of the rest it fills.
type Status struct {
	Kind      string    `json:"kind"`
	Pid       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// serve's
	Worker  string    `json:"worker,omitempty"`
	Pool    string    `json:"pool,omitempty"`
	AskedAt time.Time `json:"askedAt,omitzero"`
	Unfit   string    `json:"unfit,omitempty"`
	Stopped string    `json:"stopped,omitempty"`
	Totals  Totals    `json:"totals,omitzero"`
	Recent  []Recent  `json:"recent,omitempty"`
	// serve's and the runner's: the unit in hand, nil when none.
	Unit *Unit `json:"unit,omitempty"`
	// the tree builder's
	Tree *Tree `json:"tree,omitempty"`
}

// A Unit is the unit in hand.
type Unit struct {
	Run       string    `json:"run"`
	Unit      string    `json:"unit"`
	Package   string    `json:"package,omitempty"`
	Packages  int       `json:"packages,omitempty"`
	Shard     string    `json:"shard,omitempty"`
	Phase     string    `json:"phase"`
	Runner    string    `json:"runner,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	Deadline  time.Time `json:"deadline,omitzero"`
	Fetches   []Fetch   `json:"fetches,omitempty"`
	Tests     Tests     `json:"tests,omitzero"`
	Verdict   string    `json:"verdict,omitempty"`
}

// A Fetch is every blob of one kind had from one place: chunks, products, binaries, modules or the index, from the
// store or the cache.
type Fetch struct {
	What  string `json:"what"`
	From  string `json:"from"`
	Count int    `json:"count"`
	Bytes int64  `json:"bytes"`
}

// Tests are the unit's tests so far, top-level tests only, as go test -json reports them.
type Tests struct {
	Passed  int `json:"passed,omitempty"`
	Failed  int `json:"failed,omitempty"`
	Skipped int `json:"skipped,omitempty"`
}

// Totals are everything serve ran since it started.
type Totals struct {
	Units   int   `json:"units,omitempty"`
	Passed  int   `json:"passed,omitempty"`
	Failed  int   `json:"failed,omitempty"`
	Broken  int   `json:"broken,omitempty"`
	Fetched int64 `json:"fetched,omitempty"`
}

// A Recent is a unit serve finished: its verdict, when, how long it took and the bytes it fetched from the store.
type Recent struct {
	Run        string    `json:"run"`
	Unit       string    `json:"unit"`
	Verdict    string    `json:"verdict"`
	FinishedAt time.Time `json:"finishedAt"`
	Seconds    float64   `json:"seconds"`
	Fetched    int64     `json:"fetched,omitempty"`
}

// A Tree is the tree build in progress: its key and future, its phase, and its products built against those the
// store already held (hit).
type Tree struct {
	Key          string    `json:"key,omitempty"`
	Future       string    `json:"future,omitempty"`
	Phase        string    `json:"phase"`
	StartedAt    time.Time `json:"startedAt"`
	Packages     int       `json:"packages,omitempty"`
	ProductTests int       `json:"productTests,omitempty"`
	Products     int       `json:"products,omitempty"`
	ProductsHit  int       `json:"productsHit,omitempty"`
	Failed       int       `json:"failed,omitempty"`
}

// Fetched is the bytes the unit had from the store, not the cache.
func (unit *Unit) Fetched() int64 {
	var total int64
	for _, fetch := range unit.Fetches {
		if fetch.From != FromCache {
			total += fetch.Bytes
		}
	}
	return total
}

// AddFetch counts one blob of a kind from a place, keeping at most FetchesKept kinds and places.
func (unit *Unit) AddFetch(what, from string, bytes int64) {
	for index := range unit.Fetches {
		if unit.Fetches[index].What == what && unit.Fetches[index].From == from {
			unit.Fetches[index].Count++
			unit.Fetches[index].Bytes += bytes
			return
		}
	}
	if len(unit.Fetches) < FetchesKept {
		unit.Fetches = append(unit.Fetches, Fetch{What: what, From: from, Count: 1, Bytes: bytes})
	}
}

// AddRecent puts a finished unit first, keeping RecentKept.
func (status *Status) AddRecent(recent Recent) {
	status.Recent = append([]Recent{recent}, status.Recent...)
	if len(status.Recent) > RecentKept {
		status.Recent = status.Recent[:RecentKept]
	}
}

// bounded is the status cut to its bounds: the lists to their lengths, every text to MaximumText runes.
func bounded(status Status) Status {
	cut := func(text string) string {
		runes := []rune(text)
		if len(runes) <= MaximumText {
			return text
		}
		return string(runes[:MaximumText-1]) + "…"
	}
	status.Worker, status.Pool, status.Unfit, status.Stopped = cut(status.Worker), cut(status.Pool), cut(status.Unfit), cut(status.Stopped)
	if len(status.Recent) > RecentKept {
		status.Recent = status.Recent[:RecentKept]
	}
	recent := make([]Recent, len(status.Recent))
	for index, entry := range status.Recent {
		entry.Run, entry.Unit, entry.Verdict = cut(entry.Run), cut(entry.Unit), cut(entry.Verdict)
		recent[index] = entry
	}
	status.Recent = recent
	if status.Unit != nil {
		unit := *status.Unit
		unit.Run, unit.Unit, unit.Package, unit.Shard, unit.Phase, unit.Runner, unit.Verdict =
			cut(unit.Run), cut(unit.Unit), cut(unit.Package), cut(unit.Shard), cut(unit.Phase), cut(unit.Runner), cut(unit.Verdict)
		fetches := unit.Fetches
		if len(fetches) > FetchesKept {
			fetches = fetches[:FetchesKept]
		}
		unit.Fetches = make([]Fetch, len(fetches))
		for index, fetch := range fetches {
			fetch.What, fetch.From = cut(fetch.What), cut(fetch.From)
			unit.Fetches[index] = fetch
		}
		status.Unit = &unit
	}
	if status.Tree != nil {
		tree := *status.Tree
		tree.Key, tree.Future, tree.Phase = cut(tree.Key), cut(tree.Future), cut(tree.Phase)
		status.Tree = &tree
	}
	return status
}

// A Writer keeps one status file. Update only changes the status in memory and wakes the Writer's own goroutine, which
// copies the status under the lock and writes it outside it, at most once a second: a stalled disk stalls that
// goroutine alone, never a unit's tests or serve. A nil Writer takes every call and writes nothing, so a process with
// no status file needs no branch.
type Writer struct {
	path     string
	mutex    sync.Mutex
	status   Status
	dirty    bool
	closed   bool
	wake     chan struct{}
	done     chan struct{}
	finished chan struct{}
	// err is the last write's failure, for tests and the curious; a status that can't be written never stops its process.
	err error
	// write writes one status, spacing is the least time between two writes, and closeWait the longest Close waits for
	// the last one; tests change them.
	write     func(path string, status Status) error
	spacing   time.Duration
	closeWait time.Duration
}

// CloseWait is the longest Close waits for the last write, so a stalled disk never holds a unit's end.
const CloseWait = 5 * time.Second

// staleAfter is how old a temporary file beside a status must be before a new Writer takes it for a crashed write's.
const staleAfter = time.Minute

// NewWriter starts a status file at path with status, stamped with this process's pid and the time, and writes it as
// soon as its goroutine can. A crashed writer's temporary files left beside it go.
func NewWriter(path string, status Status) *Writer {
	return newWriter(path, status, Write, MinimumSpacing, CloseWait)
}

func newWriter(path string, status Status, write func(path string, status Status) error, spacing, closeWait time.Duration) *Writer {
	status.Pid = os.Getpid()
	if status.StartedAt.IsZero() {
		status.StartedAt = time.Now()
	}
	sweepPartials(path, time.Now())
	writer := &Writer{path: path, status: status, dirty: true, wake: make(chan struct{}, 1), done: make(chan struct{}),
		finished: make(chan struct{}), write: write, spacing: spacing, closeWait: closeWait}
	go writer.loop()
	writer.signal()
	return writer
}

// sweepPartials removes the temporary files of path's status older than staleAfter: a write killed between its create
// and its rename leaves one, and nothing else ever would. A younger one may be another writer's in flight.
func sweepPartials(path string, now time.Time) {
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".partial-"+filepath.Base(path)+"-*"))
	for _, match := range matches {
		if info, err := os.Lstat(match); err == nil && info.Mode().IsRegular() && now.Sub(info.ModTime()) > staleAfter {
			os.Remove(match)
		}
	}
}

// loop is the Writer's goroutine: each wake writes the status, once the last write is spacing old; Close's done writes
// the last at once and ends it.
func (writer *Writer) loop() {
	defer close(writer.finished)
	var written time.Time
	for closing := false; !closing; {
		select {
		case <-writer.wake:
		case <-writer.done:
			closing = true
		}
		if wait := writer.spacing - time.Since(written); !closing && !written.IsZero() && wait > 0 {
			timer := time.NewTimer(wait)
			select {
			case <-timer.C:
			case <-writer.done:
				closing = true
			}
			timer.Stop()
		}
		if writer.flush() {
			written = time.Now()
		}
	}
}

// flush writes the status if it changed since the last write: copied under the lock, written outside it.
func (writer *Writer) flush() bool {
	writer.mutex.Lock()
	if !writer.dirty {
		writer.mutex.Unlock()
		return false
	}
	writer.status.UpdatedAt = time.Now()
	status := copyStatus(writer.status)
	writer.dirty = false
	writer.mutex.Unlock()
	err := writer.write(writer.path, status)
	writer.mutex.Lock()
	writer.err = err
	writer.mutex.Unlock()
	return true
}

func (writer *Writer) signal() {
	select {
	case writer.wake <- struct{}{}:
	default:
	}
}

// Update changes the status in memory and wakes the goroutine that writes it; it never waits on the disk.
func (writer *Writer) Update(change func(status *Status)) {
	if writer == nil {
		return
	}
	writer.mutex.Lock()
	if writer.closed {
		writer.mutex.Unlock()
		return
	}
	change(&writer.status)
	writer.dirty = true
	writer.mutex.Unlock()
	writer.signal()
}

// Close writes the last status at once, if it changed since the last write, and nothing after; it waits for that write
// no longer than closeWait.
func (writer *Writer) Close() error {
	if writer == nil {
		return nil
	}
	writer.mutex.Lock()
	if writer.closed {
		defer writer.mutex.Unlock()
		return writer.err
	}
	writer.closed = true
	writer.mutex.Unlock()
	close(writer.done)
	timer := time.NewTimer(writer.closeWait)
	defer timer.Stop()
	select {
	case <-writer.finished:
	case <-timer.C:
		return fmt.Errorf("the last write of %s is still running after %v", writer.path, writer.closeWait)
	}
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return writer.err
}

// Status is a copy of the status as it stands.
func (writer *Writer) Status() Status {
	if writer == nil {
		return Status{}
	}
	writer.mutex.Lock()
	defer writer.mutex.Unlock()
	return copyStatus(writer.status)
}

// copyStatus is a status sharing nothing a later Update changes.
func copyStatus(status Status) Status {
	if status.Unit != nil {
		unit := *status.Unit
		unit.Fetches = append([]Fetch(nil), unit.Fetches...)
		status.Unit = &unit
	}
	if status.Tree != nil {
		tree := *status.Tree
		status.Tree = &tree
	}
	status.Recent = append([]Recent(nil), status.Recent...)
	return status
}

// Write replaces the file at path with the status, bounded: a temporary file beside it, renamed over it, so a reader
// sees the old status or the new one, never part of one.
func Write(path string, status Status) error {
	content, err := json.Marshal(bounded(status))
	if err != nil {
		return err
	}
	if len(content) > MaximumBytes {
		return fmt.Errorf("the status is %d bytes, over %d", len(content), MaximumBytes)
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	partial, err := os.CreateTemp(directory, ".partial-"+filepath.Base(path)+"-")
	if err != nil {
		return err
	}
	defer os.Remove(partial.Name())
	_, err = partial.Write(append(content, '\n'))
	if closeErr := partial.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Chmod(partial.Name(), 0o644)
	}
	if err != nil {
		return err
	}
	return os.Rename(partial.Name(), path)
}

// ErrTooLarge is a status file past MaximumBytes, which no Writer writes.
var ErrTooLarge = errors.New("the status file is too large to be one")

// Read reads the status at path. A missing file is an error wrapping fs.ErrNotExist; one cut short, not a status, or
// past MaximumBytes is an error too.
func Read(path string) (Status, error) {
	file, err := os.Open(path)
	if err != nil {
		return Status{}, err
	}
	defer file.Close()
	content, err := io.ReadAll(io.LimitReader(file, MaximumBytes+1))
	if err != nil {
		return Status{}, err
	}
	if len(content) > MaximumBytes {
		return Status{}, fmt.Errorf("%s: %w", path, ErrTooLarge)
	}
	var status Status
	if err := json.Unmarshal(content, &status); err != nil {
		return Status{}, fmt.Errorf("%s isn't a status: %w", path, err)
	}
	if status.Kind == "" {
		return Status{}, fmt.Errorf("%s names no kind", path)
	}
	return status, nil
}

// Alive says whether the process that wrote a status still runs: a signal 0 reaches it, or it exists under another
// user.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
