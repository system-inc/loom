package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner"
	"github.com/system-inc/loom/serving"
	"github.com/system-inc/loom/treebuilder"
)

// A topState is everything one frame of `loom top` draws, read from this box and the Workers. A part that couldn't be
// read says why in its note, and the frame draws without it.
type topState struct {
	Now     time.Time
	Host    string
	Machine topMachine
	Disks   []topDisk
	Caches  topCaches
	Units   []topUnit
	// Serve is serve's live status, ServeAlive whether serve still runs, and Running the runner's status of the unit in
	// hand, when it is that unit's.
	Serve      *livestatus.Status
	ServeAlive bool
	ServeNote  string
	Running    *livestatus.Status
	// Tree is the tree builder's live status, Builds its ledger's newest records, newest first.
	Tree      *livestatus.Status
	TreeAlive bool
	Builds    []treebuilder.Record
	Pools     []topPool
	PoolsNote string
	Queue     *topQueue
	QueueNote string
}

type topMachine struct {
	Cpus int
	// Busy is the share of the CPUs busy since the last frame, below zero when unknown (the first frame, or no /proc).
	Busy            float64
	Load            [3]float64
	HasLoad         bool
	MemoryTotal     uint64
	MemoryAvailable uint64
}

// A topDisk is one filesystem Loom writes, by what it holds, with the floor under which Loom stops writing it.
type topDisk struct {
	Names string
	Free  uint64
	Total uint64
	Floor uint64
}

// topCaches are serve's root's kept things: the blob cache against its bound, the unpacked sources and the runners.
type topCaches struct {
	Read        bool
	BlobBytes   int64
	BlobBound   int64
	Blobs       int
	Sources     int
	SourceBytes int64
	Runners     int
}

type topUnit struct {
	Name   string
	Active string
	Sub    string
	Since  time.Time
}

type topPool struct {
	Name   string
	Status poolStatus
	Err    string
}

// A topQueue is Queue's head and its board of changes, each change's line.
type topQueue struct {
	// Read says the head was read; Err, when it wasn't, says why, or why the board wasn't.
	Read       bool
	Seq        int
	LandedMain string
	MainRed    bool
	Changes    []topChange
	Err        string
}

type topChange struct {
	Change string `json:"change"`
	Owner  string `json:"owner"`
	Sha    string `json:"sha"`
	State  string `json:"state"`
	Units  struct {
		Planned int `json:"planned"`
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Void    int `json:"void"`
	} `json:"units"`
	StateSince string  `json:"stateSince"`
	FinishedAt *string `json:"finishedAt"`
}

// topUnits are the systemd user units Loom runs on a box, shown when they are installed.
var topUnits = []string{"loom-serve.service", "loom-update.timer", "loom-plan.service", "loom-place.service", "loom-build-trees.service",
	"loom-judge.service", "loom-pusher.timer"}

// A topReader reads the state. Its fast part (the status files, the machine, the disks) is read every frame; its
// slow part (the caches' sizes, systemd, the Workers) in the background, each at its own pace.
type topReader struct {
	serveRoot  string
	treeCache  string
	treeLedger string
	wire       string
	queue      string
	client     *http.Client
	// secret mints a board token for each read, which watches and changes nothing; with none, poolToken is serve's own
	// pool token, which reads only its pool. Neither is ever printed.
	secret    []byte
	poolToken string
	poolNames []string
	tokenNote string
	// systemctl runs `systemctl --user` with its arguments; nil when the box has none.
	systemctl func(arguments ...string) (string, error)

	cpu     cpuSample
	mutex   sync.Mutex
	caches  topCaches
	units   []topUnit
	pools   []topPool
	queueAt *topQueue
}

// newTopReader finds what this box holds: serve's root and settings, the tree builder's cache and ledger, and the
// token the Workers take, the token secret (Workshop, the Mac) before serve's pool token.
func newTopReader(home, serveRoot, treeCache, treeLedger, wire, queue string) *topReader {
	reader := &topReader{serveRoot: serveRoot, treeCache: treeCache, treeLedger: treeLedger, wire: wire, queue: queue,
		client: &http.Client{Timeout: 10 * time.Second}}
	if content, err := os.ReadFile(filepath.Join(home, ".loom", "pools.json")); err == nil {
		if pools, err := judge.LoadPools(content); err == nil {
			for _, pool := range pools {
				reader.poolNames = append(reader.poolNames, pool.Name)
			}
		}
	}
	paths := serving.HomePaths(home)
	servePool := ""
	if content, err := os.ReadFile(paths.Config); err == nil {
		if config, err := serving.ReadConfig(string(content)); err == nil {
			servePool = config.Pool
			if !slices.Contains(reader.poolNames, servePool) {
				reader.poolNames = append(reader.poolNames, servePool)
			}
		}
	}
	if secret, err := protocol.ReadTokenSecret(filepath.Join(home, ".loom", "token-secret")); err == nil {
		reader.secret = secret
	} else if token, err := os.ReadFile(paths.Token); err == nil && servePool != "" && strings.TrimSpace(string(token)) != "" {
		reader.poolToken, reader.poolNames = strings.TrimSpace(string(token)), []string{servePool}
	} else {
		reader.tokenNote = "no token for the Workers on this box (~/.loom/token-secret or serve's pool token)"
	}
	if path, err := exec.LookPath("systemctl"); err == nil && runtime.GOOS == "linux" {
		reader.systemctl = func(arguments ...string) (string, error) {
			output, err := exec.Command(path, append([]string{"--user"}, arguments...)...).Output()
			return string(output), err
		}
	}
	return reader
}

