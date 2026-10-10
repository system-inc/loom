package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// A unit given to `loom-runner run` by hand on another runner than the one its key names is refused before anything
// runs: the judge would void whatever this runner computed for it. A serving runner never does this, it hands the unit
// to the runner it names (runners.go). The runner the key names, or none, runs as before.
func TestAJobNamingAnotherRunnerIsRefusedAsUnfitBeforeAnythingRuns(t *testing.T) {
	fixture := newStrictFixture(t, 0)
	job := goodTestJob()
	job.Runner = strings.Repeat("e", 64)
	result, events, _ := runUnit(t, testJobUnit(job), fixture.options(t))
	if result.Status != protocol.StatusBroken ||
		!strings.Contains(errorPhases(events), "start: refused as unfit: the job's key names runner eeeeeeeeeeee, and this runner is "+selfSha256()[:12]) {
		t.Fatalf("%s, errors %q", result.Status, errorPhases(events))
	}
	if fixture.exists("prepared") || result.Workspace != "" {
		t.Fatal("something ran before the refusal")
	}
	for name, runner := range map[string]string{"its own": selfSha256(), "none": ""} {
		job.Runner = runner
		result, events, _ := runUnit(t, testJobUnit(job), fixture.options(t))
		if strings.Contains(errorPhases(events), "refused as unfit") || !fixture.exists("prepared") {
			t.Fatalf("a job naming %s runner: %s, errors %q", name, result.Status, errorPhases(events))
		}
	}
}

// A release store of runners by sha256, counting what it is asked for.
type releaseStore struct {
	mutex  sync.Mutex
	blobs  map[string][]byte
	gets   int
	server *httptest.Server
}

func newReleaseStore(t *testing.T) *releaseStore {
	store := &releaseStore{blobs: map[string][]byte{}}
	store.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		store.mutex.Lock()
		defer store.mutex.Unlock()
		store.gets++
		content, found := store.blobs[strings.TrimPrefix(request.URL.Path, "/releases/blobs/")]
		if !found {
			http.NotFound(writer, request)
			return
		}
		writer.Write(content)
	}))
	t.Cleanup(store.server.Close)
	return store
}

// put keeps content under its own sha256, which it returns.
func (store *releaseStore) put(content []byte) string {
	sum := sha256.Sum256(content)
	name := hex.EncodeToString(sum[:])
	store.mutex.Lock()
	defer store.mutex.Unlock()
	store.blobs[name] = content
	return name
}

func (store *releaseStore) url() string { return store.server.URL + "/releases/blobs/" }

// standIn is a runner that records how it was run and the unit it was given, says it started (unless starts is false,
// as a release from before the hand-off, which refuses serve's flags, never does), and exits with code.
func standIn(directory string, starts bool, code int) []byte {
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + directory + "/arguments'\ncat > '" + directory + "/unit.json'\n"
	if starts {
		script += "echo '{\"type\":\"started\"}'\n"
	}
	return []byte(script + "exit " + strconv.Itoa(code) + "\n")
}

// poolJob is a pool unit carrying a test job that names runner.
func (pool *testPool) job(id string, runner string) protocol.Unit {
	job := goodTestJob()
	job.Runner = runner
	unit := testJobUnit(job)
	unit.Unit = id
	unit.Wire = &protocol.Endpoint{Url: pool.server.URL + "/runs/r-test/events"}
	return unit
}

// takeAnyRunner stands in for the check that a fetched blob is a loom-runner for this platform, for a stand-in script.
func takeAnyRunner(string) error { return nil }

