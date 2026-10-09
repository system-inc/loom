package runner

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

const testToken = "test-run-token"

func hashOf(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

// A testStore is the blob endpoint: GET and PUT by sha256, Bearer token required, PUT verified by hash.
type testStore struct {
	mutex  sync.Mutex
	blobs  map[string][]byte
	puts   []string
	server *httptest.Server
}

func newTestStore(t *testing.T) *testStore {
	store := &testStore{blobs: map[string][]byte{}}
	store.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(writer, "no token", http.StatusUnauthorized)
			return
		}
		hash := strings.TrimPrefix(request.URL.Path, "/")
		store.mutex.Lock()
		defer store.mutex.Unlock()
		switch request.Method {
		case http.MethodGet:
			blob, ok := store.blobs[hash]
			if !ok {
				http.NotFound(writer, request)
				return
			}
			writer.Write(blob)
		case http.MethodPut:
			body, _ := io.ReadAll(request.Body)
			if hashOf(body) != hash {
				http.Error(writer, "hash mismatch", http.StatusBadRequest)
				return
			}
			store.blobs[hash] = body
			store.puts = append(store.puts, hash)
		default:
			http.Error(writer, "method", http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(store.server.Close)
	return store
}

// add stores content under its own hash and returns the hash.
func (store *testStore) add(content []byte) string {
	store.mutex.Lock()
	defer store.mutex.Unlock()
	hash := hashOf(content)
	store.blobs[hash] = content
	return hash
}

// A testWire is a run's events endpoint. failing answers every POST with that status; failFirst fails
// only the first so many POSTs with a 503.
type testWire struct {
	mutex     sync.Mutex
	failing   int
	failFirst int
	posts     int
	lines     [][]byte
	server    *httptest.Server
}

func newTestWire(t *testing.T, failing int, failFirst int) *testWire {
	wire := &testWire{failing: failing, failFirst: failFirst}
	wire.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		wire.mutex.Lock()
		defer wire.mutex.Unlock()
		wire.posts++
		if request.Header.Get("Authorization") != "Bearer "+testToken {
			http.Error(writer, "no token", http.StatusUnauthorized)
			return
		}
		if wire.failing != 0 {
			http.Error(writer, "wire down", wire.failing)
			return
		}
		if wire.posts <= wire.failFirst {
			http.Error(writer, "not yet", http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(request.Body)
		for _, line := range bytes.SplitAfter(body, []byte("\n")) {
			if len(line) > 0 {
				wire.lines = append(wire.lines, line)
			}
		}
	}))
	t.Cleanup(wire.server.Close)
	return wire
}

// A lockedBuffer takes each write whole, as a pipe to stdout does, so a test of event order sees the
// order the runner wrote in and never a torn line.
type lockedBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (locked *lockedBuffer) Write(data []byte) (int, error) {
	locked.mutex.Lock()
	defer locked.mutex.Unlock()
	return locked.buffer.Write(data)
}

func (locked *lockedBuffer) Bytes() []byte {
	locked.mutex.Lock()
	defer locked.mutex.Unlock()
	return bytes.Clone(locked.buffer.Bytes())
}

func testUnit(argv ...string) protocol.Unit {
	return protocol.Unit{Run: "r-test", Unit: "unit", Argv: argv, TimeoutSeconds: 30, Token: testToken}
}

func testOptions(t *testing.T) Options {
	return Options{
		WorkspaceParent:  t.TempDir(),
		KillGrace:        5 * time.Second,
		OutputGrace:      2 * time.Second,
		WireInterval:     20 * time.Millisecond,
		WireDrainTimeout: 2 * time.Second,
	}
}

// runUnit runs a unit, decodes its event stream strictly, and checks the stream's shape: sequence from 0
// without gaps, times in RFC 3339 UTC with milliseconds, started first and one finished last.
func runUnit(t *testing.T, unit protocol.Unit, options Options) (Result, []protocol.Event, []byte) {
	t.Helper()
	var stream lockedBuffer
	options.Events = &stream
	var diagnostics lockedBuffer
	options.Diagnostics = &diagnostics
	result := Run(context.Background(), unit, options)
	if text := diagnostics.Bytes(); len(text) > 0 {
		t.Logf("diagnostics: %s", text)
	}
	events := decodeEvents(t, stream.Bytes())
	checkStream(t, unit, events)
	if events[len(events)-1].Status != result.Status {
		t.Fatalf("finished says %s, Run returned %s", events[len(events)-1].Status, result.Status)
	}
	return result, events, stream.Bytes()
}

func decodeEvents(t *testing.T, stream []byte) []protocol.Event {
	t.Helper()
	var events []protocol.Event
	for _, line := range bytes.Split(bytes.TrimSuffix(stream, []byte("\n")), []byte("\n")) {
		var event protocol.Event
		if err := protocol.Decode(bytes.NewReader(line), &event); err != nil {
			t.Fatalf("event %q: %v", line, err)
		}
		events = append(events, event)
	}
	return events
}

var eventTimePattern = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`)

func checkStream(t *testing.T, unit protocol.Unit, events []protocol.Event) {
	t.Helper()
	for index, event := range events {
		if event.Sequence != index {
			t.Fatalf("event %d has sequence %d", index, event.Sequence)
		}
		if event.Run != unit.Run || event.Unit != unit.Unit {
			t.Fatalf("event %d is for %s/%s", index, event.Run, event.Unit)
		}
		if !eventTimePattern.MatchString(event.Time) {
			t.Fatalf("event %d time %q", index, event.Time)
		}
		if (event.Type == "finished") != (index == len(events)-1) {
			t.Fatalf("event %d is %s; finished must be last and only last", index, event.Type)
		}
	}
	if len(events) == 0 || events[0].Type != "started" {
		t.Fatalf("the stream doesn't open with started")
	}
	if verdict := protocol.Decide(unit.Run, []string{unit.Unit}, events); verdict.Status == "void" && events[len(events)-1].Status != protocol.StatusBroken {
		t.Fatalf("the coordinator would call this stream void: %v", verdict.Problems)
	}
}

func eventsOfType(events []protocol.Event, kind string) []protocol.Event {
	var result []protocol.Event
	for _, event := range events {
		if event.Type == kind {
			result = append(result, event)
		}
	}
	return result
}

func outputLines(events []protocol.Event, stream string) []string {
	var result []string
	for _, event := range eventsOfType(events, "output") {
		if event.Stream == stream {
			result = append(result, event.Text)
		}
	}
	return result
}

func errorPhases(events []protocol.Event) string {
	var result []string
	for _, event := range eventsOfType(events, "error") {
		result = append(result, event.Phase+": "+event.Message)
	}
	return strings.Join(result, "\n")
}

func TestASilentUnitStillReportsStartedExitAndFinished(t *testing.T) {
	for _, test := range []struct {
		command string
		code    int
		status  string
	}{{"true", 0, protocol.StatusPassed}, {"false", 1, protocol.StatusFailed}} {
		result, events, _ := runUnit(t, testUnit(test.command), testOptions(t))
		var kinds []string
		for _, event := range events {
			kinds = append(kinds, event.Type)
		}
		if strings.Join(kinds, " ") != "started exit finished" {
			t.Fatalf("%s: events %v", test.command, kinds)
		}
		if events[1].Code == nil || *events[1].Code != test.code || result.Status != test.status {
			t.Fatalf("%s: exit %+v, status %s", test.command, events[1], result.Status)
		}
		started := events[0]
		if started.Machine == "" || started.RunnerVersion == "" || started.Cpus <= 0 || started.MemoryMegabytes <= 0 {
			t.Fatalf("started is missing what it says about the machine: %+v", started)
		}
	}
}

func TestOutputLinesKeepTheirOrderAndAFinalLineWithoutNewline(t *testing.T) {
	_, events, _ := runUnit(t, testUnit("sh", "-c", `printf 'one\n\nthree\nbad \377 byte\nno newline'; printf 'warn\n' >&2; exit 4`), testOptions(t))
	stdout := outputLines(events, "stdout")
	if strings.Join(stdout, "|") != "one||three|bad \uFFFD byte|no newline" {
		t.Fatalf("stdout %q", stdout)
	}
	for _, event := range eventsOfType(events, "output") {
		if event.Replaced != strings.Contains(event.Text, "\uFFFD") {
			t.Fatalf("replaced is %v for %q", event.Replaced, event.Text)
		}
	}
	if stderr := outputLines(events, "stderr"); len(stderr) != 1 || stderr[0] != "warn" {
		t.Fatalf("stderr %q", stderr)
	}
	exit := eventsOfType(events, "exit")[0]
	if exit.Code == nil || *exit.Code != 4 || events[len(events)-1].Status != protocol.StatusFailed {
		t.Fatalf("exit %+v", exit)
	}
}

func TestSequenceSurvivesAFastWriterOnBothStreams(t *testing.T) {
	const lines = 5000
	script := fmt.Sprintf(`for i in $(seq 0 %d); do echo out$i; done &
for i in $(seq 0 %d); do echo err$i >&2; done
wait`, lines-1, lines-1)
	_, events, _ := runUnit(t, testUnit("sh", "-c", script), testOptions(t))
	for _, stream := range []struct{ name, prefix string }{{"stdout", "out"}, {"stderr", "err"}} {
		got := outputLines(events, stream.name)
		if len(got) != lines {
			t.Fatalf("%s: %d lines, want %d", stream.name, len(got), lines)
		}
		for index, text := range got {
			if text != stream.prefix+strconv.Itoa(index) {
				t.Fatalf("%s line %d is %q", stream.name, index, text)
			}
		}
	}
	// The two streams really were interleaved, or this test proved less than it says.
	switches := 0
	for index := 2; index < len(events)-2; index++ {
		if events[index].Stream != events[index-1].Stream {
			switches++
		}
	}
	if switches < 10 {
		t.Logf("only %d switches between streams; the interleaving was light this time", switches)
	}
}

func TestTheEnvironmentDoesNotLeak(t *testing.T) {
	t.Setenv("LOOM_LEAK_PROBE", "leaked")
	t.Setenv("LOOM_SLOT", "3")
	unit := testUnit("env")
	unit.Environment = map[string]string{"GREETING": "hello"}
	_, events, _ := runUnit(t, unit, testOptions(t))
	var names []string
	for _, line := range outputLines(events, "stdout") {
		name, _, _ := strings.Cut(line, "=")
		names = append(names, name)
	}
	sort.Strings(names)
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "LANG": true, "GREETING": true, "LOOM_SLOT": true, "LOOM_SLOT_CPUS": true}
	for _, name := range names {
		if !allowed[name] {
			t.Fatalf("the unit saw %s; its environment was %v", name, names)
		}
	}
	if !strings.Contains(strings.Join(outputLines(events, "stdout"), "\n"), "GREETING=hello") {
		t.Fatalf("the unit's own variable is missing: %v", names)
	}
	if !strings.Contains(strings.Join(outputLines(events, "stdout"), "\n"), "LOOM_SLOT=3") {
		t.Fatalf("the machine's slot didn't reach the unit: %v", names)
	}
}

func TestTheUnitsPathFindsItsCommand(t *testing.T) {
	directory := t.TempDir()
	os.WriteFile(filepath.Join(directory, "only-here"), []byte("#!/bin/sh\necho found\n"), 0o755)
	unit := testUnit("only-here")
	unit.Environment = map[string]string{"PATH": directory + ":/usr/bin:/bin"}
	_, events, _ := runUnit(t, unit, testOptions(t))
	if got := outputLines(events, "stdout"); len(got) != 1 || got[0] != "found" {
		t.Fatalf("stdout %q, errors %s", got, errorPhases(events))
	}
}

func TestACommandThatCantStartIsBroken(t *testing.T) {
	result, events, _ := runUnit(t, testUnit("no-such-command-anywhere"), testOptions(t))
	if result.Status != protocol.StatusBroken || len(eventsOfType(events, "exit")) != 0 || !strings.Contains(errorPhases(events), "start:") {
		t.Fatalf("status %s, errors %s", result.Status, errorPhases(events))
	}
}

func TestAnInvalidUnitIsBrokenWithoutRunning(t *testing.T) {
	cases := map[string]func(*protocol.Unit){
		"no argv":       func(unit *protocol.Unit) { unit.Argv = nil },
		"no timeout":    func(unit *protocol.Unit) { unit.TimeoutSeconds = 0 },
		"directory out": func(unit *protocol.Unit) { unit.Directory = "../elsewhere" },
		"input out": func(unit *protocol.Unit) {
			unit.Inputs = []protocol.Input{{Path: "../x", Sha256: strings.Repeat("a", 64)}}
		},
		"bad hash": func(unit *protocol.Unit) { unit.Inputs = []protocol.Input{{Path: "x", Sha256: "abc"}} },
		"setuid mode": func(unit *protocol.Unit) {
			unit.Inputs = []protocol.Input{{Path: "x", Sha256: strings.Repeat("a", 64), Mode: "4755"}}
		},
		"output out":      func(unit *protocol.Unit) { unit.Outputs = []protocol.Output{{Glob: "../*"}} },
		"no store":        func(unit *protocol.Unit) { unit.Store = nil; unit.Outputs = []protocol.Output{{Glob: "x"}} },
		"bad environment": func(unit *protocol.Unit) { unit.Environment = map[string]string{"A=B": "c"} },
	}
	for name, breakUnit := range cases {
		unit := testUnit("sh", "-c", "echo ran")
		unit.Store = &protocol.Endpoint{Url: "http://127.0.0.1:1"}
		breakUnit(&unit)
		result, events, _ := runUnit(t, unit, testOptions(t))
		if result.Status != protocol.StatusBroken || len(outputLines(events, "stdout")) != 0 {
			t.Errorf("%s: status %s, errors %s", name, result.Status, errorPhases(events))
		}
	}
}

func TestInputsArePlacedVerifiedWithTheirMode(t *testing.T) {
	store := newTestStore(t)
	script := []byte("#!/bin/sh\necho script ran; cat data/notes.txt\n")
	notes := []byte("notes inside")
	unit := testUnit("./run.sh")
	unit.Store = &protocol.Endpoint{Url: store.server.URL}
	unit.Inputs = []protocol.Input{
		{Path: "run.sh", Sha256: store.add(script), Mode: "0755"},
		{Path: "data/notes.txt", Sha256: store.add(notes), Mode: "0400"},
	}
	result, events, _ := runUnit(t, unit, testOptions(t))
	if got := outputLines(events, "stdout"); result.Status != protocol.StatusPassed || strings.Join(got, "|") != "script ran|notes inside" {
		t.Fatalf("status %s, stdout %q, errors %s", result.Status, got, errorPhases(events))
	}
	if events[0].InputHashes["run.sh"] != hashOf(script) {
		t.Fatalf("started inputs %v", events[0].InputHashes)
	}
}

func TestACorruptedInputIsRefusedByHash(t *testing.T) {
	store := newTestStore(t)
	wanted := []byte("the bytes the unit was planned with")
	hash := hashOf(wanted)
	store.blobs[hash] = []byte("the bytes a bad store served instead")
	unit := testUnit("cat", "data.txt")
	unit.Store = &protocol.Endpoint{Url: store.server.URL}
	unit.Inputs = []protocol.Input{{Path: "data.txt", Sha256: hash}}
	options := testOptions(t)
	result, events, _ := runUnit(t, unit, options)
	if result.Status != protocol.StatusBroken {
		t.Fatalf("status %s; a corrupted input must break the unit", result.Status)
	}
	if !strings.Contains(errorPhases(events), "fetch: input data.txt: refused") {
		t.Fatalf("errors %s", errorPhases(events))
	}
	if len(eventsOfType(events, "exit")) != 0 || len(eventsOfType(events, "output")) != 0 {
		t.Fatalf("the command ran on a corrupted input")
	}
	if entries, _ := os.ReadDir(options.WorkspaceParent); len(entries) != 0 {
		t.Fatalf("the workspace is still there: %v", entries)
	}
}

type tarEntry struct {
	name     string
	kind     byte
	content  string
	linkname string
	mode     int64
}

func makeTar(t *testing.T, entries []tarEntry, compress bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	var writer io.Writer = &buffer
	var compressor *gzip.Writer
	if compress {
		compressor = gzip.NewWriter(&buffer)
		writer = compressor
	}
	archive := tar.NewWriter(writer)
	for _, entry := range entries {
		mode := entry.mode
		if mode == 0 {
			mode = 0o644
		}
		header := &tar.Header{Name: entry.name, Typeflag: entry.kind, Linkname: entry.linkname, Mode: mode, Size: int64(len(entry.content))}
		if entry.kind != tar.TypeReg {
			header.Size = 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if entry.kind == tar.TypeReg {
			archive.Write([]byte(entry.content))
		}
	}
	archive.Close()
	if compressor != nil {
		compressor.Close()
	}
	return buffer.Bytes()
}

func TestATarInputUnpacksAtItsPath(t *testing.T) {
	store := newTestStore(t)
	archive := makeTar(t, []tarEntry{
		{name: "a/", kind: tar.TypeDir, mode: 0o755},
		{name: "a/b.txt", kind: tar.TypeReg, content: "inside b\n"},
		{name: "a/run.sh", kind: tar.TypeReg, content: "#!/bin/sh\necho via script\n", mode: 0o755},
		{name: "link.txt", kind: tar.TypeSymlink, linkname: "a/b.txt"},
		{name: "a/hard.txt", kind: tar.TypeLink, linkname: "a/b.txt"},
	}, true)
	unit := testUnit("sh", "-c", "cat ../tree/link.txt; cat a/hard.txt; ./a/run.sh")
	unit.Directory = "tree"
	unit.Store = &protocol.Endpoint{Url: store.server.URL}
	unit.Inputs = []protocol.Input{{Path: "tree", Sha256: store.add(archive), Archive: "tar"}}
	result, events, _ := runUnit(t, unit, testOptions(t))
	if got := outputLines(events, "stdout"); result.Status != protocol.StatusPassed || strings.Join(got, "|") != "inside b|inside b|via script" {
		t.Fatalf("status %s, stdout %q, errors %s", result.Status, got, errorPhases(events))
	}
}

func TestATarEntryEscapingItsPathIsRefused(t *testing.T) {
	// The input lands at <unit directory>/workspace/tree, so three levels up is the WorkspaceParent, which
	// outlives the unit: an escaped entry would still be there to find. Each layer of the refusal has a case
	// only it can catch: a dangling link out only the target check (there is nothing to resolve), a link
	// that reads local but resolves out only the resolving pass, a write through a chain of such links
	// only the os.Root (the write happens before any pass could look).
	chain := []tarEntry{
		{name: "a", kind: tar.TypeSymlink, linkname: "."},    // tree
		{name: "b", kind: tar.TypeSymlink, linkname: "a/.."}, // reads as tree, is the workspace
		{name: "c", kind: tar.TypeSymlink, linkname: "b/.."}, // reads as tree, is the unit directory
		{name: "e", kind: tar.TypeSymlink, linkname: "c/.."}, // reads as tree, is the WorkspaceParent
	}
	cases := map[string][]tarEntry{
		"climbing name":     {{name: "../../../escaped", kind: tar.TypeReg, content: "out"}},
		"absolute name":     {{name: "/tmp/loom-escaped-absolute", kind: tar.TypeReg, content: "out"}},
		"symlink out":       {{name: "escaped", kind: tar.TypeSymlink, linkname: "../../.."}},
		"dangling link out": {{name: "escaped", kind: tar.TypeSymlink, linkname: "../../../not-there"}},
		"local-looking out": chain[:2],
		"write through":     append(chain, tarEntry{name: "e/escaped", kind: tar.TypeReg, content: "out"}),
		"hard link out":     {{name: "escaped", kind: tar.TypeLink, linkname: "../../../outside"}},
		"device":            {{name: "null", kind: tar.TypeChar}},
	}
	for name, entries := range cases {
		store := newTestStore(t)
		unit := testUnit("echo", "ran")
		unit.Store = &protocol.Endpoint{Url: store.server.URL}
		unit.Inputs = []protocol.Input{{Path: "tree", Sha256: store.add(makeTar(t, entries, false)), Archive: "tar"}}
		options := testOptions(t)
		os.WriteFile(filepath.Join(options.WorkspaceParent, "outside"), []byte("outside the unit"), 0o644)
		result, events, _ := runUnit(t, unit, options)
		if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "fetch:") || len(eventsOfType(events, "exit")) != 0 {
			t.Errorf("%s: status %s, errors %s", name, result.Status, errorPhases(events))
		}
		if _, err := os.Lstat(filepath.Join(options.WorkspaceParent, "escaped")); err == nil {
			t.Errorf("%s: an entry landed outside the input's path", name)
		}
		if _, err := os.Lstat("/tmp/loom-escaped-absolute"); err == nil {
			os.Remove("/tmp/loom-escaped-absolute")
			t.Errorf("%s: an absolute entry was written", name)
		}
	}
}

// processGone polls until no process has the pid, or a few seconds pass.
func processGone(pid int) bool {
	for range 60 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

func TestATimeoutKillsTheWholeProcessGroup(t *testing.T) {
	unit := testUnit("sh", "-c", "sleep 100 & echo $!; wait")
	unit.TimeoutSeconds = 1
	began := time.Now()
	result, events, _ := runUnit(t, unit, testOptions(t))
	if elapsed := time.Since(began); elapsed > 4*time.Second {
		t.Fatalf("the timed-out unit took %v", elapsed)
	}
	exit := eventsOfType(events, "exit")[0]
	if !exit.TimedOut || exit.Signal != "SIGTERM" || result.Status != protocol.StatusFailed {
		t.Fatalf("exit %+v, status %s", exit, result.Status)
	}
	grandchild, err := strconv.Atoi(outputLines(events, "stdout")[0])
	if err != nil {
		t.Fatal(err)
	}
	if !processGone(grandchild) {
		syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("the grandchild %d outlived the timeout", grandchild)
	}
}

func TestATimeoutEscalatesToSigkill(t *testing.T) {
	unit := testUnit("sh", "-c", `trap "" TERM; sleep 100 & echo $!; wait`)
	unit.TimeoutSeconds = 1
	options := testOptions(t)
	options.KillGrace = 300 * time.Millisecond
	result, events, _ := runUnit(t, unit, options)
	exit := eventsOfType(events, "exit")[0]
	if !exit.TimedOut || exit.Signal != "SIGKILL" || result.Status != protocol.StatusFailed {
		t.Fatalf("exit %+v, status %s", exit, result.Status)
	}
	grandchild, _ := strconv.Atoi(outputLines(events, "stdout")[0])
	if !processGone(grandchild) {
		syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("the grandchild %d outlived SIGKILL", grandchild)
	}
}

func TestWhatALeaderLeavesBehindDiesWithIt(t *testing.T) {
	_, events, _ := runUnit(t, testUnit("sh", "-c", "sleep 100 >/dev/null 2>&1 & echo $!"), testOptions(t))
	grandchild, _ := strconv.Atoi(outputLines(events, "stdout")[0])
	if !processGone(grandchild) {
		syscall.Kill(grandchild, syscall.SIGKILL)
		t.Fatalf("the background process %d outlived its unit", grandchild)
	}
}

func TestAProcessThatLeftTheGroupCantHoldTheUnitOpen(t *testing.T) {
	if _, err := os.Stat("/usr/bin/perl"); err != nil {
		t.Skip("needs perl for setsid")
	}
	// The child starts its own session, so the group kill can't reach it, and keeps stdout open.
	unit := testUnit("/usr/bin/perl", "-MPOSIX", "-e", `$| = 1; if (fork) { exit 0 } POSIX::setsid(); print "$$\n"; sleep 30`)
	options := testOptions(t)
	options.OutputGrace = 300 * time.Millisecond
	began := time.Now()
	result, events, _ := runUnit(t, unit, options)
	if lines := outputLines(events, "stdout"); len(lines) == 1 {
		if pid, err := strconv.Atoi(lines[0]); err == nil {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	if elapsed := time.Since(began); elapsed > 3*time.Second || result.Status != protocol.StatusPassed {
		t.Fatalf("took %v, status %s", elapsed, result.Status)
	}
	if !strings.Contains(errorPhases(events), "run: a process outside the unit's group") {
		t.Fatalf("errors %s", errorPhases(events))
	}
}

func TestStoppingTheRunnerBreaksTheUnit(t *testing.T) {
	runContext, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	var stream bytes.Buffer
	options := testOptions(t)
	options.Events = &stream
	unit := testUnit("sleep", "100")
	result := Run(runContext, unit, options)
	events := decodeEvents(t, stream.Bytes())
	checkStream(t, unit, events)
	if result.Status != protocol.StatusBroken || eventsOfType(events, "exit")[0].Signal != "SIGTERM" {
		t.Fatalf("status %s, events %+v", result.Status, events)
	}
}

func TestOutputsUploadToTheirHash(t *testing.T) {
	store := newTestStore(t)
	unit := testUnit("sh", "-c", "mkdir -p out/deeper; printf alpha > out/a.txt; printf beta > out/b.txt; echo no > out/c.log; printf deep > out/deeper/d.txt")
	unit.Store = &protocol.Endpoint{Url: store.server.URL}
	unit.Outputs = []protocol.Output{{Glob: "out/*.txt"}, {Glob: "out/deeper/d.txt"}, {Glob: "out/a.txt"}}
	result, events, _ := runUnit(t, unit, testOptions(t))
	if result.Status != protocol.StatusPassed {
		t.Fatalf("status %s, errors %s", result.Status, errorPhases(events))
	}
	want := map[string]string{"out/a.txt": "alpha", "out/b.txt": "beta", "out/deeper/d.txt": "deep"}
	uploaded := eventsOfType(events, "uploaded")
	if len(uploaded) != len(want) {
		t.Fatalf("uploaded %+v", uploaded)
	}
	for _, event := range uploaded {
		content := want[event.Path]
		if event.Sha256 != hashOf([]byte(content)) || event.Bytes != int64(len(content)) {
			t.Fatalf("uploaded %+v for %q", event, content)
		}
		if string(store.blobs[event.Sha256]) != content {
			t.Fatalf("the store holds %q under %s", store.blobs[event.Sha256], event.Sha256)
		}
	}
}

func TestOutputsTheUnitDidntProduceFailIt(t *testing.T) {
	store := newTestStore(t)
	outside := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(outside, []byte("not the unit's"), 0o644)
	for name, test := range map[string]struct {
		script string
		glob   string
	}{
		"missing":     {"true", "out/*.txt"},
		"points away": {"mkdir out; ln -s " + outside + " out/secret.txt", "out/*.txt"},
	} {
		unit := testUnit("sh", "-c", test.script)
		unit.Store = &protocol.Endpoint{Url: store.server.URL}
		unit.Outputs = []protocol.Output{{Glob: test.glob}}
		result, events, _ := runUnit(t, unit, testOptions(t))
		if result.Status != protocol.StatusFailed || len(eventsOfType(events, "uploaded")) != 0 {
			t.Errorf("%s: status %s, errors %s", name, result.Status, errorPhases(events))
		}
	}
	if len(store.puts) != 0 {
		t.Fatalf("the store took %v", store.puts)
	}
}

func TestAStoreThatRefusesAnOutputBreaksTheUnit(t *testing.T) {
	refusing := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Error(writer, "full", http.StatusInsufficientStorage)
	}))
	defer refusing.Close()
	unit := testUnit("sh", "-c", "printf x > out.txt")
	unit.Store = &protocol.Endpoint{Url: refusing.URL}
	unit.Outputs = []protocol.Output{{Glob: "out.txt"}}
	result, events, _ := runUnit(t, unit, testOptions(t))
	if result.Status != protocol.StatusBroken || !strings.Contains(errorPhases(events), "upload:") {
		t.Fatalf("status %s, errors %s", result.Status, errorPhases(events))
	}
}

func TestAFailingWireDoesNotChangeTheStatus(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusUnauthorized} {
		wire := newTestWire(t, status, 0)
		unit := testUnit("sh", "-c", "echo one; sleep 0.2; echo two")
		unit.Wire = &protocol.Endpoint{Url: wire.server.URL + "/runs/r-test/events"}
		options := testOptions(t)
		options.WireDrainTimeout = 500 * time.Millisecond
		result, events, _ := runUnit(t, unit, options)
		if result.Status != protocol.StatusPassed {
			t.Fatalf("wire %d: status %s", status, result.Status)
		}
		if !strings.Contains(errorPhases(events), "wire:") {
			t.Fatalf("wire %d: no wire error on stdout: %s", status, errorPhases(events))
		}
		if got := outputLines(events, "stdout"); strings.Join(got, "|") != "one|two" {
			t.Fatalf("wire %d: stdout %q", status, got)
		}
	}
}

func TestTheWireGetsTheWholeStreamInBatches(t *testing.T) {
	wire := newTestWire(t, 0, 2)
	unit := testUnit("sh", "-c", "echo one; sleep 0.6; echo two; echo three >&2")
	unit.Wire = &protocol.Endpoint{Url: wire.server.URL + "/runs/r-test/events"}
	options := testOptions(t)
	options.WireInterval = 100 * time.Millisecond
	_, events, stream := runUnit(t, unit, options)
	wire.mutex.Lock()
	defer wire.mutex.Unlock()
	if wire.posts < 4 {
		t.Fatalf("%d posts; the wire should be fed while the unit runs", wire.posts)
	}
	// The stdout stream includes the error event about the two refused posts, and so does the wire.
	if got := string(bytes.Join(wire.lines, nil)); got != string(stream) {
		t.Fatalf("the wire got\n%s\nstdout had\n%s", got, stream)
	}
	if !strings.Contains(errorPhases(events), "wire:") {
		t.Fatalf("the failed posts weren't reported")
	}
}

func TestSplitLines(t *testing.T) {
	split := func(input string, maximum int) []string {
		var lines []string
		splitLines(strings.NewReader(input), maximum, func(line []byte) { lines = append(lines, string(line)) })
		return lines
	}
	cases := []struct {
		input   string
		maximum int
		want    []string
	}{
		{"a\nb\n", 8, []string{"a", "b"}},
		{"a\n\nb", 8, []string{"a", "", "b"}},
		{"", 8, nil},
		{"\n", 8, []string{""}},
		{"12345678\n", 8, []string{"12345678"}},
		{"123456789\n", 8, []string{"12345678", "9"}},
		{"1234567890123456\nx", 8, []string{"12345678", "90123456", "x"}},
		{"123456é\n", 8, []string{"123456é"}},       // exactly eight bytes with é's two
		{"1234567é\n", 8, []string{"1234567", "é"}}, // é would straddle the cut, so the cut moves before it
		{"1234567€\n", 8, []string{"1234567", "€"}}, // € is three bytes
		{"\xff\xff\xff\xff\xff\xff\xff\xff\xff", 8, []string{"\xff\xff\xff\xff\xff\xff\xff\xff", "\xff"}},
	}
	for _, test := range cases {
		if got := split(test.input, test.maximum); strings.Join(got, "|") != strings.Join(test.want, "|") || len(got) != len(test.want) {
			t.Errorf("split(%q, %d) = %q, want %q", test.input, test.maximum, got, test.want)
		}
	}
	long := strings.Repeat("x", maximumLineBytes*2+5) + "\nend\n"
	got := split(long, maximumLineBytes)
	if len(got) != 4 || len(got[0]) != maximumLineBytes || len(got[2]) != 5 || got[3] != "end" {
		t.Fatalf("a long line split into %d pieces", len(got))
	}
	if event := outputEvent("stdout", []byte("ok \xc3")); !event.Replaced || event.Text != "ok \uFFFD" {
		t.Fatalf("outputEvent %+v", event)
	}
}

func TestMachineParsing(t *testing.T) {
	if parseCpuMax("max 100000\n") != 0 || parseCpuMax("200000 100000\n") != 2 || parseCpuMax("150000 100000") != 2 {
		t.Fatal("cpu.max")
	}
	if cpusFromQuota("-1", "100000") != 0 || cpusFromQuota("50000", "100000") != 1 {
		t.Fatal("cfs quota")
	}
	if parseMemoryLimit("max\n") != 0 || parseMemoryLimit("9223372036854771712") != 0 || parseMemoryLimit("1073741824\n") != 1<<30 {
		t.Fatal("memory limit")
	}
	if parseMeminfoTotal("MemTotal:       16303528 kB\nMemFree: 1 kB\n") != 16303528<<10 {
		t.Fatal("meminfo")
	}
}

func TestLoadUnit(t *testing.T) {
	unitJson := `{"run":"r","unit":"u","argv":["true"],"timeoutSeconds":5}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		io.WriteString(writer, unitJson)
	}))
	defer server.Close()
	unit, err := LoadUnit(context.Background(), server.URL+"/unit.json", server.Client())
	if err != nil || unit.Unit != "u" {
		t.Fatalf("https: %v %+v", err, unit)
	}
	if _, err := LoadUnit(context.Background(), "http://example.com/unit.json", nil); err == nil {
		t.Fatal("a unit over plain http was accepted")
	}
	path := filepath.Join(t.TempDir(), "unit.json")
	os.WriteFile(path, []byte(`{"run":"r","unit":"u","argv":["true"],"timeoutSeconds":5,"timeout":1}`), 0o644)
	if _, err := LoadUnit(context.Background(), path, nil); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestKeepLeavesTheWorkspaceAndOtherwiseItIsGone(t *testing.T) {
	for _, keep := range []bool{false, true} {
		options := testOptions(t)
		options.Keep = keep
		unit := testUnit("sh", "-c", "mkdir -p locked/inner && touch locked/inner/file && chmod 555 locked/inner locked")
		result, _, _ := runUnit(t, unit, options)
		_, err := os.Stat(result.Workspace)
		if keep != (err == nil) {
			t.Fatalf("keep %v: workspace stat %v", keep, err)
		}
		if keep {
			removeDirectory(filepath.Dir(result.Workspace))
		}
	}
}

// A unit that stays silent past Heartbeat gets the runner's own line saying it's still running, on the runner
// stream, and keeps getting one; its exit is untouched.
func TestHeartbeatWhileSilent(t *testing.T) {
	options := testOptions(t)
	options.Heartbeat = 200 * time.Millisecond
	unit := testUnit("sh", "-c", "sleep 1")
	result, events, _ := runUnit(t, unit, options)
	if result.Status != "passed" {
		t.Fatalf("status %s", result.Status)
	}
	beats := 0
	for _, event := range events {
		if event.Type == "output" && event.Stream == "runner" {
			if !strings.HasPrefix(event.Text, "loom-runner: still running after") {
				t.Fatalf("heartbeat text %q", event.Text)
			}
			beats++
		}
	}
	if beats < 2 {
		t.Fatalf("%d heartbeats in a silent second at 200 ms, want several", beats)
	}
}
