package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/livestatus"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/treebuilder"
)

var updateTop = flag.Bool("update-top", false, "rewrite loom top's golden frames from what it draws now")

var topNow = time.Date(2026, 10, 10, 16, 4, 5, 0, time.UTC)

func ago(duration time.Duration) time.Time { return topNow.Add(-duration) }

// boxState is Cloud serving box-strict: a prebuilt unit in hand, testing, its runner's own status beside serve's, and
// the units it ran before.
func boxState() topState {
	serve := &livestatus.Status{Kind: livestatus.KindServe, Pid: 4242, Worker: "cloud-1a2b3c", Pool: "box-strict", StartedAt: ago(23 * time.Minute),
		UpdatedAt: ago(time.Second), AskedAt: ago(2*time.Minute + 14*time.Second),
		Unit: &livestatus.Unit{Run: "w-6f1c2a", Unit: "u-0042-lower", Package: "stage1/cohere/json", Packages: 3, Shard: "^(TestA|TestB)$",
			Phase: livestatus.PhaseOnRunner, Runner: "9b1d3c5e7f00", StartedAt: ago(2*time.Minute + 14*time.Second), Deadline: ago(2*time.Minute + 14*time.Second).Add(10 * time.Minute)},
		Totals: livestatus.Totals{Units: 14, Passed: 12, Failed: 1, Broken: 1, Fetched: 1288490188}}
	for index, verdict := range []string{"passed", "failed", "passed", "broken", "passed", "passed"} {
		serve.Recent = append(serve.Recent, livestatus.Recent{Run: "w-6f1c2a", Unit: fmt.Sprintf("u-%04d-shard-%c", 41-index, 'a'+index),
			Verdict: verdict, FinishedAt: ago(time.Duration(3+index*2) * time.Minute), Seconds: float64(62 + index*37), Fetched: int64(12<<20) * int64(index)})
	}
	running := &livestatus.Status{Kind: livestatus.KindUnit, Pid: 4250, Unit: &livestatus.Unit{Run: "w-6f1c2a", Unit: "u-0042-lower", Phase: livestatus.PhaseTesting,
		StartedAt: ago(2*time.Minute + 13*time.Second), Deadline: ago(2*time.Minute + 13*time.Second).Add(10 * time.Minute),
		Fetches: []livestatus.Fetch{{What: "index", From: "store", Count: 1, Bytes: 1198}, {What: "chunks", From: "store", Count: 812, Bytes: 429916160},
			{What: "binaries", From: "store", Count: 2, Bytes: 54525952}, {What: "products", From: "cache", Count: 4, Bytes: 8388608}, {What: "binaries", From: "cache", Count: 1, Bytes: 20971520}},
		Tests: livestatus.Tests{Passed: 1204, Failed: 2, Skipped: 31}}}
	return topState{Now: topNow, Host: "cloud",
		Machine: topMachine{Cpus: 16, Busy: 0.42, Load: [3]float64{3.1, 2.8, 2.4}, HasLoad: true, MemoryTotal: 64 << 30, MemoryAvailable: 40 << 30},
		Disks:   []topDisk{{Names: "serve", Free: 412 << 30, Total: 468 << 30, Floor: 3 << 30}},
		Caches:  topCaches{Read: true, BlobBytes: 2254857830, BlobBound: 4 << 30, Blobs: 830, Sources: 2, SourceBytes: 2040109465, Runners: 3},
		Units: []topUnit{{Name: "loom-serve", Active: "active", Sub: "running", Since: ago(23 * time.Minute)},
			{Name: "loom-update.timer", Active: "active", Sub: "waiting", Since: ago(26 * time.Hour)}},
		Serve: serve, ServeAlive: true, Running: running,
		Pools: []topPool{{Name: "box-strict", Status: poolStatus{Queued: 7, Workers: []poolWorker{
			{Worker: "cloud-1a2b3c", Cpus: 16, SeenAt: ago(2 * time.Minute).Format(time.RFC3339), Took: json.RawMessage(`"u-0042-lower"`)},
			{Worker: "server-77e01d", Cpus: 32, SeenAt: ago(41 * time.Second).Format(time.RFC3339), Took: json.RawMessage(`"u-0043-lower"`)},
			{Worker: "home-c0ffee", Cpus: 8, SeenAt: ago(9 * time.Second).Format(time.RFC3339), Took: json.RawMessage(`null`)}}}}},
		QueueNote: "serve's pool token doesn't read Queue; it shows on Workshop"}
}

