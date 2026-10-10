// Package treebuilder is Workshop's tree builder (`loom build-trees`, #w7agfa9): beside the placer, it builds the tree
// of every future Queue lists planned whose build the action store lacks, so the placer can name a build on each test
// unit and a runner only fetches and runs. The tree key comes from the plan (judge.PlannedUnitWire.Tree, which the
// planner read where the tree was); the build is `loom build-tree` on the future's commit checked out keyless into the
// builder's own clone, under build-tree's floors, admission and bounded caches, one tree at a time.
//
// Every build is in the ledger before it starts and when it ends, so the placer can act on it: built (the index is
// up; a package that didn't compile for the change's reasons is the change's red through the index, one that failed for
// Workshop's is broken), failed (no index: Workshop's, void, named, and built again after RetryAfter), refused (the
// disk is under its floor: nothing is checked out or built until it isn't), or interrupted (the builder stopped
// mid-build, a crash, and builds it again), or stopped (the builder was told to stop, a restart or a deploy, and the
// next one builds it again).
package treebuilder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/system-inc/loom/jsonlines"
	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
)

// The events a Record holds.
const (
	Started     = "started"
	Built       = "built"
	Failed      = "failed"
	Refused     = "refused"
	Interrupted = "interrupted"
	Stopped     = "stopped"
)

// ErrStopped is a build ended because the builder itself was told to stop (SIGTERM: systemd's stop, every update's
// restart). The build isn't the tree's failure: it's recorded Stopped, and the next builder builds it again.
var ErrStopped = errors.New("the builder was stopped mid-build")

// RetryAfter is how long a tree whose build failed stands failed: the builder builds it again after it, and the placer
// reads the failure as the tree's only while it's younger, holding a later attempt while the next build runs.
const RetryAfter = 30 * time.Minute

