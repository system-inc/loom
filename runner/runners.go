package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/system-inc/loom/housecache"
	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/protocol"
)

// DefaultReleases is where every Loom release's binaries are kept by their sha256, never expired (docs/updater.md).
const DefaultReleases = "https://artifacts.loom.system.inc/releases/blobs/"

// runnerDirectoryName holds the runners a serving runner fetched, under its root.
const runnerDirectoryName = "loom-runners"

// runnersKept bounds that directory: the runners a pool's units name are few (the pin, and the one before it while a
// pin moves), and each is about 20 MB.
const runnersKept = 4

// runnerBytesLimit bounds a runner fetch, so a store answering with something else can't fill the disk.
const runnerBytesLimit = 512 << 20

// A runnerCache is a serving runner's runners by sha256 (docs/serving.md). A unit's test job names the runner its key
// names; the box's own runner, the one its updater installed, only serves: it asks the pool and hands each unit to the
// runner the unit names, fetched once from the release store, checked by its sha256, and kept here. So the pin moves
// only when someone means it, never with every release a box installs.
type runnerCache struct {
	directory string
	releases  string
	client    *http.Client
	// house is the house cache's address, asked first through houseClient; empty when there is none.
	house       string
	houseClient *http.Client
	// check says whether a binary is a loom-runner for this platform; nil means checkRunnerBinary.
	check func(path string) error
	// live is serve's live status, which says when a unit waits on its runner's fetch; nil writes nothing.
	live *livestatus.Writer
}

// runnerFetchBound bounds a runner's fetch, and a quarter of the unit's own time bounds it further, so a slow store
// never spends the unit's deadline.
const runnerFetchBound = 5 * time.Minute

// loomRunnerPath is the main package every loom-runner is built from.
const loomRunnerPath = "github.com/system-inc/loom/runner/cmd/loom-runner"

// checkRunnerBinary refuses a binary that isn't a loom-runner built for this platform, by the build information Go
// writes into every binary: the release store holds every release's `loom` too, and both platforms' builds, and a
// unit naming one of those must never be run here as a runner.
func checkRunnerBinary(path string) error {
	information, err := buildinfo.ReadFile(path)
	if err != nil {
		return fmt.Errorf("it isn't a Go binary: %w", err)
	}
	if information.Path != loomRunnerPath {
		return fmt.Errorf("it is %s, not loom-runner", information.Path)
	}
	settings := map[string]string{}
	for _, setting := range information.Settings {
		settings[setting.Key] = setting.Value
	}
	if settings["GOOS"] != runtime.GOOS || settings["GOARCH"] != runtime.GOARCH {
		return fmt.Errorf("it is built for %s/%s, and this machine is %s/%s", settings["GOOS"], settings["GOARCH"], runtime.GOOS, runtime.GOARCH)
	}
	return nil
}

