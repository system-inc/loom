// Package treebuilder is Workshop's tree builder (`loom build-trees`, #w7agfa9): beside the placer, it builds the tree
// of every future Queue lists planned, and every base tree Judge asks for (requests.go), whose build the action store lacks, so the placer can name a build on each test
// unit and a runner only fetches and runs. The tree key comes from the plan (judge.PlannedUnitWire.Tree, which the
// planner read where the tree was); the build is `loom build-tree` on the future's commit checked out keyless into the
// builder's own clone, under build-tree's floors, admission and bounded caches, one tree at a time.
//
// Every build is in the ledger before it starts and when it ends, so the placer can act on it: built (the index is
// up; a package that didn't compile for the change's reasons is the change's red through the index, one that failed for
// Workshop's is broken), failed (no index: Workshop's, void, named, and built again after a backoff, or never once it
// has failed MaxFailures times; a checkout that hiccuped is transient, retried soon and never voided), refused (the
// disk is under its floor: nothing is checked out or built until it isn't), or interrupted (the builder stopped
// mid-build, a crash, and builds it again), or stopped (the build ended under the builder's stop, or an adopted one
// left no index this release reads, and the next pass builds it again; or nothing wants its tree any more, a withdrawn
// branch's, and no pass builds it unless a listing wants it again: unwanted.go). A build's child runs in a process group of its
// own, recorded running with its pid: a builder told to stop (a release's restart, #apsj7zp) leaves it running, and the
// next builder adopts it, waiting for it under the same bound, instead of building the tree again.
package treebuilder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/system-inc/loom/builder"
	"github.com/system-inc/loom/jsonlines"
	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

// The events a Record holds.
const (
	Started     = "started"
	Running     = "running"
	Built       = "built"
	Failed      = "failed"
	Refused     = "refused"
	Interrupted = "interrupted"
	Stopped     = "stopped"
)

// ErrStopped is a build ended because the builder itself was told to stop before its child ran (a checkout cut short).
// The build isn't the tree's failure: it's recorded Stopped, and the next builder builds it again.
var ErrStopped = errors.New("the builder was stopped mid-build")

// ErrDetached is a builder told to stop (SIGTERM: systemd's stop, every release's restart) while its build's child
// runs: the child is left running, its running record stands, and the next builder adopts it.
var ErrDetached = errors.New("the builder stopped and left its build running, for the next builder to adopt")

// ErrTransient is a build that failed before building anything for a reason that passes (review of tree-wiring,
// finding 6): its checkout, against GitHub's 5xx or the network. It's retried after TransientRetry, doubling to
// TransientCap, and never stands: the placer keeps holding, under its own bound.
var ErrTransient = errors.New("a transient failure")

// A tree's failures back off (review of tree-wiring, finding 5): the first stands RetryAfter, each later one twice the
// last, and after MaxFailures (a crash mid-build counting as one) the builder gives the tree up, standing until its
// records are compacted away, so one bad tree can't take the builder from the others.
const (
	RetryAfter     = 30 * time.Minute
	MaxFailures    = 3
	TransientRetry = time.Minute
	TransientCap   = 10 * time.Minute
)

