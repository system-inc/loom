package treebuilder

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
)

var (
	keyA, keyB, keyC, keyD = strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
)

type listedFutures []judge.PlannedFuture

func (futures listedFutures) Planned() ([]judge.PlannedFuture, error) { return futures, nil }

func unit(t *testing.T, kind, decision, tree string) judge.PlannedUnitWire {
	t.Helper()
	parts, err := json.Marshal(planner.KeyParts{Kind: kind, Package: "github.com/system-inc/adamic/internal/x", Tools: planner.Tools{Go: "go1.27.1"}})
	if err != nil {
		t.Fatal(err)
	}
	return judge.PlannedUnitWire{UnitKey: strings.Repeat("1", 64), KeyParts: parts, Decision: decision, Tree: tree}
}

func future(sha string, units ...judge.PlannedUnitWire) judge.PlannedFuture {
	return judge.PlannedFuture{Future: strings.Repeat(sha, 40), Attempt: 1, Units: units}
}

// harness is a Builder over fakes: the store's indexes, the disk's floor and every build it would run.
type harness struct {
	builder *Builder
	indexed map[string]bool
	builds  []Want
	short   error
	fail    error
	now     time.Time
}

func newHarness(t *testing.T, source judge.FutureSource, ledger *Ledger) *harness {
	h := &harness{indexed: map[string]bool{}, now: time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)}
	h.builder = &Builder{
		Source:  source,
		Indexed: func(tree string) (bool, error) { return h.indexed[tree], nil },
		Floor:   func() error { return h.short },
		Build: func(want Want, running func(pid int) error) error {
			h.builds = append(h.builds, want)
			if err := running(4242); err != nil {
				return err
			}
			if h.fail == nil {
				h.indexed[want.Tree] = true
			}
			return h.fail
		},
		Ledger: ledger,
		Now:    func() time.Time { return h.now },
		Log:    io.Discard,
	}
	return h
}

func openLedger(t *testing.T, path string, now time.Time) *Ledger {
	t.Helper()
	ledger, err := OpenLedger(path, now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ledger.Close() })
	return ledger
}

func (h *harness) buildOnce(t *testing.T, want bool) {
	t.Helper()
	built, err := h.builder.BuildOnce()
	if err != nil || built != want {
		t.Fatalf("built %v (%v), want %v", built, err, want)
	}
}

// Only a tree a listed future runs whose index the store lacks is built, the first listed first and one a pass, each
// once: not one already indexed, not one only reused units carry, not a phase-only future's. Each build is recorded
// started, then built. Mutants: the index not asked; two trees built in one pass; reused units' trees built.
func TestOnlyMissingTreesAreBuiltOneAtATime(t *testing.T) {
	source := listedFutures{
		future("1", unit(t, "test", "run", keyA), unit(t, "phase", "run", "")),
		future("2", unit(t, "test", "run", keyB), unit(t, "product", "run", keyB)),
		future("3", unit(t, "test", "reuse", keyD)),
		future("4", unit(t, "phase", "run", "")),
		future("5", unit(t, "product", "run", keyC)),
		future("6", unit(t, "test", "run", keyB)),
	}
	path := filepath.Join(t.TempDir(), "trees.jsonl")
	ledger := openLedger(t, path, time.Now())
	h := newHarness(t, source, ledger)
	h.indexed[keyA] = true
	h.buildOnce(t, true)
	if len(h.builds) != 1 || h.builds[0] != (Want{Tree: keyB, Future: strings.Repeat("2", 40), Go: "go1.27.1"}) {
		t.Fatalf("the first pass built %v, want tree b of future 2 alone", h.builds)
	}
	if newest, _ := ledger.Newest(keyB); newest.Event != Built || newest.Future != strings.Repeat("2", 40) {
		t.Fatalf("tree b's newest record is %+v", newest)
	}
	h.buildOnce(t, true)
	h.buildOnce(t, false)
	if len(h.builds) != 2 || h.builds[1].Tree != keyC {
		t.Fatalf("builds %v, want b then c and nothing more", h.builds)
	}
	if _, found := ledger.Newest(keyD); found {
		t.Fatal("a tree only a reused unit carries was built")
	}
	// A second builder can't take the ledger: two would build one tree twice.
	if second, err := OpenLedger(path, time.Now()); err == nil || !strings.Contains(err.Error(), "another tree builder holds it") {
		t.Fatalf("a second builder took the ledger (%v)", err)
	} else if second != nil {
		t.Fatal("a refused ledger came back")
	}
}