// The box's own runner only serves: a unit naming another runner runs on that one, fetched once from the release store
// by its sha256, kept read-only under the root, and run with this runner's settings and the unit on its stdin. Its exit
// is the unit's status. So a release a box installs moves no pin: the units keep running on the runner their keys name.
// Before it fetches, serve starts the unit itself, naming the worker as the machine, and the runner continues the
// stream from the sequence after serve's.
func TestServeRunsAUnitOnTheRunnerItNames(t *testing.T) {
	directory := t.TempDir()
	store := newReleaseStore(t)
	named := store.put(standIn(directory, true, 1))
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.job("first", named), pool.job("second", named)}
	options := pool.serveOptions(t, time.Now().Add(time.Second+1500*time.Millisecond), time.Second, io.Discard)
	options.Unit.Strict, options.Unit.Root, options.Releases, options.checkRunner = true, filepath.Join(directory, "root"), store.url(), takeAnyRunner
	summary, err := Serve(context.Background(), options)
	if err != nil || summary.Units != 2 || summary.Failed != 2 {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
	arguments, _ := os.ReadFile(filepath.Join(directory, "arguments"))
	want := strings.Join([]string{"run", "--workspace", options.Unit.WorkspaceParent, "--strict", "--root", options.Unit.Root, "--machine", "codex-1", "-"}, "\n") + "\n"
	if string(arguments) != want {
		t.Fatalf("the named runner was run with %q, not %q", arguments, want)
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	events := pool.events["second"]
	if len(events) != 2 || events[0].Type != "started" || events[0].Machine != "codex-1" || events[0].RunnerSha256 != selfSha256() ||
		!strings.Contains(events[1].Text, "the unit's key names runner "+named[:12]+"; fetching it") {
		t.Fatalf("serve's own events for the unit: %+v", events)
	}
	var given protocol.Unit
	expected := pool.job("second", named)
	expected.SequenceStart = 2
	if content, _ := os.ReadFile(filepath.Join(directory, "unit.json")); protocol.Decode(bytes.NewReader(content), &given) != nil || !reflect.DeepEqual(given, expected) {
		t.Fatalf("the named runner was given %+v", given)
	}
	kept, err := os.Stat(filepath.Join(options.Unit.Root, runnerDirectoryName, named))
	if err != nil || kept.Mode().Perm() != 0o555 {
		t.Fatalf("the runner kept under the root: %v, %v", kept, err)
	}
	store.mutex.Lock()
	defer store.mutex.Unlock()
	if store.gets != 1 {
		t.Fatalf("%d fetches of the runner", store.gets)
	}
}

// A runner of a release from before the hand-off refuses serve's flags and exits 2 having posted nothing. That is the
// unit's void, named by serve on the stream it started (never an ordinary broken unit that, posting nothing, the
// coordinator would queue again every two minutes forever), and it counts toward the backoff: five in a row take four.
func TestARunnerThatNeverStartsTheUnitIsItsVoidNamed(t *testing.T) {
	store := newReleaseStore(t)
	silent := store.put(standIn(t.TempDir(), false, 2))
	pool := newTestPool(t)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		pool.queue = append(pool.queue, pool.job(id, silent))
	}
	options := pool.serveOptions(t, time.Now().Add(time.Second+2*time.Second), time.Second, io.Discard)
	options.Unit.Root, options.Releases, options.checkRunner = t.TempDir(), store.url(), takeAnyRunner
	summary, err := Serve(context.Background(), options)
	if err != nil || summary.Units != 4 || summary.Broken != 4 {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	events := pool.events["1"]
	if len(events) == 0 || events[len(events)-1].Status != protocol.StatusBroken ||
		!strings.Contains(errorPhases(events), "start: the runner "+silent[:12]+" the unit's key names exited 2 without starting the unit") {
		t.Fatalf("the void's stream: %+v", events)
	}
}

// Serve holds a unit it is fetching a runner for: a slow store gets a line on the unit's stream every heartbeat, so the
// coordinator never takes it for a worker gone, and the fetch is bounded by a quarter of the unit's time, so a store
// that never answers is the unit's void in that time, never its deadline.
func TestServeHoldsTheUnitWhileItFetchesAndBoundsTheFetch(t *testing.T) {
	directory := t.TempDir()
	content := standIn(directory, true, 0)
	sum := sha256.Sum256(content)
	named := hex.EncodeToString(sum[:])
	delay := 600 * time.Millisecond
	store := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		select {
		case <-time.After(delay):
		case <-request.Context().Done():
			return
		}
		writer.Write(content)
	}))
	defer store.Close()
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.job("slow", named)}
	options := pool.serveOptions(t, time.Now().Add(time.Second+1500*time.Millisecond), time.Second, io.Discard)
	options.Unit.Root, options.Releases, options.checkRunner = t.TempDir(), store.URL+"/", takeAnyRunner
	options.Unit.Heartbeat = 100 * time.Millisecond
	if summary, err := Serve(context.Background(), options); err != nil || summary.Passed != 1 {
		t.Fatalf("a slow store: summary %+v, err %v", summary, err)
	}
	pool.mutex.Lock()
	if lines := outputLines(pool.events["slow"], "runner"); !strings.Contains(strings.Join(lines, "\n"), "still fetching runner "+named[:12]) {
		t.Fatalf("no heartbeat while fetching: %q", lines)
	}
	pool.mutex.Unlock()
	// A unit of two seconds may wait half a second for its runner; this store answers after five.
	delay = 5 * time.Second
	pool = newTestPool(t)
	unit := pool.job("stalled", named)
	unit.TimeoutSeconds = 2
	pool.queue = []protocol.Unit{unit}
	options = pool.serveOptions(t, time.Now().Add(time.Second+1500*time.Millisecond), time.Second, io.Discard)
	options.Unit.Root, options.Releases, options.checkRunner = t.TempDir(), store.URL+"/", takeAnyRunner
	started := time.Now()
	if summary, err := Serve(context.Background(), options); err != nil || summary.Broken != 1 || time.Since(started) > 4*time.Second {
		t.Fatalf("a stalled store: summary %+v, err %v after %v", summary, err, time.Since(started))
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if !strings.Contains(errorPhases(pool.events["stalled"]), "can't be had within 500ms") {
		t.Fatalf("the stalled fetch's void: %q", errorPhases(pool.events["stalled"]))
	}
}

