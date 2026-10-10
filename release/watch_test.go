package release

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// A world is Workshop as the watcher sees it: an out directory publish.sh writes, the current.txt upload.sh last sent
// (remote, what the boxes read), a branch head, the reports the boxes post (through the real receiver), a clock the test
// moves, and counts of what was published and uploaded.
type world struct {
	t          *testing.T
	config     Config
	now        time.Time
	head       string
	orders     map[string]string // each commit's release-order, when it changes it; main's commits run a, b, c...
	orderError error
	descends   bool
	asked      time.Time // when the canary's serve last asked; zero is "just now", every time
	poolError  error
	publishes  atomic.Int32
	uploads    atomic.Int32
	promoted   []string
	remote     string
	// uploadError fails the next uploads, and lost has them succeed with nothing reaching the boxes.
	uploadError error
	lost        bool
	log         bytes.Buffer
	mutex       sync.Mutex
}

func newWorld(t *testing.T) *world {
	directory := t.TempDir()
	config := DefaultConfig(directory)
	config.Out, config.State = filepath.Join(directory, "out"), filepath.Join(directory, "state")
	config.Listen = "10.0.0.1:7381"
	for index, box := range config.Boxes {
		config.Addresses[strings.ToLower(box)] = []string{fmt.Sprintf("10.0.0.%d", index+1)}
	}
	w := &world{t: t, config: config, now: time.Date(2026, 10, 10, 16, 0, 0, 0, time.UTC), head: commit("a"), orders: map[string]string{}, descends: true}
	os.MkdirAll(filepath.Join(config.Out, "manifests"), 0o755)
	os.WriteFile(filepath.Join(config.Out, "manifests", commit("a")+".txt"), []byte(manifestOf(commit("a"))), 0o644)
	os.WriteFile(filepath.Join(config.Out, "current.txt"), []byte(manifestOf(commit("a"))), 0o644)
	w.remote = manifestOf(commit("a"))
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
		// Order walks main's history, a commit per letter: each after from, up to to, that declares an order.
		Order: func(_ context.Context, from, to string) ([]OrderAt, error) {
			if w.orderError != nil {
				return nil, w.orderError
			}
			var orders []OrderAt
			for letter := from[0] + 1; letter <= to[0]; letter++ {
				if text, found := w.orders[commit(string(letter))]; found {
					orders = append(orders, OrderAt{Commit: commit(string(letter)), Text: text})
				}
			}
			return orders, nil
		},
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
		Upload: func(_ context.Context, current string) error {
			w.uploads.Add(1)
			if w.uploadError != nil {
				return w.uploadError
			}
			if current == "" {
				current = filepath.Join(w.config.Out, "current.txt")
			}
			content, err := os.ReadFile(current)
			if !w.lost {
				w.remote = string(content)
			}
			return err
		},
		Published: func(context.Context) (Manifest, error) { return ParseManifest(w.remote) },
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

// testSecret is the token secret the world's receiver checks reports against.
var testSecret = []byte("the house's token secret, for tests")

// receiver is the watcher's own report receiver, as `loom release watch` runs it.
func (w *world) receiver() *Receiver {
	return &Receiver{Directory: filepath.Join(w.config.State, "reports"), Hosts: w.config.Boxes, Secret: testSecret, Addresses: w.config.Addresses, Now: w.clock}
}

// reportToken is the host's report token, as `loom release report-token <host>` mints it.
func reportToken(t *testing.T, host string) string {
	t.Helper()
	token, err := MintReportToken(testSecret, host, time.Date(2027, 10, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// signed is a report's request as an updater sends it: signed with token, from the address.
func signed(token string, body []byte, from string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/report", bytes.NewReader(body))
	claims, _, _ := strings.Cut(token, ".")
	request.Header.Set(ClaimsHeader, claims)
	request.Header.Set(SignatureHeader, SignReport(token, body))
	request.RemoteAddr = net.JoinHostPort(from, "41234")
	return request
}

// post sends a report as the host's own updater does, at the world's time, and fails the test if it is refused.
func (w *world) post(report Report) {
	w.t.Helper()
	report.At = w.clock().Format(time.RFC3339)
	body, _ := json.Marshal(report)
	recorder := httptest.NewRecorder()
	w.receiver().ServeHTTP(recorder, signed(reportToken(w.t, report.Host), body, w.config.Addresses[strings.ToLower(report.Host)][0]))
	if recorder.Code != http.StatusNoContent {
		w.t.Fatalf("the receiver answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

// report posts a box's report through the receiver, as its updater does, at the world's time.
func (w *world) report(host, version, hooked string, services ...string) {
	w.t.Helper()
	w.post(Report{Host: host, Version: version, Hooked: hooked, Services: services})
}

func (w *world) reportHeld(host, version, held string) {
	w.t.Helper()
	w.post(Report{Host: host, Version: version, Hooked: version, Held: held})
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

// A canary that never installs the release isn't promoted by reports that only claim to be its own: one posted from
// another machine on the LAN, unsigned, signed with another box's token, Cloud's own token sent from elsewhere, or
// Cloud's claims (they cross the network in the open) with a signature only guessed, sent from Cloud's own address.
func TestASpoofedCanaryReportPromotesNothing(t *testing.T) {
	w := newWorld(t)
	w.head = commit("b")
	w.tick()
	if w.state().Phase != PhaseCanary {
		t.Fatalf("%+v", w.state())
	}
	spoof := func(token, from string) int {
		body, _ := json.Marshal(Report{Host: "cloud", Version: commit("b"), Hooked: commit("b"), Services: []string{serveUp}, At: w.clock().Format(time.RFC3339)})
		request := signed(token, body, from)
		if token == "" {
			request.Header.Del(ClaimsHeader)
			request.Header.Del(SignatureHeader)
		}
		recorder := httptest.NewRecorder()
		w.receiver().ServeHTTP(recorder, request)
		return recorder.Code
	}
	for range 4 {
		w.advance(4 * time.Minute)
		claims, _, _ := strings.Cut(reportToken(t, "Cloud"), ".")
		for _, attempt := range [][2]string{{"", "10.66.66.66"}, {"", "10.0.0.2"}, {reportToken(t, "Server"), "10.0.0.3"}, {reportToken(t, "Cloud"), "10.66.66.66"}, {claims + ".guessed", "10.0.0.2"}} {
			if code := spoof(attempt[0], attempt[1]); code/100 == 2 {
				t.Fatalf("a report claiming Cloud from %s was taken: %d", attempt[1], code)
			}
		}
		w.tick()
	}
	if state := w.state(); state.Phase != PhaseStopped || len(w.promoted) != 0 || w.published().Top() != commit("a") {
		t.Fatalf("Cloud never installed %s: %+v, promoted %q", short(commit("b")), state, w.promoted)
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

// A person who publishes by hand during the soak (a rollback copies an older manifest over current.txt and uploads it)
// is never overwritten: the watcher reads current.txt, in its out directory and where the boxes read it, before it
// promotes, and stops with nothing promoted when either no longer publishes its canary.
func TestAHandPublishDuringTheSoakIsNeverPromotedOver(t *testing.T) {
	for name, publish := range map[string]func(w *world){
		"in the out directory": func(w *world) {
			content, _ := os.ReadFile(filepath.Join(w.config.Out, "manifests", commit("a")+".txt"))
			os.WriteFile(filepath.Join(w.config.Out, "current.txt"), content, 0o644)
		},
		"where the boxes read it": func(w *world) { w.remote = manifestOf(commit("a")) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWorld(t)
			w.head = commit("b")
			w.tick()
			w.advance(time.Minute)
			w.report("Cloud", commit("b"), commit("b"), serveUp)
			w.tick()
			if w.state().Phase != PhaseSoak {
				t.Fatalf("%+v", w.state())
			}
			publish(w)
			w.soakHealthy(commit("b"))
			state := w.state()
			if state.Phase != PhaseStopped || !strings.Contains(state.Why, "no longer publishes the canary of "+short(commit("b"))) || len(w.promoted) != 0 || w.uploads.Load() != 1 {
				t.Fatalf("%+v, promoted %q, %d uploads\n%s", state, w.promoted, w.uploads.Load(), w.log.String())
			}
			if remote, _ := ParseManifest(w.remote); remote.Top() != commit("a") && remote.CanaryVersion() != commit("b") {
				t.Fatalf("what the boxes read changed: %s", w.remote)
			}
		})
	}
}

// The out directory's current.txt says the promotion only once the boxes read it: an upload that fails, or that
// succeeds with something else read back, stops the release with the canary still published in out, so `loom release
// rollback` ends it; a read back that fails is tried again by the next pass, which finds its own promotion there.
func TestAPromotionIsKeptOnlyOnceTheBoxesReadIt(t *testing.T) {
	soaked := func(t *testing.T) *world {
		w := newWorld(t)
		w.head = commit("b")
		w.tick()
		w.advance(time.Minute)
		w.report("Cloud", commit("b"), commit("b"), serveUp)
		w.tick()
		return w
	}
	for name, fail := range map[string]func(w *world){
		"the upload fails":                   func(w *world) { w.uploadError = errors.New("upload: current.txt failed") },
		"the upload never reaches the boxes": func(w *world) { w.lost = true },
	} {
		t.Run(name, func(t *testing.T) {
			w := soaked(t)
			fail(w)
			w.soakHealthy(commit("b"))
			if state := w.state(); state.Phase != PhaseStopped || !strings.Contains(state.Why, "promotion") && !strings.Contains(state.Why, "still publishes the canary") {
				t.Fatalf("%+v\n%s", state, w.log.String())
			}
			if len(w.promoted) != 0 || w.published().CanaryVersion() != commit("b") {
				t.Fatalf("out promoted ahead of the boxes: promoted %q, out %+v", w.promoted, w.published())
			}
			// A rollback is kept the same way: one the boxes never read leaves the canary in out, to roll back again.
			w.uploadError, w.lost = nil, true
			if err := Rollback(context.Background(), w.config, w.steps(), &bytes.Buffer{}); err == nil || w.published().CanaryVersion() != commit("b") {
				t.Fatalf("a rollback the boxes never read: %v, out %+v", err, w.published())
			}
			w.lost = false
			if err := Rollback(context.Background(), w.config, w.steps(), &bytes.Buffer{}); err != nil {
				t.Fatalf("the canary couldn't be ended: %v", err)
			}
			if remote, _ := ParseManifest(w.remote); !Alone(remote, commit("a")) || !Alone(w.published(), commit("a")) {
				t.Fatalf("after the rollback: the boxes read %s, out %+v", w.remote, w.published())
			}
		})
	}
	t.Run("the read back fails", func(t *testing.T) {
		w := soaked(t)
		w.advance(5 * time.Minute)
		w.report("Cloud", commit("b"), commit("b"), serveUp)
		w.tick()
		w.advance(5*time.Minute + 30*time.Second)
		w.report("Cloud", commit("b"), commit("b"), serveUp)
		watcher := w.watcher()
		published := watcher.Steps.Published
		reads := 0
		watcher.Steps.Published = func(callContext context.Context) (Manifest, error) {
			if reads++; reads == 2 {
				return Manifest{}, errors.New("GET current.txt: 503")
			}
			return published(callContext)
		}
		if err := watcher.Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "reading back the promotion") {
			t.Fatalf("a failed read back: %v", err)
		}
		if state := w.state(); state.Phase != PhaseSoak || len(w.promoted) != 0 || w.published().CanaryVersion() != commit("b") {
			t.Fatalf("after a failed read back: %+v, promoted %q", state, w.promoted)
		}
		uploads := w.uploads.Load()
		w.advance(30 * time.Second)
		w.tick()
		if state := w.state(); state.Phase != PhaseFleet || len(w.promoted) != 1 || !Alone(w.published(), commit("b")) || w.uploads.Load() != uploads {
			t.Fatalf("the next pass: %+v, promoted %q, %d uploads (was %d)\n%s", state, w.promoted, w.uploads.Load(), uploads, w.log.String())
		}
	})
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
// step before the fleet holds publishing itself; every commit the release spans declares, not only its head, and an
// order git can't read releases nothing; a rule the watcher can't honor, or two commits that disagree, stop the release
// unpublished.
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
	t.Run("declared by a commit before the head", func(t *testing.T) {
		w := newWorld(t)
		w.head = commit("c") // b and c land in one pass
		w.orders[commit("b")] = "fleet after workers\n"
		w.orders[commit("c")] = "fleet after workers # c needs them too\nschema after fleet\n"
		w.tick()
		if state := w.state(); state.Phase != PhaseBefore || !reflect.DeepEqual(state.Order, Order{Before: []string{"workers"}, After: []string{"schema"}}) || w.publishes.Load() != 0 {
			t.Fatalf("%+v, %d published\n%s", state, w.publishes.Load(), w.log.String())
		}
	})
	t.Run("two commits that disagree", func(t *testing.T) {
		w := newWorld(t)
		w.head = commit("c")
		w.orders[commit("b")] = "workers after fleet\n"
		w.orders[commit("c")] = "fleet after workers\n"
		w.tick()
		if state := w.state(); state.Phase != PhaseStopped || !strings.Contains(state.Why, short(commit("b"))+" puts workers after the fleet, and "+short(commit("c"))+" puts it before") || w.publishes.Load() != 0 {
			t.Fatalf("%+v, %d published", state, w.publishes.Load())
		}
	})
	t.Run("an order git can't read", func(t *testing.T) {
		w := newWorld(t)
		w.head = commit("b")
		w.orderError = errors.New("git log: exit status 128")
		if err := w.watcher().Tick(context.Background()); err == nil || !strings.Contains(err.Error(), "reading the release-order") {
			t.Fatalf("a pass that couldn't read the order: %v", err)
		}
		if w.publishes.Load() != 0 || w.state().Phase != PhaseIdle {
			t.Fatalf("released with its order unread: %d published, %+v", w.publishes.Load(), w.state())
		}
		w.orderError = nil
		w.tick()
		if w.publishes.Load() != 1 {
			t.Fatalf("read, still %d published", w.publishes.Load())
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
	lock, err := Lock(w.config.Out)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := Rollback(context.Background(), w.config, w.steps(), &bytes.Buffer{}); !errors.Is(err, ErrHeld) {
		t.Fatalf("rolled back under the lock: %v", err)
	}
}
