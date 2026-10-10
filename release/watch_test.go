package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Commits by letter: a is what the fleet runs when a test begins.
func commit(letter string) string { return strings.Repeat(letter, 40) }

// manifestOf is a commit's own manifest, as publish.sh writes manifests/<commit>.txt.
func manifestOf(version string) string {
	return fmt.Sprintf("version %s\nloom linux/amd64 %s\nloom-runner linux/amd64 %s\nend 2\n", version, strings.Repeat("1", 64), strings.Repeat("2", 64))
}

// A world is Workshop as the watcher sees it: an out directory publish.sh writes, a branch head, the reports the
// boxes post (through the real receiver), a clock the test moves, and counts of what was published and uploaded.
type world struct {
	t         *testing.T
	config    Config
	now       time.Time
	head      string
	orders    map[string]string
	descends  bool
	asked     time.Time // when the canary's serve last asked; zero is "just now", every time
	poolError error
	publishes atomic.Int32
	uploads   atomic.Int32
	promoted  []string
	log       bytes.Buffer
	mutex     sync.Mutex
}

func newWorld(t *testing.T) *world {
	directory := t.TempDir()
	config := DefaultConfig(directory)
	config.Out, config.State = filepath.Join(directory, "out"), filepath.Join(directory, "state")
	w := &world{t: t, config: config, now: time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC), head: commit("a"), orders: map[string]string{}, descends: true}
	os.MkdirAll(filepath.Join(config.Out, "manifests"), 0o755)
	os.WriteFile(filepath.Join(config.Out, "manifests", commit("a")+".txt"), []byte(manifestOf(commit("a"))), 0o644)
	os.WriteFile(filepath.Join(config.Out, "current.txt"), []byte(manifestOf(commit("a"))), 0o644)
	return w
}

func (w *world) clock() time.Time {
	w.mutex.Lock()
	defer w.mutex.Unlock()
	return w.now
}

func (w *world) advance(duration time.Duration) {
	w.mutex.Lock()
	w.now = w.now.Add(duration)
	w.mutex.Unlock()
}

// steps are fakes of publish.sh, upload.sh and git, with the real Promote.
func (w *world) steps() Steps {
	return Steps{
		Head:     func(context.Context) (string, error) { return w.head, nil },
		Descends: func(context.Context, string, string) (bool, error) { return w.descends, nil },
		Order:    func(_ context.Context, commit string) (string, error) { return w.orders[commit], nil },
		Publish: func(_ context.Context, version, canary string) error {
			w.publishes.Add(1)
			os.WriteFile(filepath.Join(w.config.Out, "manifests", version+".txt"), []byte(manifestOf(version)), 0o644)
			published, err := ReadPublished(w.config.Out)
			if err != nil {
				return err
			}
			text := fmt.Sprintf("canary %s\n%s\n%s", canary, strings.Join(append([]string{"version " + published.Top()}, published.Sections[0].Files...), "\n")+"\nend 2", manifestOf(version))
			return os.WriteFile(filepath.Join(w.config.Out, "current.txt"), []byte(text), 0o644)
		},
		Promote: func(version string) error {
			w.promoted = append(w.promoted, version)
			return Promote(w.config.Out, version)
		},
		Upload: func(context.Context) error { w.uploads.Add(1); return nil },
		PoolSeen: func(context.Context, string) (time.Time, error) {
			if w.asked.IsZero() { // asking all along
				return w.clock(), w.poolError
			}
			return w.asked, w.poolError
		},
	}
}

func (w *world) watcher() *Watcher {
	return &Watcher{Config: w.config, Steps: w.steps(), Now: w.clock, Log: &w.log}
}

func (w *world) tick() {
	w.t.Helper()
	if err := w.watcher().Tick(context.Background()); err != nil {
		w.t.Fatalf("a pass: %v\n%s", err, w.log.String())
	}
}

func (w *world) state() State {
	w.t.Helper()
	state, err := ReadState(w.config.State)
	if err != nil {
		w.t.Fatal(err)
	}
	return state
}

func (w *world) published() Manifest {
	w.t.Helper()
	manifest, err := ReadPublished(w.config.Out)
	if err != nil {
		w.t.Fatal(err)
	}
	return manifest
}