// Every unit serve runs itself names the worker the pool shows as its machine, so the board ties the two.
func TestServesStartedEventsNameItsWorker(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("here", "true")}
	options := pool.serveOptions(t, time.Now().Add(time.Second+500*time.Millisecond), time.Second, io.Discard)
	if summary, err := Serve(context.Background(), options); err != nil || summary.Passed != 1 {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if events := pool.events["here"]; len(events) == 0 || events[0].Machine != "codex-1" {
		t.Fatalf("started names the machine %+v, not the worker codex-1", events)
	}
}

// Only a loom-runner built for this machine's platform is run: the release store holds every release's `loom` and
// both platforms' builds too, and a unit naming one of those is refused before anything runs it.
func TestOnlyALoomRunnerForThisPlatformIsRun(t *testing.T) {
	if testing.Short() {
		t.Skip("builds loom-runner twice")
	}
	build := func(goos, goarch string) string {
		binary := filepath.Join(t.TempDir(), "loom-runner")
		command := exec.Command("go", "build", "-o", binary, loomRunnerPath)
		command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("building loom-runner for %s/%s: %v %s", goos, goarch, err, output)
		}
		return binary
	}
	if err := checkRunnerBinary(build(runtime.GOOS, runtime.GOARCH)); err != nil {
		t.Fatalf("this platform's loom-runner refused: %v", err)
	}
	other := "linux"
	if runtime.GOOS == "linux" {
		other = "darwin"
	}
	if err := checkRunnerBinary(build(other, "amd64")); err == nil || !strings.Contains(err.Error(), "built for "+other+"/amd64") {
		t.Fatalf("another platform's loom-runner: %v", err)
	}
	test, _ := os.Executable()
	if err := checkRunnerBinary(test); err == nil || !strings.Contains(err.Error(), "not loom-runner") {
		t.Fatalf("another Go program: %v", err)
	}
	script := filepath.Join(t.TempDir(), "runner")
	os.WriteFile(script, standIn(t.TempDir(), true, 0), 0o755)
	if err := checkRunnerBinary(script); err == nil || !strings.Contains(err.Error(), "isn't a Go binary") {
		t.Fatalf("a script: %v", err)
	}
	// Served, a blob that isn't a runner here is the unit's void, named.
	store := newReleaseStore(t)
	named := store.put([]byte("#!/bin/sh\nexit 0\n"))
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.job("script", named)}
	options := pool.serveOptions(t, time.Now().Add(time.Second+500*time.Millisecond), time.Second, io.Discard)
	options.Unit.Root, options.Releases = t.TempDir(), store.url()
	if summary, err := Serve(context.Background(), options); err != nil || summary.Broken != 1 {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
	pool.mutex.Lock()
	defer pool.mutex.Unlock()
	if !strings.Contains(errorPhases(pool.events["script"]), "fetch: the blob "+named[:12]+" the unit's key names isn't a runner to run here: it isn't a Go binary") {
		t.Fatalf("the script's void: %q", errorPhases(pool.events["script"]))
	}
}