// fast reads the part of the state read every frame, beside the slow part as last read.
func (reader *topReader) fast(now time.Time) topState {
	state := topState{Now: now}
	state.Host, _ = os.Hostname()
	state.Host, _, _ = strings.Cut(state.Host, ".")
	state.Machine = reader.readMachine()
	state.Disks = readDisks(reader.diskPaths())
	reader.readStatuses(&state)
	reader.mutex.Lock()
	state.Caches, state.Units = reader.caches, reader.units
	state.Pools, state.Queue = reader.pools, reader.queueAt
	reader.mutex.Unlock()
	switch {
	case reader.tokenNote != "":
		state.PoolsNote, state.QueueNote = reader.tokenNote, reader.tokenNote
	case len(reader.poolNames) == 0:
		state.PoolsNote = "no pools named on this box (~/.loom/pools.json or serve.conf)"
	}
	if reader.secret == nil && reader.tokenNote == "" {
		state.QueueNote = "serve's pool token doesn't read Queue; it shows on Workshop"
	}
	return state
}

// readStatuses reads serve's, its runner's and the tree builder's live status and the tree builder's ledger. A status
// that is missing, cut short or not a status is a note, never a stop.
func (reader *topReader) readStatuses(state *topState) {
	if serve, err := livestatus.Read(livestatus.ServePath(reader.serveRoot)); err == nil {
		state.Serve, state.ServeAlive = &serve, livestatus.Alive(serve.Pid)
		if running, err := livestatus.Read(livestatus.UnitPath(reader.serveRoot)); err == nil && running.Unit != nil && serve.Unit != nil &&
			running.Unit.Run == serve.Unit.Run && running.Unit.Unit == serve.Unit.Unit {
			state.Running = &running
		}
	} else if errors.Is(err, fs.ErrNotExist) {
		state.ServeNote = "no serve on this box (" + livestatus.ServePath(reader.serveRoot) + ")"
	} else {
		state.ServeNote = err.Error()
	}
	if tree, err := livestatus.Read(livestatus.TreePath(reader.treeCache)); err == nil && tree.Tree != nil {
		state.Tree, state.TreeAlive = &tree, livestatus.Alive(tree.Pid)
	}
	state.Builds = readLedgerTail(reader.treeLedger, 64<<10)
}

// readLedgerTail is the tree builder's newest records but its starts, newest first, from the ledger's last bytes; a
// line cut by the window or a crash is skipped.
func readLedgerTail(path string, window int64) []treebuilder.Record {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil
	}
	offset := max(0, info.Size()-window)
	content, err := io.ReadAll(io.NewSectionReader(file, offset, info.Size()-offset))
	if err != nil {
		return nil
	}
	lines := bytes.Split(content, []byte("\n"))
	if offset > 0 && len(lines) > 0 {
		lines = lines[1:]
	}
	var records []treebuilder.Record
	for index := len(lines) - 1; index >= 0; index-- {
		var record treebuilder.Record
		if json.Unmarshal(lines[index], &record) != nil || record.Tree == "" || record.Event == treebuilder.Started {
			continue
		}
		records = append(records, record)
	}
	return records
}

// diskPaths are the filesystems Loom writes on this box that exist, each named, with its floor: serve's root under the
// prebuilt floor, the tree builder's cache under its own.
func (reader *topReader) diskPaths() []diskPath {
	var paths []diskPath
	if _, err := os.Stat(reader.serveRoot); err == nil {
		paths = append(paths, diskPath{name: "serve", path: reader.serveRoot, floor: runner.DefaultFreeFloorBytes})
	}
	if _, err := os.Stat(reader.treeCache); err == nil {
		paths = append(paths, diskPath{name: "trees", path: reader.treeCache, floor: 100 << 30})
	}
	if len(paths) == 0 {
		home, _ := os.UserHomeDir()
		paths = append(paths, diskPath{name: "home", path: home})
	}
	return paths
}