// report posts a box's report through the receiver, as its updater does, at the world's time.
func (w *world) report(host, version, hooked string, services ...string) {
	w.t.Helper()
	body, _ := json.Marshal(Report{Host: host, Version: version, Hooked: hooked, Services: services, At: w.clock().Format(time.RFC3339)})
	request := httptest.NewRequest(http.MethodPost, "/report", bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	Receiver{Directory: filepath.Join(w.config.State, "reports"), Hosts: w.config.Boxes, Now: w.clock}.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		w.t.Fatalf("the receiver answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

func (w *world) reportHeld(host, version, held string) {
	w.t.Helper()
	body, _ := json.Marshal(Report{Host: host, Version: version, Hooked: version, Held: held})
	recorder := httptest.NewRecorder()
	Receiver{Directory: filepath.Join(w.config.State, "reports"), Hosts: w.config.Boxes, Now: w.clock}.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/report", bytes.NewReader(body)))
}

const serveUp = "loom-serve.service active running restarts=0"

func serveRestarted(count int) string {
	return fmt.Sprintf("loom-serve.service active running restarts=%d", count)
}

// soakHealthy has the canary report the release healthy every 5 minutes through the soak, then one pass past it.
func (w *world) soakHealthy(version string) {
	for range 2 {
		w.advance(5 * time.Minute)
		w.report("Cloud", version, version, serveUp)
		w.tick()
	}
	w.advance(30 * time.Second)
	w.tick()
}

// A canary that never reports the release is never promoted: the release stops once the canary's time is up, Cloud is
// left on it for inspection, every other box stays where it was, and nothing more is published until a person says.
// A report from before the canary was published (here one naming the same commit, as a retried release finds it)
// says nothing of this release.
func TestACanaryThatNeverReportsIsNotPromoted(t *testing.T) {
	w := newWorld(t)
	w.report("Cloud", commit("b"), commit("b"), serveUp) // from an earlier attempt at b
	w.advance(time.Minute)
	w.head = commit("b")
	w.tick()
	if state := w.state(); state.Phase != PhaseCanary || w.publishes.Load() != 1 || w.published().CanaryVersion() != commit("b") || w.uploads.Load() != 1 {
		t.Fatalf("after the first pass: %+v, %d published\n%s", state, w.publishes.Load(), w.log.String())
	}
	for range 7 {
		w.advance(2 * time.Minute)
		w.tick()
		if state := w.state(); state.Phase != PhaseCanary {
			t.Fatalf("moved on from the canary with nothing heard: %+v", state)
		}
	}
	w.advance(2 * time.Minute)
	w.tick()
	state := w.state()
	if state.Phase != PhaseStopped || !strings.Contains(state.Why, "didn't report "+short(commit("b"))+" installed") || state.Failed != commit("b") {
		t.Fatalf("a silent canary: %+v\n%s", state, w.log.String())
	}
	if len(w.promoted) != 0 || w.published().CanaryVersion() != commit("b") || w.published().Top() != commit("a") {
		t.Fatalf("promoted %q, published %+v", w.promoted, w.published())
	}
	if !strings.Contains(w.log.String(), "RELEASE STOPPED") || !strings.Contains(w.log.String(), "loom release rollback") {
		t.Fatalf("not loud:\n%s", w.log.String())
	}
	// Stopped: a late healthy report and a new head move nothing.
	w.report("Cloud", commit("b"), commit("b"), serveUp)
	w.head = commit("c")
	w.advance(time.Hour)
	w.tick()
	if w.publishes.Load() != 1 || len(w.promoted) != 0 || w.state().Phase != PhaseStopped {
		t.Fatalf("a stopped watcher moved: %d published, promoted %q", w.publishes.Load(), w.promoted)
	}
}

// Each way a canary can fail stops the release before any other box sees it: its serve restarting more than a release
// drain and an hourly turnover explain, its hooks failing, its serve down when the soak ends, leaving the release, or
// its serve not asking its pool.
func TestAFailingCanaryStopsTheRelease(t *testing.T) {
	for name, test := range map[string]struct {
		fail func(w *world)
		why  string
	}{
		"its serve restarts and restarts": {func(w *world) {
			w.report("Cloud", commit("b"), commit("b"), serveUp)
			w.tick()
			for restarts := 1; restarts <= 3; restarts++ {
				w.advance(time.Minute)
				w.report("Cloud", commit("b"), commit("b"), serveRestarted(restarts))
				w.tick()
			}
		}, "restarted 3 times during the soak, more than 2"},
		"its hooks fail": {func(w *world) {
			for range 16 {
				w.report("Cloud", commit("b"), commit("a"), serveUp)
				w.tick()
				w.advance(time.Minute)
			}
			w.tick()
		}, "hooks passed for " + short(commit("a"))},
		"its serve is down when the soak ends": {func(w *world) {
			w.report("Cloud", commit("b"), commit("b"), serveUp)
			w.tick()
			w.advance(5 * time.Minute)
			w.tick()
			w.advance(5*time.Minute + time.Second)
			w.report("Cloud", commit("b"), commit("b"), "loom-serve.service failed failed restarts=1")
			w.tick()
		}, "ended the soak unhealthy: loom-serve.service failed failed"},
		"it leaves the release": {func(w *world) {
			w.report("Cloud", commit("b"), commit("b"), serveUp)
			w.tick()
			w.advance(3 * time.Minute)
			w.report("Cloud", commit("a"), commit("a"), serveUp)
			w.tick()
		}, "left " + short(commit("b")) + " during the soak"},
		"its serve doesn't ask its pool": {func(w *world) {
			w.asked = w.now.Add(-time.Hour)
			w.report("Cloud", commit("b"), commit("b"), serveUp)
			w.tick()
			for range 4 {
				w.advance(4 * time.Minute)
				w.report("Cloud", commit("b"), commit("b"), serveUp)
				w.tick()
			}
		}, "serve isn't asking its pool"},
		"the wire can't say whether it asks": {func(w *world) {
			w.poolError = errors.New("GET /pools/box-strict: 502")
			w.report("Cloud", commit("b"), commit("b"), serveUp)
			w.tick()
			for range 4 {
				w.advance(4 * time.Minute)
				w.report("Cloud", commit("b"), commit("b"), serveUp)
				w.tick()
			}
		}, "502"},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.head = commit("b")
			w.tick()
			w.advance(time.Minute)
			test.fail(w)
			state := w.state()
			if state.Phase != PhaseStopped || !strings.Contains(state.Why, test.why) {
				t.Fatalf("%+v\n%s", state, w.log.String())
			}
			if len(w.promoted) != 0 || w.published().CanaryVersion() != commit("b") || w.published().Top() != commit("a") {
				t.Fatalf("promoted %q, published %+v", w.promoted, w.published())
			}
		})
	}
}

// A canary that stays healthy (one drain restart allowed) is promoted after the soak; every box but a held one then
// reports the release, and the release is done. The held box is named, never waited on.
func TestAHealthyCanaryIsPromotedAndTheFleetFollows(t *testing.T) {
	w := newWorld(t)
	w.head = commit("b")
	w.tick()
	w.advance(2 * time.Minute)
	w.report("Cloud", commit("b"), commit("b"), serveUp)
	w.tick()
	if w.state().Phase != PhaseSoak {
		t.Fatalf("%+v\n%s", w.state(), w.log.String())
	}
	w.advance(5 * time.Minute)
	w.report("Cloud", commit("b"), commit("b"), serveRestarted(1)) // the release's drain
	w.tick()
	if len(w.promoted) != 0 {
		t.Fatal("promoted before the soak ended")
	}
	w.advance(5*time.Minute + time.Second)
	w.report("Cloud", commit("b"), commit("b"), serveRestarted(1))
	w.tick()
	if state := w.state(); state.Phase != PhaseFleet || len(w.promoted) != 1 || w.published().CanaryVersion() != "" || w.published().Top() != commit("b") || w.uploads.Load() != 2 {
		t.Fatalf("after the soak: %+v, promoted %q, uploads %d\n%s", state, w.promoted, w.uploads.Load(), w.log.String())
	}
	w.reportHeld("Chonchon", commit("a"), commit("a"))
	w.advance(time.Minute)
	for _, box := range []string{"Workshop", "Server"} {
		w.report(box, commit("b"), commit("b"))
	}
	w.tick()
	if w.state().Phase != PhaseFleet {
		t.Fatalf("done while Home hadn't reported: %+v", w.state())
	}
	w.report("Home", commit("b"), commit("b"))
	w.tick()
	state := w.state()
	if state.Phase != PhaseDone || len(state.Held) != 1 || !strings.Contains(state.Held[0], "Chonchon") || len(state.Lagging) != 0 {
		t.Fatalf("%+v\n%s", state, w.log.String())
	}
	if !strings.Contains(w.log.String(), "HELD: Chonchon") || !strings.Contains(w.log.String(), "released "+short(commit("b"))) {
		t.Fatalf("the log:\n%s", w.log.String())
	}
	// Released: the same head publishes nothing more.
	w.tick()
	if w.publishes.Load() != 1 {
		t.Fatalf("published %d times", w.publishes.Load())
	}
}

// A box that doesn't take the release in time is named as lagging, loudly, without stopping the release: an offline
// box catches up when it returns.
func TestABoxThatDoesntFollowIsNamedLagging(t *testing.T) {
	w := newWorld(t)
	w.head = commit("b")
	w.tick()
	w.advance(time.Second)
	w.report("Cloud", commit("b"), commit("b"), serveUp)
	w.tick()
	w.soakHealthy(commit("b"))
	if w.state().Phase != PhaseFleet {
		t.Fatalf("%+v\n%s", w.state(), w.log.String())
	}
	w.advance(time.Minute)
	for _, box := range []string{"Workshop", "Server", "Chonchon"} {
		w.report(box, commit("b"), commit("b"))
	}
	w.advance(15 * time.Minute)
	w.tick()
	if state := w.state(); state.Phase != PhaseDone || len(state.Lagging) != 1 || state.Lagging[0] != "Home" || !strings.Contains(w.log.String(), "LAGGING: Home") {
		t.Fatalf("%+v\n%s", state, w.log.String())
	}
}

// Two watchers on one out directory (a second one started by hand, say) never publish at once: whoever holds the lock
// publishes, and the other's pass changes nothing.
func TestTwoWatchersNeverPublishAtOnce(t *testing.T) {
	w := newWorld(t)
	w.head = commit("b")
	var running, most atomic.Int32
	steps := w.steps()
	head := steps.Head
	steps.Head = func(callContext context.Context) (string, error) {
		time.Sleep(50 * time.Millisecond)
		return head(callContext)
	}
	publish := steps.Publish
	steps.Publish = func(callContext context.Context, version, canary string) error {
		now := running.Add(1)
		defer running.Add(-1)
		for {
			seen := most.Load()
			if now <= seen || most.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
		return publish(callContext, version, canary)
	}
	other := w.config
	other.State = filepath.Join(t.TempDir(), "another-state")
	watchers := []*Watcher{{Config: w.config, Steps: steps, Now: w.clock, Log: &bytes.Buffer{}}, {Config: other, Steps: steps, Now: w.clock, Log: &bytes.Buffer{}}}
	errs := make([]error, len(watchers))
	var group sync.WaitGroup
	for index, watcher := range watchers {
		group.Add(1)
		go func() {
			defer group.Done()
			errs[index] = watcher.Tick(context.Background())
		}()
	}
	group.Wait()
	if most.Load() != 1 || w.publishes.Load() != 1 {
		t.Fatalf("%d published at once, %d in all", most.Load(), w.publishes.Load())
	}
	loser := -1
	for index, err := range errs {
		if errors.Is(err, ErrHeld) {
			loser = index
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if !errors.Is(errs[0], ErrHeld) == !errors.Is(errs[1], ErrHeld) {
		t.Fatalf("not one pass found the lock held: %v", errs)
	}
	// The other watcher's next pass finds a canary it didn't begin, and stops rather than begin a second release.
	if err := watchers[loser].Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if state, _ := ReadState(watchers[loser].Config.State); state.Phase != PhaseStopped || !strings.Contains(state.Why, "no release here began it") || w.publishes.Load() != 1 {
		t.Fatalf("the second watcher: %+v, %d published", state, w.publishes.Load())
	}
}

// A release's order is honored: a step after the fleet holds the release (and the next one) until it is marked done; a
// step before the fleet holds publishing itself; a rule the watcher can't honor stops the release unpublished.
func TestAReleaseHonorsItsOrder(t *testing.T) {
	t.Run("workers after the fleet", func(t *testing.T) {
		w := newWorld(t)
		w.head = commit("b")
		w.orders[commit("b")] = "# the Go judge must be on every box before the Workers move\nworkers after fleet\n"
		w.tick()
		w.advance(time.Second)
		w.report("Cloud", commit("b"), commit("b"), serveUp)
		w.tick()
		w.soakHealthy(commit("b"))
		w.advance(time.Minute)
		for _, box := range []string{"Workshop", "Server", "Home", "Chonchon"} {
			w.report(box, commit("b"), commit("b"))
		}
		w.tick()
		if state := w.state(); state.Phase != PhaseAfter || !strings.Contains(w.log.String(), "WAITING: "+short(commit("b"))+"'s updater/release-order puts workers after the fleet") {
			t.Fatalf("%+v\n%s", state, w.log.String())
		}
		w.head = commit("c")
		w.advance(time.Hour)
		w.tick()
		if w.publishes.Load() != 1 || w.state().Phase != PhaseAfter {
			t.Fatalf("the next release didn't wait: %d published, %+v", w.publishes.Load(), w.state())
		}
		if err := Mark(w.config.State, commit("b"), "workers"); err != nil {
			t.Fatal(err)
		}
		w.tick()
		if w.state().Phase != PhaseDone {
			t.Fatalf("marked, still %+v", w.state())
		}
		w.tick()
		if w.publishes.Load() != 2 || w.published().CanaryVersion() != commit("c") {
			t.Fatalf("the next release: %d published", w.publishes.Load())
		}
	})
	t.Run("workers before the fleet", func(t *testing.T) {
		w := newWorld(t)
		w.head = commit("b")
		w.orders[commit("b")] = "fleet after workers\n"
		w.tick()
		w.advance(time.Hour)
		w.tick()
		if state := w.state(); state.Phase != PhaseBefore || w.publishes.Load() != 0 || !strings.Contains(w.log.String(), "puts workers before the fleet") {
			t.Fatalf("%+v, %d published\n%s", state, w.publishes.Load(), w.log.String())
		}
		Mark(w.config.State, commit("b"), "workers")
		w.tick()
		if w.state().Phase != PhaseCanary || w.publishes.Load() != 1 {
			t.Fatalf("marked, still %+v", w.state())
		}
	})
	t.Run("a rule it can't honor", func(t *testing.T) {
		w := newWorld(t)
		w.head = commit("b")
		w.orders[commit("b")] = "judge before workers\n"
		w.tick()
		if state := w.state(); state.Phase != PhaseStopped || !strings.Contains(state.Why, "isn't \"<step> after fleet\"") || w.publishes.Load() != 0 {
			t.Fatalf("%+v, %d published", state, w.publishes.Load())
		}
	})
}

// A head that doesn't descend from the fleet's release (a forced push, a wrong branch) stops, unpublished.
func TestAHeadThatDoesntDescendStops(t *testing.T) {
	w := newWorld(t)
	w.head, w.descends = commit("b"), false
	w.tick()
	if state := w.state(); state.Phase != PhaseStopped || !strings.Contains(state.Why, "doesn't descend") || w.publishes.Load() != 0 {
		t.Fatalf("%+v", state)
	}
}

// After a stop: resume refuses while the canary is published; rollback ends it; resume then releases the next head but
// not the commit that stopped it, unless retried.
func TestRollbackAndResume(t *testing.T) {
	w := newWorld(t)
	w.head = commit("b")
	w.tick()
	w.advance(16 * time.Minute)
	w.tick()
	if w.state().Phase != PhaseStopped {
		t.Fatal(w.state())
	}
	var out bytes.Buffer
	if err := Resume(w.config, false, &out); err == nil || !strings.Contains(err.Error(), "still published") {
		t.Fatalf("resumed over a published canary: %v", err)
	}
	if err := Rollback(context.Background(), w.config, w.steps(), &out); err != nil {
		t.Fatal(err)
	}
	if published := w.published(); published.CanaryVersion() != "" || published.Top() != commit("a") || w.uploads.Load() != 2 {
		t.Fatalf("rolled back: %+v, %d uploads", published, w.uploads.Load())
	}
	if err := Rollback(context.Background(), w.config, w.steps(), &out); err == nil {
		t.Fatal("rolled back with no canary")
	}
	if err := Resume(w.config, false, &out); err != nil {
		t.Fatal(err)
	}
	w.tick()
	if w.publishes.Load() != 1 || w.state().Phase != PhaseIdle {
		t.Fatalf("released the commit that stopped it again: %+v", w.state())
	}
	w.head = commit("c")
	w.tick()
	if w.publishes.Load() != 2 || w.published().CanaryVersion() != commit("c") {
		t.Fatalf("the next head: %d published", w.publishes.Load())
	}
}

// While a release moves, the lock and the phase keep a person's rollback out of it.
func TestRollbackRefusesAMovingRelease(t *testing.T) {
	w := newWorld(t)
	w.head = commit("b")
	w.tick()
	if err := Rollback(context.Background(), w.config, w.steps(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "canary phase") {
		t.Fatalf("rolled back a release in its canary phase: %v", err)
	}
	unlock, err := Lock(w.config.Out)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := Rollback(context.Background(), w.config, w.steps(), &bytes.Buffer{}); !errors.Is(err, ErrHeld) {
		t.Fatalf("rolled back under the lock: %v", err)
	}
}