// run runs a unit on the runner it names: this one when it names none or this one, else that runner, as a process of
// its own. Before fetching that runner it starts the unit itself (started, naming this machine, then a line saying what
// it fetches, and one every heartbeat while it does), so the coordinator holds the unit as taken instead of queuing it
// again, and the runner continues the unit's stream from there. had is false when the unit names a runner that couldn't
// be had (not in the store, not whole, not a loom-runner for this platform, or never starting the unit, as a release
// from before this hand-off doesn't); the unit is then finished here, broken and named, the coordinator's to place
// again, and serve counts it toward its backoff.
func (cache runnerCache) run(runContext context.Context, unit protocol.Unit, options Options) (Result, bool) {
	named := ""
	if unit.Test != nil {
		named = unit.Test.Runner
	}
	if named == "" || named == selfSha256() {
		cache.live.Update(func(status *livestatus.Status) {
			if status.Unit != nil {
				status.Unit.Phase, status.Unit.Runner = livestatus.PhaseOnRunner, shortSum(selfSha256())
			}
		})
		return Run(runContext, unit, options), true
	}
	run := begin(unit, options)
	// The named runner keeps the unit's own live status from here; serve's says it fetches that runner, then runs on it.
	run.live.Close()
	run.live = nil
	cache.live.Update(func(status *livestatus.Status) {
		if status.Unit != nil {
			status.Unit.Phase, status.Unit.Runner = livestatus.PhaseFetchingRunner, shortSum(named)
		}
	})
	broken := func(phase string, err error) (Result, bool) {
		run.fail(phase, err)
		run.finish(protocol.StatusBroken)
		return Result{Status: protocol.StatusBroken}, false
	}
	run.say(fmt.Sprintf("the unit's key names runner %.12s; fetching it from the release store", named))
	bound := runnerFetchBound
	if quarter := time.Duration(unit.TimeoutSeconds) * time.Second / 4; quarter > 0 && quarter < bound {
		bound = quarter
	}
	fetchContext, cancel := context.WithTimeout(runContext, bound)
	beating, beaten := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(beaten)
		started := time.Now()
		ticker := time.NewTicker(run.options.Heartbeat / 4)
		defer ticker.Stop()
		for {
			select {
			case <-beating:
				return
			case <-ticker.C:
				run.emitter.beat(run.options.Heartbeat, fmt.Sprintf("loom-runner: still fetching runner %.12s after %.0f s", named, time.Since(started).Seconds()))
			}
		}
	}()
	binary, err := cache.path(fetchContext, named)
	// The beat has stopped before anything after it: the named runner continues the stream from this one's next
	// sequence, so a late beat here would take a sequence it uses.
	close(beating)
	<-beaten
	cancel()
	if err != nil {
		return broken(protocol.PhaseFetch, fmt.Errorf("the runner %.12s the unit's key names can't be had within %v: %w: Loom's, never the change's", named, bound, err))
	}
	check := cache.check
	if check == nil {
		check = checkRunnerBinary
	}
	if err := check(binary); err != nil {
		return broken(protocol.PhaseFetch, fmt.Errorf("the blob %.12s the unit's key names isn't a runner to run here: %v: Loom's, never the change's", named, err))
	}
	// What this runner said lands before the named runner continues the stream from the next sequence.
	if wire := run.emitter.wire; wire != nil {
		wire.Drain(time.Now().Add(run.options.WireDrainTimeout))
	}
	unit.SequenceStart = run.emitter.next()
	cache.live.Update(func(status *livestatus.Status) {
		if status.Unit != nil {
			status.Unit.Phase = livestatus.PhaseOnRunner
		}
	})
	result, started, err := runOn(runContext, binary, unit, options)
	switch {
	case err != nil:
		return broken(protocol.PhaseStart, fmt.Errorf("the runner %.12s the unit's key names can't run here: %w: Loom's, never the change's", named, err))
	case !started:
		return broken(protocol.PhaseStart, fmt.Errorf("the runner %.12s the unit's key names exited %s without starting the unit (a release from before serve's hand-off takes none of its settings): Loom's, never the change's", named, result.Status))
	}
	return result, true
}

// startWatcher passes a runner's event lines through and notes whether one was its unit's started event.
type startWatcher struct {
	writer  io.Writer
	partial []byte
	started bool
}

func (watcher *startWatcher) Write(content []byte) (int, error) {
	watcher.partial = append(watcher.partial, content...)
	for {
		end := bytes.IndexByte(watcher.partial, '\n')
		if end < 0 {
			break
		}
		var event protocol.Event
		if json.Unmarshal(watcher.partial[:end], &event) == nil && event.Type == "started" {
			watcher.started = true
		}
		watcher.partial = watcher.partial[end+1:]
	}
	return watcher.writer.Write(content)
}

// runOn runs the unit on another runner binary, `run` with this runner's own settings and the unit on its stdin, the
// way serve would run it: its events go to options.Events and to the wire the unit names, posted by that runner. Its
// exit says the status, and started whether it began the unit at all. A stopped serve stops it with SIGTERM, which
// breaks the unit there as here.
func runOn(runContext context.Context, binary string, unit protocol.Unit, options Options) (Result, bool, error) {
	options = options.withDefaults()
	encoded, err := json.Marshal(unit)
	if err != nil {
		return Result{}, false, err
	}
	arguments := []string{"run", "--workspace", options.WorkspaceParent}
	if options.Strict {
		arguments = append(arguments, "--strict")
	}
	if options.PhaseJobs {
		arguments = append(arguments, "--phase-jobs")
	}
	if options.Exclusive {
		arguments = append(arguments, "--exclusive")
	}
	if options.Root != "" {
		arguments = append(arguments, "--root", options.Root)
	}
	if options.Tree != "" {
		arguments = append(arguments, "--tree", options.Tree)
	}
	if options.Machine != "" {
		arguments = append(arguments, "--machine", options.Machine)
	}
	command := exec.CommandContext(runContext, binary, append(arguments, "-")...)
	if options.HouseCache != "" {
		// By the environment, not a flag: a pinned runner from before the house cache refuses a flag it doesn't know,
		// and ignores a variable.
		command.Env = append(os.Environ(), housecache.Variable+"="+options.HouseCache)
	}
	command.Stdin = bytes.NewReader(encoded)
	watcher := &startWatcher{writer: options.Events}
	command.Stdout, command.Stderr = watcher, options.Diagnostics
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	// Time for it to break the unit and post that: its kill grace and its wire's drain.
	command.WaitDelay = options.KillGrace + options.WireDrainTimeout + 30*time.Second
	err = command.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return Result{}, false, err
	}
	switch code := command.ProcessState.ExitCode(); code {
	case 0:
		return Result{Status: protocol.StatusPassed}, watcher.started, nil
	case 1:
		return Result{Status: protocol.StatusFailed}, watcher.started, nil
	default:
		return Result{Status: fmt.Sprintf("%d", code)}, watcher.started, nil
	}
}