// A Record is one ledger line: what the builder did with one tree, by its key.
type Record struct {
	Tree    string  `json:"tree"`
	Future  string  `json:"future"`
	At      string  `json:"at"`
	Event   string  `json:"event"`
	Cause   string  `json:"cause,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

// at is when the record was written; an unreadable time is the zero time, as old as can be.
func (record Record) at() time.Time {
	at, _ := time.Parse(time.RFC3339, record.At)
	return at
}

// Standing says whether a failed record still stands at now: younger than RetryAfter, before the builder tries again.
func (record Record) Standing(now time.Time) bool {
	return record.Event == Failed && now.Sub(record.at()) < RetryAfter
}

// String is the record as a void's cause names it.
func (record Record) String() string {
	text := fmt.Sprintf("%s at %s", record.Event, record.At)
	if record.Cause != "" {
		text += ": " + record.Cause
	}
	return text
}

// A Ledger is the builder's ledger file, each tree's newest record held.
type Ledger struct {
	file   *jsonlines.File[Record]
	newest map[string]Record
	order  []Record
}

// OpenLedger takes the ledger at path for this process's life, refused while another builder holds it. A tree whose
// newest record is Started was being built when the last builder stopped, so it's recorded Interrupted, to be built
// again.
func OpenLedger(path string, now time.Time) (*Ledger, error) {
	file, records, err := jsonlines.Open[Record](path, "another tree builder holds it, and two would build one tree twice")
	if err != nil {
		return nil, err
	}
	ledger := &Ledger{file: file, newest: map[string]Record{}}
	for _, record := range records {
		ledger.hold(record)
	}
	for _, record := range records {
		if newest := ledger.newest[record.Tree]; newest == record && record.Event == Started {
			interrupted := Record{Tree: record.Tree, Future: record.Future, At: now.UTC().Format(time.RFC3339), Event: Interrupted,
				Cause: "the builder stopped mid-build (a restart or a crash), and builds it again"}
			if err := ledger.Append(interrupted); err != nil {
				file.Close()
				return nil, err
			}
		}
	}
	return ledger, nil
}

func (ledger *Ledger) hold(record Record) {
	ledger.newest[record.Tree] = record
	ledger.order = append(ledger.order, record)
}

// Newest is the tree's newest record.
func (ledger *Ledger) Newest(tree string) (Record, bool) {
	record, found := ledger.newest[tree]
	return record, found
}

// Append writes the record and syncs it, then holds it.
func (ledger *Ledger) Append(record Record) error {
	if err := ledger.file.Append(record); err != nil {
		return err
	}
	ledger.hold(record)
	return nil
}

// Compact keeps only the records keep says to, each tree's newest always among them while keep keeps it.
func (ledger *Ledger) Compact(keep func(Record) bool) error {
	kept := []Record{}
	for _, record := range ledger.order {
		if keep(record) {
			kept = append(kept, record)
		}
	}
	if err := ledger.file.Rewrite(kept); err != nil {
		return err
	}
	ledger.order, ledger.newest = nil, map[string]Record{}
	for _, record := range kept {
		ledger.hold(record)
	}
	return nil
}

// Close gives the ledger up.
func (ledger *Ledger) Close() error {
	return ledger.file.Close()
}

// Newest reads the tree's newest record from the ledger at path without taking it, as the placer does while the
// builder holds it.
func Newest(path, tree string) (Record, bool, error) {
	records, err := jsonlines.Read[Record](path)
	if err != nil {
		return Record{}, false, err
	}
	var newest Record
	found := false
	for _, record := range records {
		if record.Tree == tree {
			newest, found = record, true
		}
	}
	return newest, found, nil
}

// A Want is one tree the listed futures need built: its key and the first future listed that runs it.
type Want struct {
	Tree   string
	Future string
}

// Wanted lists the trees the listed futures run, in listing order, each once: the tree key a future's test and product
// units carry when one of them runs (not reused). A future whose units carry two keys, or a running one none, is
// named, never built: its plan can't say which build its units run.
func Wanted(futures []judge.PlannedFuture) ([]Want, []string) {
	wants, problems, seen := []Want{}, []string{}, map[string]bool{}
	for _, future := range futures {
		keys, running, keyless := map[string]bool{}, false, 0
		for _, unit := range future.Units {
			var parts planner.KeyParts
			if json.Unmarshal(unit.KeyParts, &parts) != nil || !planner.RunsTreeBuild(parts.Kind) {
				continue
			}
			if unit.Tree == "" {
				if unit.Decision == "run" {
					keyless++
				}
				continue
			}
			keys[unit.Tree] = true
			running = running || unit.Decision == "run"
		}
		switch {
		case keyless > 0:
			problems = append(problems, fmt.Sprintf("future %s: %d running units carry no tree key", future.Future, keyless))
		case len(keys) > 1:
			problems = append(problems, fmt.Sprintf("future %s: its units carry %d tree keys", future.Future, len(keys)))
		case running:
			for key := range keys {
				if !seen[key] {
					seen[key] = true
					wants = append(wants, Want{Tree: key, Future: future.Future})
				}
			}
		}
	}
	return wants, problems
}

// A Builder builds the listed futures' trees the store lacks, one at a time.
type Builder struct {
	Source judge.FutureSource // Queue's planned listing (judge.HTTPFutures)
	// Indexed says whether the action store holds trees/<tree>.json.
	Indexed func(tree string) (bool, error)
	// Floor refuses while a filesystem a build writes is under its floor (builder.CheckFloor over build-tree's watches
	// and the clone's): nothing is checked out or built until it has room.
	Floor func() error
	// Build checks the future's commit out keyless in the builder's clone and runs `loom build-tree` on it with the
	// plan's tree key, which refuses a tree keying otherwise. Its error is why the build ended badly; whether the tree
	// was built is read from the store after it, never from its exit.
	Build  func(want Want) error
	Ledger *Ledger
	// Keep is how long a record is kept; zero keeps every one.
	Keep        time.Duration
	Now         func() time.Time
	Log         io.Writer
	noted       map[string]bool
	compactedAt time.Time
}

func (builder *Builder) note(line string) {
	if builder.noted == nil {
		builder.noted = map[string]bool{}
	}
	if !builder.noted[line] {
		builder.noted[line] = true
		fmt.Fprintln(builder.Log, line)
	}
}

// BuildOnce builds the first listed tree the store lacks whose last build doesn't stand failed, and says whether it
// ran a build (well or badly). A tree another build put up meanwhile is left; one refused under the floor waits.
func (builder *Builder) BuildOnce() (bool, error) {
	futures, err := builder.Source.Planned()
	if err != nil {
		return false, err
	}
	wants, problems := Wanted(futures)
	for _, problem := range problems {
		builder.note(problem + ": not built")
	}
	if err := builder.compact(); err != nil {
		fmt.Fprintf(builder.Log, "compacting the ledger: %v\n", err)
	}
	for _, want := range wants {
		newest, found := builder.Ledger.Newest(want.Tree)
		if found && newest.Standing(builder.Now()) {
			continue
		}
		indexed, err := builder.Indexed(want.Tree)
		if err != nil {
			return false, fmt.Errorf("tree %s: %w", want.Tree, err)
		}
		if indexed {
			continue
		}
		// Under the floor nothing is checked out or built, this tree or any other, until the disk has room: refused
		// once in the ledger, so the placer can say why a tree it waits on isn't coming.
		if err := builder.Floor(); err != nil {
			if newest.Event == Refused {
				return false, nil
			}
			fmt.Fprintf(builder.Log, "tree %s of %s: refused, %v\n", want.Tree, want.Future, err)
			return false, builder.Ledger.Append(Record{Tree: want.Tree, Future: want.Future, At: builder.Now().UTC().Format(time.RFC3339), Event: Refused, Cause: err.Error()})
		}
		return true, builder.build(want)
	}
	return false, nil
}

// build builds one tree, recorded before it starts and when it ends.
func (builder *Builder) build(want Want) error {
	record := Record{Tree: want.Tree, Future: want.Future, At: builder.Now().UTC().Format(time.RFC3339), Event: Started}
	if err := builder.Ledger.Append(record); err != nil {
		return err
	}
	fmt.Fprintf(builder.Log, "tree %s of %s: building\n", want.Tree, want.Future)
	started := builder.Now()
	buildErr := builder.Build(want)
	indexed, indexErr := builder.Indexed(want.Tree)
	record.At, record.Seconds = builder.Now().UTC().Format(time.RFC3339), builder.Now().Sub(started).Seconds()
	switch {
	case errors.Is(buildErr, ErrStopped) && !(indexErr == nil && indexed):
		// A stop is the builder's, never the tree's: nothing stands failed, and the next builder builds it again.
		record.Event, record.Cause = Stopped, buildErr.Error()
	case indexErr == nil && indexed:
		// Packages that failed are the index's to say, each the change's red or Workshop's broken.
		record.Event = Built
		if buildErr != nil {
			record.Cause = buildErr.Error()
		}
	case indexErr != nil:
		record.Event, record.Cause = Failed, fmt.Sprintf("reading the store for its index after the build: %v", indexErr)
	case buildErr != nil:
		record.Event, record.Cause = Failed, buildErr.Error()
	default:
		record.Event, record.Cause = Failed, "build-tree ended well, and trees/"+want.Tree+".json isn't in the store"
	}
	fmt.Fprintf(builder.Log, "tree %s of %s: %s\n", want.Tree, want.Future, record)
	return builder.Ledger.Append(record)
}

// compact drops, at most hourly, the records over Keep old.
func (builder *Builder) compact() error {
	now := builder.Now()
	if builder.Keep <= 0 || now.Sub(builder.compactedAt) < time.Hour {
		return nil
	}
	builder.compactedAt = now
	return builder.Ledger.Compact(func(record Record) bool {
		return now.Sub(record.at()) < builder.Keep
	})
}
