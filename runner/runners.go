package runner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

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
}

// run runs a unit on the runner it names: this one when it names none or this one, else that runner, as a process of
// its own. had is false when the unit names a runner that couldn't be had (not in the store, not whole, or unable to
// start here); the unit is then refused here, broken and named, the coordinator's to place again.
func (cache runnerCache) run(runContext context.Context, unit protocol.Unit, options Options) (Result, bool) {
	named := ""
	if unit.Test != nil {
		named = unit.Test.Runner
	}
	if named == "" || named == selfSha256() {
		return Run(runContext, unit, options), true
	}
	binary, err := cache.path(runContext, named)
	if err != nil {
		return refuse(unit, options, protocol.PhaseFetch, fmt.Errorf("the runner %.12s the unit's key names can't be had: %w: Loom's, never the change's", named, err)), false
	}
	result, err := runOn(runContext, binary, unit, options)
	if err != nil {
		return refuse(unit, options, protocol.PhaseStart, fmt.Errorf("the runner %.12s the unit's key names can't run here: %w: Loom's, never the change's", named, err)), false
	}
	return result, true
}

// runOn runs the unit on another runner binary, `run` with this runner's own settings and the unit on its stdin, the
// way serve would run it: its events go to options.Events and to the wire the unit names, posted by that runner. Its
// exit says the status. A stopped serve stops it with SIGTERM, which breaks the unit there as here.
func runOn(runContext context.Context, binary string, unit protocol.Unit, options Options) (Result, error) {
	options = options.withDefaults()
	encoded, err := json.Marshal(unit)
	if err != nil {
		return Result{}, err
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
	command := exec.CommandContext(runContext, binary, append(arguments, "-")...)
	command.Stdin = bytes.NewReader(encoded)
	command.Stdout, command.Stderr = options.Events, options.Diagnostics
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	// Time for it to break the unit and post that: its kill grace and its wire's drain.
	command.WaitDelay = options.KillGrace + options.WireDrainTimeout + 30*time.Second
	err = command.Run()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		return Result{}, err
	}
	switch command.ProcessState.ExitCode() {
	case 0:
		return Result{Status: protocol.StatusPassed}, nil
	case 1:
		return Result{Status: protocol.StatusFailed}, nil
	}
	return Result{Status: protocol.StatusBroken}, nil
}

// path is the runner with this sha256, ready to run: kept here and checked again, or fetched from the release store,
// checked as it arrives, and made read-only and executable before it takes its name. The oldest beyond runnersKept go.
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
	requestContext, cancel := context.WithTimeout(fetchContext, 10*time.Minute)
	defer cancel()
	url := strings.TrimSuffix(cache.releases, "/") + "/" + sum
	request, err := http.NewRequestWithContext(requestContext, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	response, err := cache.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, response.Status)
	}
	partial, err := os.CreateTemp(cache.directory, ".partial-"+sum+"-")
	if err != nil {
		return "", err
	}
	defer os.Remove(partial.Name())
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(partial, hash), io.LimitReader(response.Body, runnerBytesLimit+1))
	if closeErr := partial.Close(); err == nil {
		err = closeErr
	}
	switch {
	case err != nil:
		return "", fmt.Errorf("GET %s: %w", url, err)
	case written > runnerBytesLimit:
		return "", fmt.Errorf("GET %s: over %d bytes", url, runnerBytesLimit)
	case hex.EncodeToString(hash.Sum(nil)) != sum:
		return "", fmt.Errorf("GET %s: its bytes hash to %s", url, hex.EncodeToString(hash.Sum(nil)))
	}
	if err := os.Chmod(partial.Name(), 0o555); err != nil {
		return "", err
	}
	if err := os.Rename(partial.Name(), path); err != nil {
		return "", err
	}
	cache.evict(sum)
	return path, nil
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