// A runner the store lacks, or gives with bytes that aren't its hash, is the unit's void, named: broken, its stream
// posted by the serving runner itself, nothing kept. After two such units in a row serve waits before it asks again,
// longer each time, and a unit it can run starts that over, so a box never idles while runnable units are queued.
func TestARunnerThatCantBeHadIsTheUnitsVoidNamed(t *testing.T) {
	store := newReleaseStore(t)
	corrupt := strings.Repeat("c", 64)
	store.blobs[corrupt] = []byte("not this runner")
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.job("first", corrupt)}
	options := pool.serveOptions(t, time.Now().Add(time.Second+500*time.Millisecond), time.Second, io.Discard)
	options.Unit.Root, options.Releases = t.TempDir(), store.url()
	if summary, err := Serve(context.Background(), options); err != nil || summary.Units != 1 || summary.Broken != 1 {
		t.Fatalf("summary %+v, err %v", summary, err)
	}
	pool.mutex.Lock()
	events := pool.events["first"]
	pool.mutex.Unlock()
	if len(events) < 3 || events[0].Type != "started" || events[0].RunnerSha256 != selfSha256() || events[len(events)-1].Status != protocol.StatusBroken ||
		!strings.Contains(errorPhases(events), "fetch: the runner cccccccccccc the unit's key names can't be had within ") || !strings.Contains(errorPhases(events), "its bytes hash to ") {
		t.Fatalf("the void's stream: %+v", events)
	}
	if entries, _ := os.ReadDir(filepath.Join(options.Unit.Root, runnerDirectoryName)); len(entries) != 0 {
		t.Fatalf("kept %d files of a runner that wasn't whole", len(entries))
	}
	// Five in a row the store lacks: the third waits a second and the fourth two, past the deadline, so four are taken.
	missing := strings.Repeat("d", 64)
	pool = newTestPool(t)
	for _, id := range []string{"1", "2", "3", "4", "5"} {
		pool.queue = append(pool.queue, pool.job(id, missing))
	}
	options = pool.serveOptions(t, time.Now().Add(time.Second+2*time.Second), time.Second, io.Discard)
	options.Unit.Root, options.Releases = t.TempDir(), store.url()
	if summary, err := Serve(context.Background(), options); err != nil || summary.Units != 4 {
		t.Fatalf("five missing runners in a row: summary %+v, err %v", summary, err)
	}
	// Two missing, one it can run, two missing: never a wait, so all five are taken in well under a second.
	pool = newTestPool(t)
	pool.queue = []protocol.Unit{pool.job("1", missing), pool.job("2", missing), pool.unit("3", "true"), pool.job("4", missing), pool.job("5", missing)}
	options = pool.serveOptions(t, time.Now().Add(time.Second+600*time.Millisecond), time.Second, io.Discard)
	options.Unit.Root, options.Releases = t.TempDir(), store.url()
	if summary, err := Serve(context.Background(), options); err != nil || summary.Units != 5 {
		t.Fatalf("with a runnable unit between: summary %+v, err %v", summary, err)
	}
}