// A builder that stops mid-build (a restart, a crash) left its tree started: the next one records it interrupted and
// builds it again, and a crash's cut-short last line is dropped. Mutant: a started tree never recorded interrupted, so
// the placer can't tell it from one building.
func TestABuildCutShortByACrashIsBuiltAgain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trees.jsonl")
	source := listedFutures{future("2", unit(t, "test", "run", keyB))}
	now := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
	// The crash: the builder dies with its ledger saying started, and a line it was writing cut short.
	crashed := openLedger(t, path, now)
	crashed.Append(Record{Tree: keyB, Future: strings.Repeat("2", 40), At: now.Format(time.RFC3339), Event: Started})
	crashed.Close()
	file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	file.WriteString(`{"tree":"` + keyC + `","ev`)
	file.Close()

	restarted := openLedger(t, path, now)
	if newest, _ := restarted.Newest(keyB); newest.Event != Interrupted || !strings.Contains(newest.Cause, "stopped mid-build") {
		t.Fatalf("after the restart tree b's newest record is %+v, want interrupted", newest)
	}
	if read, found, err := Newest(path, keyB); err != nil || !found || read.Event != Interrupted {
		t.Fatalf("the placer reads %+v %v %v", read, found, err)
	}
	again := newHarness(t, source, restarted)
	again.buildOnce(t, true)
	if len(again.builds) != 1 {
		t.Fatalf("builds %v, want tree b built again", again.builds)
	}
	if newest, _ := restarted.Newest(keyB); newest.Event != Built {
		t.Fatalf("tree b's newest record is %+v", newest)
	}
	if content, _ := os.ReadFile(path); strings.Contains(string(content), `"ev`+"\n") || !strings.HasSuffix(string(content), "}\n") {
		t.Fatalf("the ledger is %q", content)
	}
}

// Under the floor nothing is checked out or built: the tree is recorded refused once, however many passes the disk
// stays short, and built once it has room. Mutants: the floor not read; a refusal recorded every pass.
func TestUnderTheFloorNothingIsBuilt(t *testing.T) {
	source := listedFutures{future("2", unit(t, "test", "run", keyB))}
	ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
	h := newHarness(t, source, ledger)
	h.short = errors.New("the cache base (/trees) has 120.0 GB free, under its 200 GB floor")
	h.buildOnce(t, false)
	h.buildOnce(t, false)
	if len(h.builds) != 0 {
		t.Fatalf("built %v under the floor", h.builds)
	}
	if newest, _ := ledger.Newest(keyB); newest.Event != Refused || !strings.Contains(newest.Cause, "under its 200 GB floor") {
		t.Fatalf("tree b's newest record is %+v", newest)
	}
	if refusals := len(ledger.order); refusals != 1 {
		t.Fatalf("%d records for two short passes, want one refusal", refusals)
	}
	h.short = nil
	h.buildOnce(t, true)
	if newest, _ := ledger.Newest(keyB); len(h.builds) != 1 || newest.Event != Built {
		t.Fatalf("with room: builds %v, newest %+v", h.builds, newest)
	}
}