// A Record is one ledger line: what the builder did with one tree, by its key.
type Record struct {
	Tree    string  `json:"tree"`
	Future  string  `json:"future"`
	At      string  `json:"at"`
	Event   string  `json:"event"`
	Cause   string  `json:"cause,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
	// Retry is when the builder builds a failed tree again (RFC 3339); a failure without one is given up.
	Retry string `json:"retry,omitempty"`
	// Transient marks a failure that passes (ErrTransient): retried soon, never standing.
	Transient bool `json:"transient,omitempty"`
	// Pid is a running build's child, the leader of its process group, which a later builder adopts.
	Pid int `json:"pid,omitempty"`
	// Phases are the build's seconds by phase, its checkout's and build-tree's own, so a tree's phases outlive its
	// build (#s0cqqhk); none on a start, or when the build said none.
	Phases *builder.TreePhases `json:"phases,omitempty"`
}

// at is when the record was written; an unreadable time is the zero time, as old as can be.
func (record Record) at() time.Time {
	at, _ := time.Parse(time.RFC3339, record.At)
	return at
}

// Standing says whether a failed record stands at now, the tree's failure: not transient, and before the builder
// tries it again, or given up.
func (record Record) Standing(now time.Time) bool {
	return record.Event == Failed && !record.Transient && (record.Retry == "" || now.Before(record.retry()))
}

// due says whether the builder may build the tree at now, after this record: always, except after a failure before its
// retry, or one given up.
func (record Record) due(now time.Time) bool {
	return record.Event != Failed || (record.Retry != "" && !now.Before(record.retry()))
}

func (record Record) retry() time.Time {
	retry, _ := time.Parse(time.RFC3339, record.Retry)
	return retry
}

// String is the record as a void's cause names it.
func (record Record) String() string {
	text := fmt.Sprintf("%s at %s", record.Event, record.At)
	switch {
	case record.Event != Failed:
	case record.Transient:
		text += ", transient, tried again at " + record.Retry
	case record.Retry == "":
		text += fmt.Sprintf(", given up after %d failures", MaxFailures)
	default:
		text += ", tried again at " + record.Retry
	}
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
				Cause: "the builder stopped mid-build without being told to (a crash), and builds it again"}
			// A tree that keeps taking the builder down with it is given up like one that keeps failing.
			if ledger.failures(record.Tree)+1 >= MaxFailures {
				interrupted.Event, interrupted.Cause = Failed, fmt.Sprintf("the builder died mid-build %d times building it", MaxFailures)
			}
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

// failures counts the tree's failures since it was last built: failed builds that weren't transient, and builds a
// crash cut short.
func (ledger *Ledger) failures(tree string) int {
	count := 0
	for index := len(ledger.order) - 1; index >= 0; index-- {
		record := ledger.order[index]
		switch {
		case record.Tree != tree:
		case record.Event == Built:
			return count
		case record.Event == Interrupted, record.Event == Failed && !record.Transient:
			count++
		}
	}
	return count
}

// transients counts the tree's transient failures since anything else but a start, a running child or a refusal.
func (ledger *Ledger) transients(tree string) int {
	count := 0
	for index := len(ledger.order) - 1; index >= 0; index-- {
		record := ledger.order[index]
		switch {
		case record.Tree != tree, record.Event == Started, record.Event == Running, record.Event == Refused:
		case record.Event == Failed && record.Transient:
			count++
		default:
			return count
		}
	}
	return count
}

// running is every tree's newest record that says its build is running, in the order they were written.
func (ledger *Ledger) running() []Record {
	running := []Record{}
	for _, record := range ledger.order {
		if newest := ledger.newest[record.Tree]; newest == record && record.Event == Running {
			running = append(running, record)
		}
	}
	return running
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
	// Go is the Go release the plan's units are keyed on (keyParts.tools.go), which the planner checked is the tree
	// key's: build-tree refuses to build with another.
	Go string
}

// Wanted lists the trees the listed futures run, in listing order, each once: the tree key a future's test, product and
// phase units carry when one of them runs (not reused). A future whose units carry two keys, or a running one none, is
// named, never built: its plan can't say which build its units run.
func Wanted(futures []judge.PlannedFuture) ([]Want, []string) {
	wants, problems, seen := []Want{}, []string{}, map[string]bool{}
	for _, future := range futures {
		keys, releases, running, keyless := map[string]bool{}, map[string]bool{}, false, 0
		for _, unit := range future.Units {
			var parts planner.KeyParts
			if json.Unmarshal(unit.KeyParts, &parts) != nil || !planner.ReadsTreeBuild(parts.Kind) {
				continue
			}
			if unit.Tree == "" {
				if unit.Decision == "run" {
					keyless++
				}
				continue
			}
			keys[unit.Tree], releases[parts.Tools.Go] = true, true
			running = running || unit.Decision == "run"
		}
		switch {
		case keyless > 0:
			problems = append(problems, fmt.Sprintf("future %s: %d running units carry no tree key", future.Future, keyless))
		case len(keys) > 1:
			problems = append(problems, fmt.Sprintf("future %s: its units carry %d tree keys", future.Future, len(keys)))
		case len(releases) > 1 || releases[""]:
			problems = append(problems, fmt.Sprintf("future %s: its units name %d Go releases, not one", future.Future, len(releases)))
		case running:
			for key := range keys {
				for release := range releases {
					if !seen[key] {
						seen[key] = true
						wants = append(wants, Want{Tree: key, Future: future.Future, Go: release})
					}
				}
			}
		}
	}
	return wants, problems
}

// Requested lists the trees Judge asked for as wants, the commit as the future checked out, in the order asked: a
// base's tree serves every future on that base, so they go ahead of the listing's. A request whose tree isn't a
// tree key, whose commit isn't a full commit, or whose Go isn't a release is named, never built.
func Requested(requests []Request) ([]Want, []string) {
	wants, problems := []Want{}, []string{}
	for _, request := range requests {
		if !protocol.Sha256Pattern.MatchString(request.Tree) || !protocol.CommitPattern.MatchString(request.Commit) || !protocol.GoVersionPattern.MatchString(request.Go) {
			problems = append(problems, fmt.Sprintf("Judge's request %+v isn't a tree key, a commit and a Go release", request))
			continue
		}
		wants = append(wants, Want{Tree: request.Tree, Future: request.Commit, Go: request.Go})
	}
	return wants, problems
}

// A Builder builds the listed futures' trees the store lacks, and the trees Judge asked for, one at a time.
type Builder struct {
	Source judge.FutureSource // Queue's planned listing (judge.HTTPFutures)
	// Requests are the trees Judge asked for (ReadRequests over its request file), built ahead of the listing's: a
	// base's tree, which a rerun on it waits for. Nil asks for none.
	Requests func() ([]Request, error)
	// Indexed says whether the action store holds trees/<tree>.json.
	Indexed func(tree string) (bool, error)
	// Floor refuses while a filesystem a build writes is under its floor (builder.CheckFloor over build-tree's watches
	// and the clone's): nothing is checked out or built until it has room.
	Floor func() error
	// Build checks the future's commit out keyless in the builder's clone and runs `loom build-tree` on it with the
	// plan's tree key, which refuses a tree keying otherwise, calling running with the child's pid once it runs, and
	// returns the build's phases, nil when it has none. Its error is why the build ended badly, or ErrDetached when the
	// builder stopped and left the child running; whether the tree was built is read from the store after it, never
	// from its exit.
	Build func(want Want, running func(pid int) error) (*builder.TreePhases, error)
	// Phases reads a tree's phases from its build's log once an adopted build ends, which returned none to this
	// builder (nil: an adopted build's record holds none).
	Phases func(tree string) *builder.TreePhases
	// Alive says whether pid is still the build of tree a builder started; Kill kills its process group. Bound is the
	// longest a build runs, an adopted one counted from its running record; Poll how often an adopted one is looked at
	// (zero: AdoptPoll), Sleep how it waits (nil: time.Sleep), and Stopping whether this builder was told to stop, when
	// it leaves an adopted build running for the next.
	Alive    func(pid int, tree string) bool
	Kill     func(pid int)
	Bound    time.Duration
	Poll     time.Duration
	Sleep    func(time.Duration)
	Stopping func() bool
	// WantPoll is how often a running build's tree is asked about, stopped once nothing wants it (unwanted.go; zero:
	// WantPoll).
	WantPoll time.Duration
	Ledger   *Ledger
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

// BuildOnce builds the first tree the store lacks whose last build doesn't stand failed, Judge's requests ahead of the
// listing's, and says whether it ran a build (well or badly). A tree another build put up meanwhile is left; one refused under the floor waits.
func (builder *Builder) BuildOnce() (bool, error) {
	// A build a stopped builder left running is this one's first, before any checkout moves the clone under it.
	if adopted, err := builder.adopt(); adopted || err != nil {
		return adopted, err
	}
	// A builder told to stop while it waited on an adopted build leaves it running and starts nothing new.
	if builder.Stopping != nil && builder.Stopping() {
		return false, nil
	}
	futures, err := builder.Source.Planned()
	if err != nil {
		return false, err
	}
	wants, problems := Wanted(futures)
	if builder.Requests != nil {
		requests, err := builder.Requests()
		if err != nil {
			return false, fmt.Errorf("Judge's requests: %w", err)
		}
		requested, refused := Requested(requests)
		problems = append(problems, refused...)
		wants = append(requested, wants...)
	}
	for _, problem := range problems {
		builder.note(problem + ": not built")
	}
	if err := builder.compact(); err != nil {
		fmt.Fprintf(builder.Log, "compacting the ledger: %v\n", err)
	}
	// The trees due, fewest failures first, listing order among equals: a tree that keeps failing waits behind every
	// other, and never takes more than its share of the builder.
	type due struct {
		want     Want
		newest   Record
		failures int
	}
	dues := []due{}
	for _, want := range wants {
		newest, found := builder.Ledger.Newest(want.Tree)
		if found && !newest.due(builder.Now()) {
			continue
		}
		indexed, err := builder.Indexed(want.Tree)
		if err != nil {
			return false, fmt.Errorf("tree %s: %w", want.Tree, err)
		}
		if !indexed {
			dues = append(dues, due{want: want, newest: newest, failures: builder.Ledger.failures(want.Tree)})
		}
	}
	sort.SliceStable(dues, func(left, right int) bool { return dues[left].failures < dues[right].failures })
	for _, next := range dues {
		want, newest := next.want, next.newest
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
	var watch *stopWatch
	phases, buildErr := builder.Build(want, func(pid int) error {
		if err := builder.Ledger.Append(Record{Tree: want.Tree, Future: want.Future, At: builder.Now().UTC().Format(time.RFC3339), Event: Running, Pid: pid}); err != nil {
			return err
		}
		watch = builder.watch(want.Tree, pid)
		return nil
	})
	unwanted := watch.end()
	if errors.Is(buildErr, ErrDetached) {
		// Its running record stands, for the next builder to adopt.
		fmt.Fprintf(builder.Log, "tree %s of %s: left running for the next builder: %v\n", want.Tree, want.Future, buildErr)
		return nil
	}
	indexed, indexErr := builder.Indexed(want.Tree)
	record.At, record.Seconds, record.Phases = builder.Now().UTC().Format(time.RFC3339), builder.Now().Sub(started).Seconds(), phases
	switch {
	case unwanted != "" && !(indexErr == nil && indexed):
		// Nothing wants the tree: never the tree's failure, and no listing builds it again unless one wants it.
		record.Event, record.Cause = Stopped, "stopped mid-build: "+unwanted
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
	if record.Event == Failed {
		builder.backOff(&record, errors.Is(buildErr, ErrTransient) && indexErr == nil)
	}
	fmt.Fprintf(builder.Log, "tree %s of %s: %s\n", want.Tree, want.Future, record)
	return builder.Ledger.Append(record)
}

// AdoptPoll is how often an adopted build is looked at.
const AdoptPoll = 5 * time.Second

// adopt waits for each build a stopped builder left running (its newest record Running), in the ledger's order, and
// records how it ended, by the store: built when its index is up; failed, backed off, when it ran past Bound and was
// killed; and otherwise stopped, built again at once, since an adopted child's exit isn't this builder's to read (an
// older release's build may leave an index this one doesn't). One whose process is gone is decided the same way. It
// says whether it adopted one, and leaves the build running when this builder is told to stop meanwhile.
func (builder *Builder) adopt() (bool, error) {
	adopted := false
	for _, held := range builder.Ledger.running() {
		fmt.Fprintf(builder.Log, "tree %s of %s: adopting its build, pid %d, running since %s\n", held.Tree, held.Future, held.Pid, held.At)
		deadline, killed, unwanted, asked := held.at().Add(builder.Bound), false, "", builder.Now()
		for builder.Alive != nil && builder.Alive(held.Pid, held.Tree) {
			if builder.Stopping != nil && builder.Stopping() {
				return adopted, nil
			}
			if !builder.Now().Before(deadline) {
				builder.Kill(held.Pid)
				killed = true
				break
			}
			if builder.Now().Sub(asked) >= builder.wantPoll() {
				asked = builder.Now()
				if unwanted = builder.unwanted(held.Tree); unwanted != "" {
					builder.Kill(held.Pid)
					break
				}
			}
			builder.sleep(builder.poll())
		}
		adopted = true
		indexed, indexErr := builder.Indexed(held.Tree)
		record := Record{Tree: held.Tree, Future: held.Future, At: builder.Now().UTC().Format(time.RFC3339), Seconds: builder.Now().Sub(held.at()).Seconds()}
		if builder.Phases != nil {
			// build-tree's own phases, from its summary line; the checkout was the last builder's, and went with it.
			record.Phases = builder.Phases(held.Tree)
		}
		switch {
		case indexErr == nil && indexed:
			record.Event, record.Cause = Built, fmt.Sprintf("adopted, pid %d", held.Pid)
		case unwanted != "":
			record.Event, record.Cause = Stopped, fmt.Sprintf("the adopted build, pid %d, stopped mid-build: %s", held.Pid, unwanted)
		case indexErr != nil:
			record.Event, record.Cause = Failed, fmt.Sprintf("reading the store for its index after the adopted build: %v", indexErr)
		case killed:
			record.Event, record.Cause = Failed, fmt.Sprintf("the adopted build, pid %d, ran past its %v bound and was killed", held.Pid, builder.Bound)
		default:
			record.Event, record.Cause = Stopped, fmt.Sprintf("the adopted build, pid %d, ended and trees/%s.json isn't in the store as this release reads it: built again", held.Pid, held.Tree)
		}
		if record.Event == Failed {
			builder.backOff(&record, false)
		}
		fmt.Fprintf(builder.Log, "tree %s of %s: %s\n", record.Tree, record.Future, record)
		if err := builder.Ledger.Append(record); err != nil {
			return adopted, err
		}
	}
	return adopted, nil
}

func (builder *Builder) poll() time.Duration {
	if builder.Poll > 0 {
		return builder.Poll
	}
	return AdoptPoll
}

func (builder *Builder) sleep(duration time.Duration) {
	if builder.Sleep != nil {
		builder.Sleep(duration)
		return
	}
	time.Sleep(duration)
}

// backOff sets a failed record's retry: a transient failure's after TransientRetry, doubling with each in a row to
// TransientCap; any other's after RetryAfter, doubling with each failure since the tree was last built, and none,
// given up, at the MaxFailures'th.
func (builder *Builder) backOff(record *Record, transient bool) {
	now := builder.Now()
	if transient {
		record.Transient = true
		delay := TransientRetry << min(builder.Ledger.transients(record.Tree), 10)
		record.Retry = now.Add(min(delay, TransientCap)).UTC().Format(time.RFC3339)
		return
	}
	failures := builder.Ledger.failures(record.Tree) + 1
	if failures >= MaxFailures {
		return
	}
	record.Retry = now.Add(RetryAfter << (failures - 1)).UTC().Format(time.RFC3339)
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