// workshopState is Workshop: a tree building, its ledger, the daemons, three pools and Queue's line.
func workshopState() topState {
	state := boxState()
	state.Host = "workshop"
	state.Serve, state.Running, state.ServeNote = nil, nil, "no serve on this box (/home/kirk/loom-serve/root/loom-live/serve.json)"
	state.Caches = topCaches{}
	state.Disks = []topDisk{{Names: "trees", Free: 61 << 30, Total: 1863 << 30, Floor: 100 << 30}}
	state.Units = []topUnit{{Name: "loom-plan", Active: "active", Sub: "running", Since: ago(5 * time.Hour)}, {Name: "loom-place", Active: "active", Sub: "running", Since: ago(5 * time.Hour)},
		{Name: "loom-build-trees", Active: "active", Sub: "running", Since: ago(5 * time.Hour)}, {Name: "loom-judge", Active: "failed", Sub: "failed"},
		{Name: "loom-pusher.timer", Active: "active", Sub: "waiting", Since: ago(2 * 24 * time.Hour)}, {Name: "loom-update.timer", Active: "active", Sub: "waiting", Since: ago(26 * time.Hour)}}
	state.Tree = &livestatus.Status{Kind: livestatus.KindTree, Pid: 777, UpdatedAt: ago(time.Second), Tree: &livestatus.Tree{Key: strings.Repeat("ab12", 16),
		Future: strings.Repeat("9f8e", 10), Phase: "products", StartedAt: ago(4*time.Minute + 12*time.Second), Packages: 312, ProductTests: 52, ProductsHit: 40}}
	state.TreeAlive = true
	state.Builds = []treebuilder.Record{
		{Tree: strings.Repeat("cd34", 16), Future: strings.Repeat("1234", 10), At: ago(9 * time.Minute).Format(time.RFC3339), Event: treebuilder.Built, Seconds: 362},
		{Tree: strings.Repeat("ef56", 16), Future: strings.Repeat("5678", 10), At: ago(31 * time.Minute).Format(time.RFC3339), Event: treebuilder.Failed, Seconds: 95, Cause: "package stage2/x didn't compile"},
		{Tree: strings.Repeat("0a0b", 16), Future: strings.Repeat("9abc", 10), At: ago(55 * time.Minute).Format(time.RFC3339), Event: treebuilder.Built, Seconds: 401},
	}
	box := state.Pools[0]
	state.Pools = []topPool{box, {Name: "box-phase", Status: poolStatus{Workers: []poolWorker{{Worker: "chonchon-5e5e5e", Cpus: 24, SeenAt: ago(12 * time.Second).Format(time.RFC3339), Took: json.RawMessage(`"p-gofmt"`)}}}},
		{Name: "codex", Err: "https://runs.loom.system.inc/pools/codex answered 503 Service Unavailable"}}
	state.QueueNote = ""
	change := func(change, owner, sha, status string, since time.Duration, planned, passed, failed, void int) topChange {
		line := topChange{Change: change, Owner: owner, Sha: sha, State: status, StateSince: ago(since).Format(time.RFC3339)}
		line.Units.Planned, line.Units.Passed, line.Units.Failed, line.Units.Void = planned, passed, failed, void
		return line
	}
	state.Queue = &topQueue{Read: true, Seq: 18422, LandedMain: strings.Repeat("77aa", 10), Changes: []topChange{
		change("chg_01k7queued0000000000000000", "fabric", strings.Repeat("3c", 20), "queued", 2*time.Minute, 0, 0, 0, 0),
		change("chg_01k7landed0000000000000000", "release", strings.Repeat("4d", 20), "landed", 14*time.Minute, 120, 120, 0, 0),
		change("chg_01k7testing000000000000000", "kirk", strings.Repeat("1a", 20), "testing", 6*time.Minute, 240, 187, 1, 3),
		change("chg_01k7building00000000000000", "loom", strings.Repeat("2b", 20), "building", 4*time.Minute, 0, 0, 0, 0),
	}}
	return state
}