// A build that leaves no index is failed with why, Workshop's, and stands failed for RetryAfter, then is built again;
// one that left its index is built whatever build-tree's exit (a package that didn't compile is the index's to say).
// Mutants: a failure built again at once; the exit read instead of the store.
func TestAFailedBuildStandsUntilItsRetry(t *testing.T) {
	source := listedFutures{future("2", unit(t, "test", "run", keyB))}
	ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
	h := newHarness(t, source, ledger)
	h.fail = errors.New("build-tree: exit status 1: build-tree: checking abc out keyless: git fetch: 502")
	h.buildOnce(t, true)
	newest, _ := ledger.Newest(keyB)
	if newest.Event != Failed || !strings.Contains(newest.Cause, "502") || !newest.Standing(h.now) {
		t.Fatalf("tree b's newest record is %+v", newest)
	}
	h.now = h.now.Add(RetryAfter - time.Minute)
	h.buildOnce(t, false)
	if len(h.builds) != 1 {
		t.Fatalf("a standing failure was built again: %v", h.builds)
	}
	h.now = h.now.Add(2 * time.Minute)
	if newest.Standing(h.now) {
		t.Fatal("a failure past RetryAfter still stands")
	}
	h.fail = errors.New("build-tree: exit status 1: 2 packages failed")
	h.builder.Build = func(want Want, _ func(int) error) error {
		h.builds = append(h.builds, want)
		h.indexed[want.Tree] = true
		return h.fail
	}
	h.buildOnce(t, true)
	if newest, _ := ledger.Newest(keyB); len(h.builds) != 2 || newest.Event != Built || !strings.Contains(newest.Cause, "2 packages failed") {
		t.Fatalf("builds %d, newest %+v: an index that went up is built", len(h.builds), newest)
	}
}

// A plan that can't say which build its units run is named and never built: units carrying two tree keys, or a
// running test or phase unit carrying none (a planner older than tree keys; a phase unit reads its tree's npm
// packages, #v03v751). Mutant: the first key taken.
func TestAPlanNamingNoOneTreeIsNeverBuilt(t *testing.T) {
	wants, problems := Wanted([]judge.PlannedFuture{
		future("1", unit(t, "test", "run", keyA), unit(t, "test", "run", keyB)),
		future("2", unit(t, "test", "run", ""), unit(t, "phase", "run", "")),
		future("3", unit(t, "test", "reuse", ""), unit(t, "test", "run", keyC)),
	})
	if len(wants) != 1 || wants[0].Tree != keyC {
		t.Fatalf("wanted %v, want only future 3's tree", wants)
	}
	if len(problems) != 2 || !strings.Contains(problems[0], "2 tree keys") || !strings.Contains(problems[1], "2 running units carry no tree key") {
		t.Fatalf("problems %v", problems)
	}
}

// A future whose units name two Go releases can't say which go builds its tree, and isn't built; one release is carried
// to build-tree, which refuses another. Mutant: the releases not compared.
func TestAPlanNamingTwoGoReleasesIsNeverBuilt(t *testing.T) {
	other := unit(t, "test", "run", keyA)
	var parts planner.KeyParts
	json.Unmarshal(other.KeyParts, &parts)
	parts.Tools.Go = "go1.27.2"
	other.KeyParts, _ = json.Marshal(parts)
	wants, problems := Wanted([]judge.PlannedFuture{future("1", unit(t, "test", "run", keyA), other), future("2", unit(t, "test", "run", keyB))})
	if len(wants) != 1 || wants[0] != (Want{Tree: keyB, Future: strings.Repeat("2", 40), Go: "go1.27.1"}) {
		t.Fatalf("wanted %v", wants)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "2 Go releases") {
		t.Fatalf("problems %v", problems)
	}
}

// A tree that keeps failing backs off, RetryAfter then twice that, and at MaxFailures is given up, standing for good,
// so it never takes the builder again (review of tree-wiring, finding 5). Mutants: no backoff; no cap.
func TestRepeatedFailuresBackOffAndAreGivenUp(t *testing.T) {
	source := listedFutures{future("2", unit(t, "test", "run", keyB))}
	ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
	h := newHarness(t, source, ledger)
	h.fail = errors.New("build-tree: exit status 1: the store answered 500")
	start := h.now
	for _, step := range []struct {
		after  time.Duration
		builds int
	}{{0, 1}, {RetryAfter - time.Second, 1}, {RetryAfter, 2}, {RetryAfter + 2*RetryAfter - time.Second, 2}, {3 * RetryAfter, 3}, {48 * time.Hour, 3}} {
		h.now = start.Add(step.after)
		h.builder.BuildOnce()
		if len(h.builds) != step.builds {
			t.Fatalf("at +%v: %d builds, want %d", step.after, len(h.builds), step.builds)
		}
	}
	newest, _ := ledger.Newest(keyB)
	if newest.Retry != "" || !newest.Standing(h.now) || !strings.Contains(newest.String(), "given up after 3 failures") {
		t.Fatalf("after %d failures the newest record is %+v", MaxFailures, newest)
	}
}

