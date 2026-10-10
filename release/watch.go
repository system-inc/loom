package release

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Phases a release passes through. Idle and done wait for a new commit; stopped waits for a person.
const (
	PhaseIdle    = "idle"
	PhaseBefore  = "before"  // waiting for the steps its order puts before the fleet to be marked done
	PhaseCanary  = "canary"  // published for the canary, waiting for it to report the release installed and healthy
	PhaseSoak    = "soak"    // the canary runs it; it must stay healthy for the soak
	PhaseFleet   = "fleet"   // promoted, waiting for every box to report it
	PhaseAfter   = "after"   // the fleet has it, waiting for the steps its order puts after the fleet
	PhaseDone    = "done"    // released
	PhaseStopped = "stopped" // a failure stopped it: nothing more is released until `loom release resume`
)

// A State is the watcher's release.json: the release in hand and where it is, kept across restarts, so a watcher
// restarted mid-release (every release's hook restarts it on Workshop) carries on where it was.
type State struct {
	Commit   string         `json:"commit,omitempty"`
	From     string         `json:"from,omitempty"` // the fleet's release when this one began
	Phase    string         `json:"phase"`
	Since    time.Time      `json:"since"` // when the phase began
	Order    Order          `json:"order"`
	Baseline map[string]int `json:"baseline,omitempty"` // the canary services' restarts when the soak began
	Lagging  []string       `json:"lagging,omitempty"`  // boxes that didn't report the release in time
	Held     []string       `json:"held,omitempty"`     // boxes a hold kept off it
	Why      string         `json:"why,omitempty"`      // why it stopped
	Failed   string         `json:"failed,omitempty"`   // the commit that stopped a release, never released again until retried
}

// Steps are what the watcher does to the world, each swapped in tests.
type Steps struct {
	Head     func(context.Context) (string, error)               // fetches the branch and names its head
	Descends func(context.Context, string, string) (bool, error) // whether the second commit descends from the first
	// Order is updater/release-order as each commit after the first, up to the second, that changes it left it, oldest
	// first: a file kept from an earlier release is that release's, never this one's. It fails rather than read none.
	Order   func(context.Context, string, string) ([]OrderAt, error)
	Publish func(context.Context, string, string) error // publish.sh, with --canary <host> when one is given
	Promote func(string) error                          // the commit's own manifest becomes current.txt
	// Upload is upload.sh, blobs first and current.txt last: <out>/current.txt, or the manifest file given.
	Upload func(context.Context, string) error
	// Published is the current.txt the boxes read (<base>/current.txt), fetched past any cache.
	Published func(context.Context) (Manifest, error)
	// PoolSeen is when the host's serve last asked any pool it serves; nil leaves the pools unread.
	PoolSeen func(context.Context, string) (time.Time, error)
}

// A Watcher releases each new commit on its branch, one at a time (docs/releases.md).
type Watcher struct {
	Config Config
	Steps  Steps
	Now    func() time.Time
	Log    io.Writer
}

// ErrHeld is a pass that found another release holding the lock.
var ErrHeld = errors.New("another release holds the lock")

// Lock takes <out>/.release.lock without waiting, so two watchers (or a watcher and a person's rollback, publish or
// upload) never publish at once: whoever holds it publishes, and anyone else's pass waits for the next. It returns the
// lock's file, whose Close releases it. An out directory that doesn't exist is refused, not made: the first release is
// published by hand (docs/updater.md).
func Lock(out string) (*os.File, error) {
	path := filepath.Join(out, ".release.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w", path, ErrHeld)
		}
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return file, nil
}

type heldKey struct{}

// Holding is a context that carries the held lock to the steps, which hand it down to publish.sh and upload.sh: they
// take the same lock themselves, and would otherwise find it held by the pass that runs them.
func Holding(callContext context.Context, lock *os.File) context.Context {
	return context.WithValue(callContext, heldKey{}, lock)
}

// held is the lock the context carries, or nil.
func held(callContext context.Context) *os.File {
	lock, _ := callContext.Value(heldKey{}).(*os.File)
	return lock
}

func (watcher *Watcher) say(format string, arguments ...any) {
	fmt.Fprintf(watcher.Log, "%s loom release: %s\n", watcher.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, arguments...))
}