// golden compares a frame with testdata/<name>, or rewrites it under -update-top. Every row is at most width columns
// and there are exactly height of them.
func golden(t *testing.T, name, frame string, width, height int) {
	t.Helper()
	rows := strings.Split(frame, "\n")
	if len(rows) != height {
		t.Fatalf("%s: %d rows, want %d", name, len(rows), height)
	}
	for index, row := range rows {
		if columns := len([]rune(row)); columns > width {
			t.Fatalf("%s: row %d is %d columns, over %d: %q", name, index, columns, width, row)
		}
	}
	path := filepath.Join("testdata", name)
	if *updateTop {
		os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(path, []byte(frame+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (go test ./cmd/loom -run Top -update-top writes it)", err)
	}
	if string(want) != frame+"\n" {
		t.Fatalf("%s drew\n%s\nwant\n%s", name, frame, want)
	}
}

// At 80 by 24 a serving box shows its machine, the unit in hand (where it is against its deadline, what it fetched from
// where, its tests), what serve ran and the units before, and its pool and its workers; it grows with the terminal.
// Mutants: the optional rows given to no section; the runner's own status left out of the unit in hand.
func TestTopDrawsABoxAtEightyByTwentyFour(t *testing.T) {
	golden(t, "top-box-80x24.txt", drawTop(boxState(), 80, 24, false), 80, 24)
	golden(t, "top-box-132x40.txt", drawTop(boxState(), 132, 40, false), 132, 40)
}

// Workshop shows the tree it builds (phase, products hit and built) and the ones before, the daemons, every pool, and
// Queue's line, the changes on their way first; a pool that can't be read says why in its row, and a row too long for
// the terminal ends in an ellipsis at its edge. Mutants: the finished changes before the live ones; a failed unit drawn
// as running; a row not cut to the width.
func TestTopDrawsWorkshop(t *testing.T) {
	golden(t, "top-workshop-80x24.txt", drawTop(workshopState(), 80, 24, false), 80, 24)
	golden(t, "top-workshop-120x40.txt", drawTop(workshopState(), 120, 40, false), 120, 40)
}

var escapes = regexp.MustCompile("\033\\[[0-9;]*m")

// Color is only escapes around the same text: the colored frame, its escapes taken out, is the plain one, and every
// escape is one of the palette's or a reset. Mutant: a span's text dropped when colored.
func TestTopsColorsAreOnlyEscapes(t *testing.T) {
	for _, state := range []topState{boxState(), workshopState()} {
		colored := drawTop(state, 100, 30, true)
		if escapes.ReplaceAllString(colored, "") != drawTop(state, 100, 30, false) {
			t.Fatalf("colored and plain frames differ:\n%s", escapes.ReplaceAllString(colored, ""))
		}
		if !strings.Contains(colored, "\033[1;38;5;213m") || !strings.Contains(colored, "\033[0m") {
			t.Fatal("no heading color")
		}
	}
}

// A serve that is gone, one unfit, an idle one, a status that can't be read and no Workers token each draw a row that
// says so, and nothing else breaks. Mutant: a gone serve's last unit drawn as in hand.
func TestTopSaysWhatItCantShow(t *testing.T) {
	state := boxState()
	state.ServeAlive = false
	state.Serve.Stopped = "at the deadline"
	frame := drawTop(state, 80, 24, false)
	if !strings.Contains(frame, "stopped at the deadline, 1s ago") || strings.Contains(frame, "▶ u-0042-lower") {
		t.Fatalf("a gone serve:\n%s", frame)
	}
	state = boxState()
	state.Serve.Unit, state.Running, state.Serve.Unfit = nil, nil, "1200 MB free on /home, under 1500"
	if frame := drawTop(state, 80, 24, false); !strings.Contains(frame, " unfit: 1200 MB free on /home, under 1500") {
		t.Fatalf("an unfit serve:\n%s", frame)
	}
	state.Serve.Unfit = ""
	if frame := drawTop(state, 80, 24, false); !strings.Contains(frame, " idle, asking the pool") {
		t.Fatalf("an idle serve:\n%s", frame)
	}
	state = topState{Now: topNow, Host: "home", Machine: topMachine{Cpus: 8, Busy: -1}, ServeNote: "/x/serve.json isn't a status: unexpected end of JSON input",
		PoolsNote: "no token for the Workers on this box", QueueNote: "no token for the Workers on this box"}
	frame = drawTop(state, 80, 24, false)
	for _, want := range []string{"isn't a status", "no token for the Workers", " --%", "not read here"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("no %q in\n%s", want, frame)
		}
	}
}

// The reader takes a missing serve status as no serve, a cut-short one as its error, a dead writer as gone, and the
// runner's status only when it is the unit in hand's; the ledger's tail skips a line cut by its window and the starts.
// Mutants: the runner's status of another unit shown; a dead tree builder taken as alive; the ledger's starts read as
// builds.
func TestTopReadsStatusFilesTolerantly(t *testing.T) {
	directory := t.TempDir()
	reader := &topReader{serveRoot: filepath.Join(directory, "root"), treeCache: filepath.Join(directory, "trees"), treeLedger: filepath.Join(directory, "trees.jsonl")}
	state := topState{}
	reader.readStatuses(&state)
	if state.Serve != nil || !strings.HasPrefix(state.ServeNote, "no serve on this box") || state.Tree != nil || state.Builds != nil {
		t.Fatalf("nothing on disk: %+v", state)
	}
	servePath := livestatus.ServePath(reader.serveRoot)
	os.MkdirAll(filepath.Dir(servePath), 0o755)
	os.WriteFile(servePath, []byte(`{"kind":"serve","pid":`), 0o644)
	state = topState{}
	reader.readStatuses(&state)
	if state.Serve != nil || !strings.Contains(state.ServeNote, "isn't a status") {
		t.Fatalf("a cut-short status: %+v", state)
	}
	unit := &livestatus.Unit{Run: "r", Unit: "u", Phase: livestatus.PhaseOnRunner}
	livestatus.Write(servePath, livestatus.Status{Kind: livestatus.KindServe, Pid: os.Getpid(), Unit: unit})
	livestatus.Write(livestatus.UnitPath(reader.serveRoot), livestatus.Status{Kind: livestatus.KindUnit, Pid: os.Getpid(), Unit: &livestatus.Unit{Run: "r", Unit: "another", Phase: livestatus.PhaseTesting}})
	state = topState{}
	reader.readStatuses(&state)
	if state.Serve == nil || !state.ServeAlive || state.Running != nil {
		t.Fatalf("another unit's runner status: %+v", state)
	}
	livestatus.Write(livestatus.UnitPath(reader.serveRoot), livestatus.Status{Kind: livestatus.KindUnit, Pid: os.Getpid(), Unit: &livestatus.Unit{Run: "r", Unit: "u", Phase: livestatus.PhaseTesting}})
	livestatus.Write(livestatus.TreePath(reader.treeCache), livestatus.Status{Kind: livestatus.KindTree, Pid: 1 << 30, Tree: &livestatus.Tree{Phase: "warming"}})
	state = topState{}
	reader.readStatuses(&state)
	if state.Running == nil || state.Running.Unit.Phase != livestatus.PhaseTesting || state.Tree == nil || state.TreeAlive {
		t.Fatalf("the unit in hand's runner status, a dead tree builder: %+v", state)
	}
	records := []string{
		`{"tree":"cut","future":"f","at":"2026-10-10T15:00:00Z","event":"built"}`,
		`{"tree":"one","future":"f","at":"2026-10-10T15:01:00Z","event":"built","seconds":30}`,
		`{"tree":"two","future":"f","at":"2026-10-10T15:02:00Z","event":"started"}`,
		`{"tree":"two","future":"f","at":"2026-10-10T15:03:00Z","event":"failed","cause":"x"}`,
		`{"tree":"thr`,
	}
	content := strings.Join(records, "\n")
	os.WriteFile(reader.treeLedger, []byte(content), 0o644)
	window := int64(len(content) - len(records[0]) + 10)
	builds := readLedgerTail(reader.treeLedger, window)
	if len(builds) != 2 || builds[0].Tree != "two" || builds[0].Event != treebuilder.Failed || builds[1].Tree != "one" {
		t.Fatalf("the ledger's tail: %+v", builds)
	}
}

// Only a terminal is one: a file, a pipe or a buffer isn't, so `loom top > f`, `| cat` and ssh without a terminal draw
// one plain frame and exit instead of drawing forever. Mutant: a failed size ioctl taken as an 80 by 24 terminal.
func TestTopOffATerminalDrawsOnceAndExits(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("HOME", directory)
	file, _ := os.Create(filepath.Join(directory, "frame.txt"))
	defer file.Close()
	reading, writing, _ := os.Pipe()
	defer reading.Close()
	defer writing.Close()
	for name, output := range map[string]io.Writer{"a file": file, "a pipe": writing, "a buffer": &strings.Builder{}} {
		if _, isTerminal := openTerminal(output); isTerminal {
			t.Fatalf("%s taken as a terminal", name)
		}
	}
	exited := make(chan int, 1)
	go func() {
		exited <- top([]string{"--serve-root", filepath.Join(directory, "root"), "--trees", filepath.Join(directory, "trees"),
			"--ledger", filepath.Join(directory, "trees.jsonl"), "--wire", "http://127.0.0.1:9", "--queue", "http://127.0.0.1:9"}, file, io.Discard)
	}()
	select {
	case code := <-exited:
		frame, _ := os.ReadFile(file.Name())
		if code != 0 || strings.Count(string(frame), "\n") != 24 || strings.Contains(string(frame), "\033[") || !strings.Contains(string(frame), "no serve on this box") {
			t.Fatalf("exit %d, frame:\n%q", code, frame)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("loom top into a file never exited")
	}
}

// A background read that panics becomes the frame's note, and `loom top` goes on drawing with its terminal intact.
// Mutant: no recover.
func TestTopSurvivesAPanickingBackgroundRead(t *testing.T) {
	reader := &topReader{serveRoot: t.TempDir(), treeCache: t.TempDir(), treeLedger: filepath.Join(t.TempDir(), "none"), poolNames: []string{"box-strict"}}
	reader.safely(func() { panic("a pool answered something odd") })
	if state := reader.fast(topNow); !strings.Contains(state.PoolsNote, "panicked") || !strings.Contains(state.PoolsNote, "a pool answered something odd") {
		t.Fatalf("pools note %q", state.PoolsNote)
	}
}

// systemd's show output becomes the installed units, active or not, and since when; one not installed is left out.
// Mutant: a unit not installed shown.
func TestTopReadsTheUnits(t *testing.T) {
	output := "Id=loom-serve.service\nLoadState=loaded\nActiveState=active\nSubState=running\nActiveEnterTimestamp=Sat 2026-10-10 15:41:05 UTC\n\n" +
		"Id=loom-plan.service\nLoadState=not-found\nActiveState=inactive\nSubState=dead\nActiveEnterTimestamp=\n\n" +
		"Id=loom-update.timer\nLoadState=loaded\nActiveState=active\nSubState=waiting\nActiveEnterTimestamp=Fri 2026-10-09 14:00:00 UTC\n"
	units := parseUnits(output)
	if len(units) != 2 || units[0] != (topUnit{Name: "loom-serve", Active: "active", Sub: "running", Since: ago(23 * time.Minute)}) || units[1].Name != "loom-update.timer" {
		t.Fatalf("%+v", units)
	}
}

// The Workers are read with a board token minted from the secret (the pools, Queue's head and its board), or with
// serve's pool token for its pool alone; a refusal is the pool's row, and no error names the token. Mutants: serve's
// pool token sent to Queue; the token in an error.
func TestTopReadsTheWorkersWithATokenTheBoxHolds(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		token := strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer ")
		claims, err := protocol.VerifyToken(secret, token, time.Now())
		scope := "pool-token"
		if err == nil {
			scope = claims.Scope
		}
		seen = append(seen, scope+" "+request.URL.Path)
		switch {
		case scope == "pool-token" && request.URL.Path == "/pools/box-strict":
			writer.WriteHeader(http.StatusForbidden)
			writer.Write([]byte(`{"error":"a pool token can't do this"}`))
		case request.URL.Path == "/pools/box-strict":
			writer.Write([]byte(`{"queued":3,"workers":[{"worker":"cloud-1a2b3c","cpus":16,"seenAt":"2026-10-10T16:00:00Z","took":"u-1"}]}`))
		case request.URL.Path == "/head":
			writer.Write([]byte(`{"seq":7,"head":"h","landedMain":"abc","mainRed":null}`))
		case request.URL.Path == "/board/changes":
			writer.Write([]byte(`{"changes":[{"change":"chg_1","owner":"kirk","sha":"1a","state":"testing","units":{"planned":4,"passed":1,"failed":0,"void":0},"stateSince":"2026-10-10T16:00:00Z"}]}`))
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()
	board := &topReader{wire: server.URL, queue: server.URL, client: server.Client(), secret: secret, poolNames: []string{"box-strict"}}
	pools, queue := board.readWorkers(t.Context())
	if len(pools) != 1 || pools[0].Err != "" || pools[0].Status.Queued != 3 || queue == nil || !queue.Read || queue.Seq != 7 || queue.LandedMain != "abc" ||
		len(queue.Changes) != 1 || queue.Changes[0].Units.Planned != 4 {
		t.Fatalf("with a board token: %+v %+v", pools, queue)
	}
	seen = nil
	box := &topReader{wire: server.URL, queue: server.URL, client: server.Client(), poolToken: "the-pool-token-itself", poolNames: []string{"box-strict"}}
	pools, queue = box.readWorkers(t.Context())
	if queue != nil || len(seen) != 1 || seen[0] != "pool-token /pools/box-strict" || !strings.Contains(pools[0].Err, "refuses serve's pool token for the pool's status (403)") {
		t.Fatalf("with serve's pool token: %+v %v %v", pools, queue, seen)
	}
	// Any other refusal is the wire's own words, and never the token.
	box.poolNames = []string{"box-phase"}
	pools, _ = box.readWorkers(t.Context())
	if !strings.Contains(pools[0].Err, "/pools/box-phase answered 404 Not Found") || strings.Contains(pools[0].Err, "the-pool-token-itself") {
		t.Fatalf("a pool the wire lacks: %+v", pools)
	}
}