// A tree that failed waits behind every tree that hasn't, whatever the listing's order: a bad tree never takes more
// than its share of the builder while others wait. Mutant: listing order alone.
func TestATreeThatFailedWaitsBehindOnesThatHavent(t *testing.T) {
	source := listedFutures{future("2", unit(t, "test", "run", keyB)), future("3", unit(t, "test", "run", keyC))}
	ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
	h := newHarness(t, source, ledger)
	ledger.Append(Record{Tree: keyB, Event: Failed, At: h.now.Add(-time.Hour).Format(time.RFC3339), Retry: h.now.Add(-time.Minute).Format(time.RFC3339)})
	h.buildOnce(t, true)
	h.buildOnce(t, true)
	if len(h.builds) != 2 || h.builds[0].Tree != keyC || h.builds[1].Tree != keyB {
		t.Fatalf("builds %v, want c, never tried, before b, which failed", h.builds)
	}
}

// A checkout that hiccups (GitHub's 5xx, the network) is transient: retried after a minute, doubling to TransientCap,
// never standing, and never counted toward giving the tree up (review of tree-wiring, finding 6). Mutants: a
// transient failure standing; transients counted as failures.
func TestATransientFailureIsRetriedSoonAndNeverStands(t *testing.T) {
	source := listedFutures{future("2", unit(t, "test", "run", keyB))}
	ledger := openLedger(t, filepath.Join(t.TempDir(), "trees.jsonl"), time.Now())
	h := newHarness(t, source, ledger)
	h.fail = fmt.Errorf("%w: checking it out keyless: git fetch: 502", ErrTransient)
	h.buildOnce(t, true)
	newest, _ := ledger.Newest(keyB)
	if !newest.Transient || newest.Standing(h.now) || newest.Retry != h.now.Add(TransientRetry).Format(time.RFC3339) {
		t.Fatalf("a transient failure is recorded %+v", newest)
	}
	for range 5 {
		h.now = h.now.Add(TransientCap)
		h.buildOnce(t, true)
	}
	if newest, _ = ledger.Newest(keyB); newest.Retry != h.now.Add(TransientCap).Format(time.RFC3339) {
		t.Fatalf("the sixth transient failure retries at %s, want capped at %v", newest.Retry, TransientCap)
	}
	// Six transients later, a real failure is the tree's first: it backs off RetryAfter, never given up.
	h.now, h.fail = h.now.Add(TransientCap), errors.New("build-tree: exit status 1")
	h.buildOnce(t, true)
	if newest, _ = ledger.Newest(keyB); newest.Transient || newest.Retry != h.now.Add(RetryAfter).Format(time.RFC3339) {
		t.Fatalf("a real failure after six transient ones is %+v, want the first, retried after %v", newest, RetryAfter)
	}
}

// A tree whose build keeps taking the builder down (a crash, every time it's built) is given up at MaxFailures like
// one that keeps failing, not rebuilt at every restart. Mutant: crashes not counted.
func TestATreeThatKeepsCrashingTheBuilderIsGivenUp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trees.jsonl")
	now := time.Date(2026, 10, 10, 13, 0, 0, 0, time.UTC)
	for crash := 1; crash <= MaxFailures; crash++ {
		ledger, err := OpenLedger(path, now)
		if err != nil {
			t.Fatal(err)
		}
		if newest, _ := ledger.Newest(keyB); crash > 1 && newest.Event != Interrupted {
			t.Fatalf("after crash %d the tree is %+v", crash-1, newest)
		}
		ledger.Append(Record{Tree: keyB, Future: strings.Repeat("2", 40), At: now.Format(time.RFC3339), Event: Started})
		ledger.Close()
	}
	ledger := openLedger(t, path, now)
	if newest, _ := ledger.Newest(keyB); newest.Event != Failed || !newest.Standing(now.Add(48*time.Hour)) || !strings.Contains(newest.Cause, "died mid-build 3 times") {
		t.Fatalf("after %d crashes the tree is %+v", MaxFailures, newest)
	}
}