type diskPath struct {
	name  string
	path  string
	floor uint64
}

// readDisks reads each path's filesystem once, by its id: two paths on one filesystem are one disk, under the higher
// floor.
func readDisks(paths []diskPath) []topDisk {
	var disks []topDisk
	seen := map[uint64]int{}
	for _, path := range paths {
		var stat syscall.Statfs_t
		info, err := os.Stat(path.path)
		if err != nil || syscall.Statfs(path.path, &stat) != nil {
			continue
		}
		device, _ := info.Sys().(*syscall.Stat_t)
		if device == nil {
			continue
		}
		id := uint64(device.Dev)
		if index, found := seen[id]; found {
			disks[index].Names += ", " + path.name
			disks[index].Floor = max(disks[index].Floor, path.floor)
			continue
		}
		seen[id] = len(disks)
		disks = append(disks, topDisk{Names: path.name, Free: stat.Bavail * uint64(stat.Bsize), Total: stat.Blocks * uint64(stat.Bsize), Floor: path.floor})
	}
	return disks
}

// A cpuSample is /proc/stat's first line: the CPUs' busy and total ticks since boot.
type cpuSample struct {
	busy, total uint64
}

// readMachine reads the CPUs' load since the last frame, the load averages and memory from /proc where there is one.
func (reader *topReader) readMachine() topMachine {
	machine := topMachine{Cpus: runtime.NumCPU(), Busy: -1}
	if content, err := os.ReadFile("/proc/stat"); err == nil {
		line, _, _ := strings.Cut(string(content), "\n")
		fields := strings.Fields(line)
		var sample cpuSample
		for index, field := range fields[min(1, len(fields)):] {
			value, _ := strconv.ParseUint(field, 10, 64)
			sample.total += value
			// idle and iowait are the fourth and fifth.
			if index != 3 && index != 4 {
				sample.busy += value
			}
		}
		if reader.cpu.total > 0 && sample.total > reader.cpu.total {
			machine.Busy = float64(sample.busy-reader.cpu.busy) / float64(sample.total-reader.cpu.total)
		}
		reader.cpu = sample
	}
	if content, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(content))
		if len(fields) >= 3 {
			for index := range 3 {
				machine.Load[index], _ = strconv.ParseFloat(fields[index], 64)
			}
			machine.HasLoad = true
		}
	}
	if file, err := os.Open("/proc/meminfo"); err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			fields := strings.Fields(scanner.Text())
			if len(fields) < 2 {
				continue
			}
			kilobytes, _ := strconv.ParseUint(fields[1], 10, 64)
			switch fields[0] {
			case "MemTotal:":
				machine.MemoryTotal = kilobytes << 10
			case "MemAvailable:":
				machine.MemoryAvailable = kilobytes << 10
			}
		}
		file.Close()
	}
	return machine
}

// readCaches sizes serve's root's blob cache, unpacked sources and kept runners. The sources are tens of thousands of
// files, so this is read in the background, never every frame.
func readCaches(root string) topCaches {
	caches := topCaches{BlobBound: runner.DefaultBlobCacheBytes}
	size := func(directory string) (int64, int) {
		var total int64
		files := 0
		filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() {
				return nil
			}
			if info, err := entry.Info(); err == nil {
				total += info.Size()
				files++
			}
			return nil
		})
		return total, files
	}
	if _, err := os.Stat(root); err != nil {
		return caches
	}
	caches.Read = true
	caches.BlobBytes, caches.Blobs = size(filepath.Join(root, "loom-blobs"))
	if entries, err := os.ReadDir(filepath.Join(root, "loom-sources")); err == nil {
		for _, entry := range entries {
			if entry.IsDir() && protocol.Sha256Pattern.MatchString(entry.Name()) {
				caches.Sources++
				bytes, _ := size(filepath.Join(root, "loom-sources", entry.Name()))
				caches.SourceBytes += bytes
			}
		}
	}
	if entries, err := os.ReadDir(filepath.Join(root, "loom-runners")); err == nil {
		for _, entry := range entries {
			if protocol.Sha256Pattern.MatchString(entry.Name()) {
				caches.Runners++
			}
		}
	}
	return caches
}

// readUnits is each of Loom's systemd user units that is installed: active or not, its sub-state, and since when.
func readUnits(systemctl func(arguments ...string) (string, error)) []topUnit {
	if systemctl == nil {
		return nil
	}
	arguments := []string{"show", "--property=Id,LoadState,ActiveState,SubState,ActiveEnterTimestamp"}
	output, err := systemctl(append(arguments, topUnits...)...)
	if err != nil && output == "" {
		return nil
	}
	return parseUnits(output)
}