// The kept runners are bounded, least recently used first, and one whose bytes changed on disk is fetched again.
func TestKeptRunnersAreBoundedAndCheckedAgain(t *testing.T) {
	store := newReleaseStore(t)
	cache := runnerCache{directory: t.TempDir(), releases: store.url(), client: http.DefaultClient}
	var names []string
	for index := range runnersKept + 2 {
		name := store.put([]byte("runner " + strconv.Itoa(index)))
		names = append(names, name)
		if _, err := cache.path(context.Background(), name); err != nil {
			t.Fatal(err)
		}
		past := time.Now().Add(time.Duration(index-10) * time.Minute)
		os.Chtimes(filepath.Join(cache.directory, name), past, past)
	}
	entries, _ := os.ReadDir(cache.directory)
	if len(entries) != runnersKept {
		t.Fatalf("kept %d runners, bound %d", len(entries), runnersKept)
	}
	for _, name := range names[:2] {
		if _, err := os.Stat(filepath.Join(cache.directory, name)); err == nil {
			t.Fatalf("kept the oldest runner %.12s", name)
		}
	}
	last := filepath.Join(cache.directory, names[len(names)-1])
	os.Chmod(last, 0o644)
	os.WriteFile(last, []byte("changed"), 0o644)
	before := store.gets
	if path, err := cache.path(context.Background(), names[len(names)-1]); err != nil || store.gets != before+1 {
		t.Fatalf("a changed runner: %s, %v, %d fetches", path, err, store.gets-before)
	}
	if content, _ := os.ReadFile(last); string(content) != "runner "+strconv.Itoa(len(names)-1) {
		t.Fatalf("the runner fetched again holds %q", content)
	}
}

// A drain is how a release restarts a box's serve (loom-serve's reload): nothing more is asked for, the unit in hand
// runs to its finish, never broken, and serve ends long before its deadline. Idle, it ends at once.
func TestADrainLetsTheUnitInHandFinishAndEnds(t *testing.T) {
	pool := newTestPool(t)
	pool.queue = []protocol.Unit{pool.unit("long", "sleep 1; echo long"), pool.unit("next", "true")}
	drain := make(chan struct{})
	time.AfterFunc(300*time.Millisecond, func() { close(drain) })
	options := pool.serveOptions(t, time.Now().Add(time.Hour), time.Minute, io.Discard)
	options.Drain = drain
	started := time.Now()
	summary, err := Serve(backstop(t, 15*time.Second), options)
	if err != nil || summary.Units != 1 || summary.Passed != 1 || summary.Broken != 0 || summary.Stopped != "on a drain" || time.Since(started) > 10*time.Second {
		t.Fatalf("summary %+v, err %v after %v", summary, err, time.Since(started))
	}
	pool.mutex.Lock()
	if events := pool.events["long"]; len(events) == 0 || events[len(events)-1].Status != protocol.StatusPassed || len(pool.queue) != 1 {
		t.Fatalf("the wire has %+v for the unit in hand, %d queued", events, len(pool.queue))
	}
	pool.mutex.Unlock()
	// Idle, or standing down on a full disk, a drain ends the wait at once.
	for name, free := range map[string]int64{"idle": 1 << 20, "standing down": 10} {
		pool := newTestPool(t)
		drain := make(chan struct{})
		time.AfterFunc(300*time.Millisecond, func() { close(drain) })
		options := pool.serveOptions(t, time.Now().Add(time.Hour), time.Minute, io.Discard)
		options.Drain, options.UnfitPause = drain, time.Hour
		options.freeMegabytes = func(string) (int64, error) { return free, nil }
		started := time.Now()
		if summary, err := Serve(backstop(t, 10*time.Second), options); err != nil || summary.Stopped != "on a drain" || time.Since(started) > 5*time.Second {
			t.Fatalf("%s: summary %+v, err %v after %v", name, summary, err, time.Since(started))
		}
	}
}

// backstop ends a serve that never drains, so a broken drain fails its test rather than hanging it for an hour.
func backstop(t *testing.T, after time.Duration) context.Context {
	backstopContext, cancel := context.WithTimeout(context.Background(), after)
	t.Cleanup(cancel)
	return backstopContext
}