// path is the runner with this sha256, ready to run: kept here and checked again, or fetched (from the house cache when
// there is one and it gives it whole, otherwise from the release store), checked as it arrives, and made read-only and
// executable before it takes its name. The oldest beyond runnersKept go.
func (cache runnerCache) path(fetchContext context.Context, sum string) (string, error) {
	if !protocol.Sha256Pattern.MatchString(sum) {
		return "", fmt.Errorf("%q isn't a sha256", sum)
	}
	path := filepath.Join(cache.directory, sum)
	if file, err := os.Open(path); err == nil {
		hash := sha256.New()
		_, err := io.Copy(hash, file)
		file.Close()
		if err == nil && hex.EncodeToString(hash.Sum(nil)) == sum {
			now := time.Now()
			os.Chtimes(path, now, now)
			return path, nil
		}
		os.Remove(path)
	}
	if err := os.MkdirAll(cache.directory, 0o755); err != nil {
		return "", err
	}
	url := strings.TrimSuffix(cache.releases, "/") + "/" + sum
	err := errors.New("no house cache")
	if through := housecache.Through(cache.house, url); through != "" {
		// Its own context, cut once its answer stalls or trickles, so the house cache never spends the fetch's bound.
		houseContext, cancelHouse := context.WithCancel(fetchContext)
		err = cache.download(houseContext, cache.houseClient, through, sum, path, cancelHouse)
		cancelHouse()
		var status houseStatus
		if err != nil && !errors.As(err, &status) {
			housecache.Unanswered(cache.house)
		}
	}
	if err != nil && fetchContext.Err() == nil {
		err = cache.download(fetchContext, cache.client, url, sum, path, nil)
	}
	if err != nil {
		return "", err
	}
	cache.evict(sum)
	return path, nil
}

// houseStatus is an answer that came whole and wasn't the runner: a status, or bytes that don't hash to its name.
type houseStatus struct{ error }

func (status houseStatus) Unwrap() error { return status.error }

// download reads runner sum from url into a partial file, and renames it to path, read-only and executable, only when
// it is whole and hashes to sum. With watch, the cancel of fetchContext, the body is cut off once it stalls or
// trickles (housecache.Watch).
func (cache runnerCache) download(fetchContext context.Context, client *http.Client, url, sum, path string, watch context.CancelFunc) error {
	request, err := http.NewRequestWithContext(fetchContext, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	body := response.Body
	if watch != nil {
		body = housecache.Watch(body, watch)
	}
	defer body.Close()
	if response.StatusCode != http.StatusOK {
		return houseStatus{fmt.Errorf("GET %s: %s", url, response.Status)}
	}
	partial, err := os.CreateTemp(cache.directory, ".partial-"+sum+"-")
	if err != nil {
		return err
	}
	defer os.Remove(partial.Name())
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(partial, hash), io.LimitReader(body, runnerBytesLimit+1))
	if closeErr := partial.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		return fmt.Errorf("GET %s: %w", url, err)
	case written > runnerBytesLimit:
		return fmt.Errorf("GET %s: over %d bytes", url, runnerBytesLimit)
	case hex.EncodeToString(hash.Sum(nil)) != sum:
		return houseStatus{fmt.Errorf("GET %s: its bytes hash to %s", url, hex.EncodeToString(hash.Sum(nil)))}
	}
	if err := os.Chmod(partial.Name(), 0o555); err != nil {
		return err
	}
	return os.Rename(partial.Name(), path)
}

// evict removes the least recently used runners beyond runnersKept, never the one just readied. A runner that is
// running stays whole: its process holds the file, whatever happens to its name.
func (cache runnerCache) evict(keep string) {
	entries, err := os.ReadDir(cache.directory)
	if err != nil {
		return
	}
	type kept struct {
		name string
		used time.Time
	}
	var runners []kept
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !protocol.Sha256Pattern.MatchString(entry.Name()) || entry.Name() == keep {
			continue
		}
		runners = append(runners, kept{entry.Name(), info.ModTime()})
	}
	slices.SortFunc(runners, func(left, right kept) int { return right.used.Compare(left.used) })
	for index := runnersKept - 1; index < len(runners); index++ {
		os.Remove(filepath.Join(cache.directory, runners[index].name))
	}
}