// parseUnits reads `systemctl show`'s blocks, one per unit, a blank line between, leaving out a unit not installed.
func parseUnits(output string) []topUnit {
	var units []topUnit
	for _, block := range strings.Split(strings.TrimSpace(output), "\n\n") {
		properties := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			key, value, _ := strings.Cut(line, "=")
			properties[key] = value
		}
		if properties["Id"] == "" || properties["LoadState"] != "loaded" {
			continue
		}
		unit := topUnit{Name: strings.TrimSuffix(properties["Id"], ".service"), Active: properties["ActiveState"], Sub: properties["SubState"]}
		// systemd's own form: "Sat 2026-10-10 16:00:00 UTC".
		if since, err := time.Parse("Mon 2006-01-02 15:04:05 MST", properties["ActiveEnterTimestamp"]); err == nil {
			unit.Since = since
		}
		units = append(units, unit)
	}
	return units
}

// bearer is a token for one read of the Workers: a board token minted for a minute, or serve's pool token.
func (reader *topReader) bearer() (string, error) {
	if reader.secret != nil {
		return protocol.MintToken(reader.secret, protocol.TokenClaims{Run: protocol.BoardRun, Scope: protocol.ScopeBoard, Expires: time.Now().Add(time.Minute).Unix()})
	}
	return reader.poolToken, nil
}

// get reads one JSON answer from the Workers. An error names the address and the answer, never the token.
func (reader *topReader) get(readContext context.Context, url string, answer any) error {
	token, err := reader.bearer()
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(readContext, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := reader.client.Do(request)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, errors.Unwrap(err))
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if response.StatusCode != http.StatusOK {
		var refusal struct {
			Error string `json:"error"`
		}
		json.Unmarshal(body, &refusal)
		return fmt.Errorf("%s answered %s %s", url, response.Status, refusal.Error)
	}
	return json.Unmarshal(body, answer)
}

// readWorkers reads every pool's status and, with a board token, Queue's head and board.
func (reader *topReader) readWorkers(readContext context.Context) ([]topPool, *topQueue) {
	if reader.tokenNote != "" {
		return nil, nil
	}
	pools := make([]topPool, len(reader.poolNames))
	for index, name := range reader.poolNames {
		pools[index].Name = name
		err := reader.get(readContext, strings.TrimSuffix(reader.wire, "/")+"/pools/"+name, &pools[index].Status)
		switch {
		case err != nil && reader.secret == nil && strings.Contains(err.Error(), " 403 "):
			// Today's wire takes a pool token for its pool's next alone (docs/protocol.md, The pool).
			pools[index].Err = "the wire refuses serve's pool token for the pool's status (403): it reads only on Workshop"
		case err != nil:
			pools[index].Err = err.Error()
		}
	}
	if reader.secret == nil {
		return pools, nil
	}
	queue := &topQueue{}
	var head struct {
		Seq        int     `json:"seq"`
		LandedMain *string `json:"landedMain"`
		MainRed    any     `json:"mainRed"`
	}
	base := strings.TrimSuffix(reader.queue, "/")
	if err := reader.get(readContext, base+"/head", &head); err != nil {
		queue.Err = err.Error()
		return pools, queue
	}
	queue.Read, queue.Seq, queue.MainRed = true, head.Seq, head.MainRed != nil
	if head.LandedMain != nil {
		queue.LandedMain = *head.LandedMain
	}
	var board struct {
		Changes []topChange `json:"changes"`
	}
	if err := reader.get(readContext, base+"/board/changes", &board); err != nil {
		queue.Err = err.Error()
	}
	queue.Changes = board.Changes
	return pools, queue
}

// slow reads the slow part now: the caches, the units and the Workers.
func (reader *topReader) slow(readContext context.Context, caches, units, workers bool) {
	if caches {
		read := readCaches(reader.serveRoot)
		reader.mutex.Lock()
		reader.caches = read
		reader.mutex.Unlock()
	}
	if units {
		read := readUnits(reader.systemctl)
		reader.mutex.Lock()
		reader.units = read
		reader.mutex.Unlock()
	}
	if workers {
		pools, queue := reader.readWorkers(readContext)
		reader.mutex.Lock()
		reader.pools, reader.queueAt = pools, queue
		reader.mutex.Unlock()
	}
}

// background keeps the slow part fresh until the context ends: the units and the Workers every 5 s, the caches every
// 15 s.
func (reader *topReader) background(readContext context.Context) {
	for tick := 0; readContext.Err() == nil; tick++ {
		reader.slow(readContext, tick%15 == 0, tick%5 == 0, tick%5 == 0)
		select {
		case <-readContext.Done():
		case <-time.After(time.Second):
		}
	}
}