// ReadState is the watcher's state, idle when it has none.
func ReadState(directory string) (State, error) {
	content, err := os.ReadFile(filepath.Join(directory, "release.json"))
	if errors.Is(err, os.ErrNotExist) {
		return State{Phase: PhaseIdle}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(content, &state); err != nil {
		return State{}, fmt.Errorf("%s: %w", filepath.Join(directory, "release.json"), err)
	}
	return state, nil
}

// WriteState keeps the state, whole or not at all.
func WriteState(directory string, state State) error {
	return writeJSON(filepath.Join(directory, "release.json"), state)
}

// ReadPublished is what the out directory last published: <out>/current.txt.
func ReadPublished(out string) (Manifest, error) {
	content, err := os.ReadFile(filepath.Join(out, "current.txt"))
	if err != nil {
		return Manifest{}, err
	}
	manifest, err := ParseManifest(string(content))
	if err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", filepath.Join(out, "current.txt"), err)
	}
	return manifest, nil
}

var markStep = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
var commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func markPath(directory, commit, step string) string {
	return filepath.Join(directory, "marks", commit+"."+step)
}

// Mark records that a step a release's order names is done for that commit: `loom release mark <commit> <step>`,
// which the step's own deploy runs (Worker deploys, #5tvjjx3) or a person does.
func Mark(directory, commit, step string) error {
	if !commitPattern.MatchString(commit) {
		return fmt.Errorf("%q isn't a commit's 40 hex digits", commit)
	}
	if !markStep.MatchString(step) {
		return fmt.Errorf("step %q isn't a lowercase name", step)
	}
	if err := os.MkdirAll(filepath.Join(directory, "marks"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(markPath(directory, commit, step), []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// Unmarked is the steps not yet marked done for the commit.
func Unmarked(directory, commit string, steps []string) []string {
	var left []string
	for _, step := range steps {
		if _, err := os.Stat(markPath(directory, commit, step)); err != nil {
			left = append(left, step)
		}
	}
	return left
}

func short(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// Tick is one pass: under the lock, it moves the release in hand on by at most one phase, or begins the branch's new
// head. A pass that finds the lock held changes nothing and returns ErrHeld.
func (watcher *Watcher) Tick(callContext context.Context) error {
	lock, err := Lock(watcher.Config.Out)
	if err != nil {
		return err
	}
	defer lock.Close()
	callContext = Holding(callContext, lock)
	state, err := ReadState(watcher.Config.State)
	if err != nil {
		return err
	}
	before := state
	switch state.Phase {
	case PhaseStopped:
		return nil
	case PhaseBefore:
		err = watcher.before(callContext, &state)
	case PhaseCanary:
		err = watcher.canary(&state)
	case PhaseSoak:
		err = watcher.soak(callContext, &state)
	case PhaseFleet:
		err = watcher.fleet(callContext, &state)
	case PhaseAfter:
		watcher.after(&state)
	default:
		err = watcher.begin(callContext, &state)
	}
	if !sameState(before, state) {
		if writeErr := WriteState(watcher.Config.State, state); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	return err
}

func sameState(left, right State) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

// stop ends the release loudly. The canary is left as it is, for inspection: Cloud keeps the release it was given and
// every other box the one before; `loom release rollback` returns Cloud, and `loom release resume` lets the next
// commit release (this one only with --retry).
func (watcher *Watcher) stop(state *State, why string) {
	state.Phase, state.Why, state.Failed, state.Since = PhaseStopped, why, state.Commit, watcher.Now().UTC()
	watcher.say("RELEASE STOPPED: %s: %s. Nothing more releases until `loom release resume`. %s keeps what it was given, for inspection; the other boxes stay on %s. `loom release rollback` returns %s to %s.",
		short(state.Commit), why, watcher.Config.Canary, short(state.From), watcher.Config.Canary, short(state.From))
}

// begin starts a release of the branch's head when it isn't what the fleet runs.
func (watcher *Watcher) begin(callContext context.Context, state *State) error {
	published, err := ReadPublished(watcher.Config.Out)
	if err != nil {
		return fmt.Errorf("reading what was published (the first release is published by hand, docs/updater.md): %w", err)
	}
	head, err := watcher.Steps.Head(callContext)
	if err != nil {
		return fmt.Errorf("reading %s's head: %w", watcher.Config.Branch, err)
	}
	if published.CanaryVersion() != "" {
		// A canary no release of this watcher's began (published by hand, or left by a stopped release since
		// resumed): two releases would be in flight, so nothing more is published until it is settled.
		failed := state.Failed
		*state = State{Commit: published.CanaryVersion(), From: published.Top(), Phase: state.Phase}
		watcher.stop(state, fmt.Sprintf("a canary of %s for %s is published, and no release here began it; settle it by hand (promote it, or `loom release rollback`)",
			short(published.CanaryVersion()), strings.Join(published.Canary, ", ")))
		state.Failed = failed // not this watcher's release, so not one it refuses to try
		return nil
	}
	if head == published.Top() || head == state.Failed {
		return nil
	}
	descends, err := watcher.Steps.Descends(callContext, published.Top(), head)
	if err != nil {
		return err
	}
	*state = State{Commit: head, From: published.Top(), Phase: PhaseIdle, Failed: state.Failed}
	if !descends {
		watcher.stop(state, fmt.Sprintf("%s's head doesn't descend from the release the fleet runs, %s", watcher.Config.Branch, short(published.Top())))
		return nil
	}
	// Every commit the release spans declares its order, not only the head: the merge train may land several at once.
	orders, err := watcher.Steps.Order(callContext, published.Top(), head)
	if err != nil {
		return fmt.Errorf("reading the release-order of %s..%s: %w", short(published.Top()), short(head), err)
	}
	if state.Order, err = MergeOrders(orders); err != nil {
		watcher.stop(state, err.Error())
		return nil
	}
	watcher.say("releasing %s (the fleet runs %s)", short(head), short(published.Top()))
	if len(state.Order.Before) > 0 {
		state.Phase, state.Since = PhaseBefore, watcher.Now().UTC()
		watcher.say("WAITING: %s's %s puts %s before the fleet; publishing waits until each is marked (`loom release mark %s <step>`)",
			short(head), OrderFile, strings.Join(state.Order.Before, ", "), head)
		return nil
	}
	return watcher.publishCanary(callContext, state)
}

// before waits for the steps the release's order puts before the fleet. If the branch moves on meanwhile, nothing was
// published, so the release in hand is dropped for the new head.
func (watcher *Watcher) before(callContext context.Context, state *State) error {
	if left := Unmarked(watcher.Config.State, state.Commit, state.Order.Before); len(left) > 0 {
		head, err := watcher.Steps.Head(callContext)
		if err == nil && head != state.Commit {
			watcher.say("%s moved to %s while %s waited on %s; releasing the new head instead", watcher.Config.Branch, short(head), short(state.Commit), strings.Join(left, ", "))
			*state = State{Phase: PhaseIdle, Failed: state.Failed}
		}
		return nil
	}
	watcher.say("%s's steps before the fleet (%s) are marked done", short(state.Commit), strings.Join(state.Order.Before, ", "))
	return watcher.publishCanary(callContext, state)
}

func (watcher *Watcher) publishCanary(callContext context.Context, state *State) error {
	if err := watcher.Steps.Publish(callContext, state.Commit, watcher.Config.Canary); err != nil {
		watcher.stop(state, "publishing the canary: "+err.Error())
		return nil
	}
	if err := watcher.Steps.Upload(callContext, ""); err != nil {
		watcher.stop(state, "uploading the canary: "+err.Error())
		return nil
	}
	state.Phase, state.Since = PhaseCanary, watcher.Now().UTC()
	watcher.say("canary %s published for %s; waiting up to %s for it to report the release installed, its hooks passed and %s active",
		short(state.Commit), watcher.Config.Canary, watcher.Config.CanaryWithin, strings.Join(watcher.Config.CanaryServices, ", "))
	return nil
}

// heard is the canary's report, when it arrived after the phase began: anything older says nothing of this release.
func (watcher *Watcher) heard(state *State, host string) (*Report, error) {
	report, err := ReadReport(filepath.Join(watcher.Config.State, "reports"), host)
	if err != nil || report == nil || !report.Received.After(state.Since) {
		return nil, err
	}
	return report, nil
}

func describe(report *Report) string {
	if report == nil {
		return "nothing since the phase began"
	}
	text := fmt.Sprintf("version %s, hooks passed for %s, services [%s], at %s", short(report.Version), short(report.Hooked), strings.Join(report.Services, "; "), report.Received.Format(time.RFC3339))
	if report.Refused != "" {
		text += ", refused: " + report.Refused
	}
	return text
}

// Alone is whether the manifest gives every box the commit, with no canary: a promotion, or a rollback's end.
func Alone(manifest Manifest, commit string) bool {
	return manifest.Top() == commit && manifest.CanaryVersion() == ""
}

// upload sends the commit's own manifest as current.txt, leaving the out directory's current.txt as it is.
func (watcher *Watcher) upload(callContext context.Context, commit string) error {
	if _, err := OwnManifest(watcher.Config.Out, commit); err != nil {
		return err
	}
	return watcher.Steps.Upload(callContext, ManifestPath(watcher.Config.Out, commit))
}

// describeManifest is a manifest in a few words: the version every box follows, and the canary's.
func describeManifest(manifest Manifest) string {
	text := short(manifest.Top()) + " for every box"
	if canary := manifest.CanaryVersion(); canary != "" {
		text += fmt.Sprintf(", canary %s for %s", short(canary), strings.Join(manifest.Canary, ", "))
	}
	return text
}

// canary waits for the canary to report the release installed, its hooks passed and its services active.
func (watcher *Watcher) canary(state *State) error {
	report, err := watcher.heard(state, watcher.Config.Canary)
	if err != nil {
		return err
	}
	if report != nil && report.Version == state.Commit && report.Hooked == state.Commit && len(report.Unhealthy(watcher.Config.CanaryServices)) == 0 {
		state.Phase, state.Since, state.Baseline = PhaseSoak, watcher.Now().UTC(), map[string]int{}
		for _, name := range watcher.Config.CanaryServices {
			service, _ := report.Service(name)
			state.Baseline[name] = service.Restarts
		}
		watcher.say("%s runs %s, hooks passed, %s active; soaking %s", watcher.Config.Canary, short(state.Commit), strings.Join(watcher.Config.CanaryServices, ", "), watcher.Config.Soak)
		return nil
	}
	if watcher.Now().Sub(state.Since) > watcher.Config.CanaryWithin {
		watcher.stop(state, fmt.Sprintf("%s didn't report %s installed, its hooks passed and %s active within %s; it reported %s",
			watcher.Config.Canary, short(state.Commit), strings.Join(watcher.Config.CanaryServices, ", "), watcher.Config.CanaryWithin, describe(report)))
	}
	return nil
}

// soak holds the canary to the release: still on it, its services restarting no more than allowed, and at the soak's
// end heard from since it began, every service active, and its serve asking its pool.
func (watcher *Watcher) soak(callContext context.Context, state *State) error {
	report, err := ReadReport(filepath.Join(watcher.Config.State, "reports"), watcher.Config.Canary)
	if err != nil {
		return err
	}
	canary := watcher.Config.Canary
	if report == nil || report.Version != state.Commit {
		watcher.stop(state, fmt.Sprintf("%s left %s during the soak: %s", canary, short(state.Commit), describe(report)))
		return nil
	}
	for _, name := range watcher.Config.CanaryServices {
		service, _ := report.Service(name)
		if baseline := state.Baseline[name]; service.Restarts >= 0 && baseline >= 0 && service.Restarts-baseline > watcher.Config.Restarts {
			watcher.stop(state, fmt.Sprintf("%s's %s restarted %d times during the soak, more than %d", canary, name, service.Restarts-baseline, watcher.Config.Restarts))
			return nil
		}
	}
	elapsed := watcher.Now().Sub(state.Since)
	if elapsed < watcher.Config.Soak {
		return nil
	}
	if !report.Received.After(state.Since) {
		watcher.stop(state, fmt.Sprintf("%s reported nothing during the %s soak (last at %s)", canary, watcher.Config.Soak, report.Received.Format(time.RFC3339)))
		return nil
	}
	if problems := report.Unhealthy(watcher.Config.CanaryServices); len(problems) > 0 {
		watcher.stop(state, fmt.Sprintf("%s ended the soak unhealthy: %s", canary, strings.Join(problems, ", ")))
		return nil
	}
	if watcher.Steps.PoolSeen != nil {
		seen, err := watcher.Steps.PoolSeen(callContext, canary)
		if err != nil || watcher.Now().Sub(seen) > 3*time.Minute {
			// The wire may lag or blink; the canary has five more minutes to be seen asking.
			if elapsed < watcher.Config.Soak+5*time.Minute {
				return nil
			}
			why := fmt.Sprintf("last asked at %s", seen.Format(time.RFC3339))
			if err != nil {
				why = err.Error()
			} else if seen.IsZero() {
				why = "no worker of it in " + strings.Join(watcher.Config.Pools, " or ")
			}
			watcher.stop(state, fmt.Sprintf("%s's serve isn't asking its pool: %s", canary, why))
			return nil
		}
	}
	// A person may have published by hand during the soak (a rollback is copying an older manifest over current.txt
	// and uploading it): what they published stands, and the canary isn't promoted over it.
	local, err := ReadPublished(watcher.Config.Out)
	if err != nil {
		return err
	}
	if local.CanaryVersion() != state.Commit {
		watcher.stop(state, fmt.Sprintf("%s/current.txt no longer publishes the canary of %s (it reads %s); someone published by hand during the soak, and nothing is promoted over it",
			watcher.Config.Out, short(state.Commit), describeManifest(local)))
		return nil
	}
	remote, err := watcher.Steps.Published(callContext)
	if err != nil {
		return fmt.Errorf("reading what the boxes read before promoting %s: %w", short(state.Commit), err)
	}
	// The commit alone there is this release's own promotion, uploaded by a pass that couldn't read it back.
	if !Alone(remote, state.Commit) {
		if remote.CanaryVersion() != state.Commit {
			watcher.stop(state, fmt.Sprintf("%s/current.txt, what the boxes read, no longer publishes the canary of %s (it reads %s); someone published by hand during the soak, and nothing is promoted over it",
				watcher.Config.Base, short(state.Commit), describeManifest(remote)))
			return nil
		}
		if err := watcher.upload(callContext, state.Commit); err != nil {
			watcher.stop(state, "uploading the promotion: "+err.Error())
			return nil
		}
		if remote, err = watcher.Steps.Published(callContext); err != nil {
			return fmt.Errorf("reading back the promotion of %s: %w", short(state.Commit), err)
		}
		if !Alone(remote, state.Commit) {
			watcher.stop(state, fmt.Sprintf("uploaded %s's manifest as current.txt, but %s/current.txt reads %s; %s/current.txt still publishes the canary",
				short(state.Commit), watcher.Config.Base, describeManifest(remote), watcher.Config.Out))
			return nil
		}
	}
	// Only once the boxes read the promotion does the out directory: until then it still says what they read, so a
	// failed upload leaves the canary for `loom release rollback` to end.
	if err := watcher.Steps.Promote(state.Commit); err != nil {
		watcher.stop(state, "promoting: "+err.Error())
		return nil
	}
	state.Phase, state.Since = PhaseFleet, watcher.Now().UTC()
	watcher.say("%s stayed healthy for %s; %s promoted to every box, waiting up to %s for each to report it", canary, watcher.Config.Soak, short(state.Commit), watcher.Config.FleetWithin)
	return nil
}

// fleet waits for every box but the canary to report the release with its hooks passed. A held box is skipped and
// named. A box that doesn't report in time is named loudly as lagging, but doesn't stop the release: a box that was
// off catches up when it returns, and status says it lags until then.
func (watcher *Watcher) fleet(callContext context.Context, state *State) error {
	var waiting, held []string
	for _, box := range watcher.Config.Boxes {
		if strings.EqualFold(box, watcher.Config.Canary) {
			continue
		}
		report, err := ReadReport(filepath.Join(watcher.Config.State, "reports"), box)
		if err != nil {
			return err
		}
		switch {
		case report != nil && report.Held != "":
			held = append(held, fmt.Sprintf("%s (held at %s)", box, report.Held))
		case report != nil && report.Received.After(state.Since) && report.Version == state.Commit && report.Hooked == state.Commit:
		default:
			waiting = append(waiting, box)
		}
	}
	if len(waiting) > 0 && watcher.Now().Sub(state.Since) <= watcher.Config.FleetWithin {
		return nil
	}
	state.Held, state.Lagging = held, waiting
	if len(held) > 0 {
		watcher.say("HELD: %s stay where their hold keeps them, off %s", strings.Join(held, ", "), short(state.Commit))
	}
	if len(waiting) > 0 {
		watcher.say("LAGGING: %s didn't report %s installed within %s; `loom release status` names what each last said", strings.Join(waiting, ", "), short(state.Commit), watcher.Config.FleetWithin)
	} else {
		watcher.say("every box runs %s", short(state.Commit))
	}
	state.Phase, state.Since = PhaseAfter, watcher.Now().UTC()
	if len(state.Order.After) > 0 {
		watcher.say("WAITING: %s's %s puts %s after the fleet: do each now, then `loom release mark %s <step>`; the next release waits on it",
			short(state.Commit), OrderFile, strings.Join(state.Order.After, ", "), state.Commit)
	}
	watcher.after(state)
	return nil
}

// after waits for the steps the release's order puts after the fleet, then the release is done.
func (watcher *Watcher) after(state *State) {
	if len(Unmarked(watcher.Config.State, state.Commit, state.Order.After)) > 0 {
		return
	}
	state.Phase, state.Since = PhaseDone, watcher.Now().UTC()
	watcher.say("released %s", short(state.Commit))
}

// Watch runs a pass every interval until the context ends. A pass's error is logged, never fatal: the next pass
// tries again.
func (watcher *Watcher) Watch(callContext context.Context) {
	for {
		if err := watcher.Tick(callContext); err != nil && !errors.Is(err, ErrHeld) {
			watcher.say("this pass: %v", err)
		}
		select {
		case <-callContext.Done():
			return
		case <-time.After(watcher.Config.Interval):
		}
	}
}

// Rollback ends a canary: the fleet's own release becomes current.txt again, alone, uploaded and read back where the
// boxes read it before the out directory says so, and the canary host returns to it (its previous version, still on disk). It refuses while a release is moving, and when there is no
// canary to end; rolling the whole fleet back is publishing an older manifest by hand (docs/updater.md).
func Rollback(callContext context.Context, config Config, steps Steps, log io.Writer) error {
	lock, err := Lock(config.Out)
	if err != nil {
		return err
	}
	defer lock.Close()
	callContext = Holding(callContext, lock)
	state, err := ReadState(config.State)
	if err != nil {
		return err
	}
	switch state.Phase {
	case PhaseBefore, PhaseCanary, PhaseSoak, PhaseFleet:
		return fmt.Errorf("a release of %s is in its %s phase; stop the watcher first (systemctl --user stop loom-release)", short(state.Commit), state.Phase)
	}
	published, err := ReadPublished(config.Out)
	if err != nil {
		return err
	}
	if published.CanaryVersion() == "" {
		return fmt.Errorf("no canary is published: every box follows %s (rolling the fleet back is publishing an older manifest, docs/updater.md)", short(published.Top()))
	}
	// As a promotion: uploaded and read back where the boxes read it before the out directory says so.
	if _, err := OwnManifest(config.Out, published.Top()); err != nil {
		return err
	}
	if err := steps.Upload(callContext, ManifestPath(config.Out, published.Top())); err != nil {
		return err
	}
	remote, err := steps.Published(callContext)
	if err != nil {
		return fmt.Errorf("uploaded %s's manifest, but reading it back: %w; %s/current.txt still publishes the canary, so rollback again", short(published.Top()), err, config.Out)
	}
	if !Alone(remote, published.Top()) {
		return fmt.Errorf("uploaded %s's manifest, but %s/current.txt reads %s; %s/current.txt still publishes the canary", short(published.Top()), config.Base, describeManifest(remote), config.Out)
	}
	if err := steps.Promote(published.Top()); err != nil {
		return err
	}
	fmt.Fprintf(log, "loom release: the canary of %s is ended: %s returns to %s\n", short(published.CanaryVersion()), strings.Join(published.Canary, ", "), short(published.Top()))
	return nil
}

// Resume lets a stopped watcher release again once the canary is settled: the next new head releases, but the commit
// that stopped it only with retry.
func Resume(config Config, retry bool, log io.Writer) error {
	lock, err := Lock(config.Out)
	if err != nil {
		return err
	}
	defer lock.Close()
	state, err := ReadState(config.State)
	if err != nil {
		return err
	}
	if state.Phase != PhaseStopped {
		return fmt.Errorf("the watcher isn't stopped (it is %s)", state.Phase)
	}
	if published, err := ReadPublished(config.Out); err != nil {
		return err
	} else if published.CanaryVersion() != "" {
		return fmt.Errorf("a canary of %s is still published for %s: promote it or `loom release rollback` first", short(published.CanaryVersion()), strings.Join(published.Canary, ", "))
	}
	failed := state.Failed
	if retry {
		failed = ""
	}
	if err := WriteState(config.State, State{Phase: PhaseIdle, Failed: failed}); err != nil {
		return err
	}
	if failed != "" {
		fmt.Fprintf(log, "loom release: resumed; %s stays unreleased until the branch moves past it (or resume --retry)\n", short(failed))
	} else {
		fmt.Fprintf(log, "loom release: resumed\n")
	}
	return nil
}
