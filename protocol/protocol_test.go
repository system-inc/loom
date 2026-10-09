package protocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func readJob(t *testing.T, path string) Job {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var job Job
	if err := Decode(file, &job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestTheExamplesDecodeAndTheGateExpandsToElevenUnits(t *testing.T) {
	job := readJob(t, "../examples/adamic-gate.job.json")
	plan, err := Expand(job)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 11 || plan[0].Id != "build" || plan[1].Id != "tests[shard=0]" || plan[10].Id != "tests[shard=9]" {
		t.Fatalf("plan: %v", ids(plan))
	}
	if got := plan[4].Unit.Argv; !reflect.DeepEqual(got, []string{"bash", "loom/run-shard.sh", "3"}) {
		t.Fatalf("argv of shard 3: %v", got)
	}
	if got := plan[4].Unit.Outputs[0].Glob; got != "loom-out/shard-3/test.jsonl" {
		t.Fatalf("output of shard 3: %s", got)
	}
	file, err := os.Open("../examples/echo.unit.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var unit Unit
	if err := Decode(file, &unit); err != nil {
		t.Fatal(err)
	}
}

func ids(plan []PlannedUnit) []string {
	var result []string
	for _, unit := range plan {
		result = append(result, unit.Id)
	}
	return result
}

func TestDecodeRefusesUnknownFieldsAndTrailingData(t *testing.T) {
	var job Job
	for _, text := range []string{`{"name":"a","units":[],"nmae":"b"}`, `{"name":"a","units":[]} {}`, `{"name":"a","units":[]} x`} {
		if err := Decode(strings.NewReader(text), &job); err == nil {
			t.Errorf("accepted %s", text)
		}
	}
}

func TestExpandRefusesBrokenJobs(t *testing.T) {
	good := func() Job {
		return Job{Name: "j", Units: []JobUnit{{Id: "a", Argv: []string{"true"}, TimeoutSeconds: 1}}}
	}
	cases := map[string]func(*Job){
		"no name":           func(j *Job) { j.Name = "" },
		"no units":          func(j *Job) { j.Units = nil },
		"bad id":            func(j *Job) { j.Units[0].Id = "A b" },
		"duplicate id":      func(j *Job) { j.Units = append(j.Units, j.Units[0]) },
		"no argv":           func(j *Job) { j.Units[0].Argv = nil },
		"no timeout":        func(j *Job) { j.Units[0].TimeoutSeconds = 0 },
		"unknown need":      func(j *Job) { j.Units[0].Needs = []string{"z"} },
		"self need":         func(j *Job) { j.Units[0].Needs = []string{"a"} },
		"unknown toolchain": func(j *Job) { j.Units[0].Requires = []string{"wasi-sdk"} },
		"bad hash":          func(j *Job) { j.Units[0].Inputs = []Input{{Path: "x", Sha256: "abc"}} },
		"bad archive": func(j *Job) {
			j.Units[0].Inputs = []Input{{Path: "x", Sha256: strings.Repeat("a", 64), Archive: "zip"}}
		},
		"undefined matrix": func(j *Job) { j.Units[0].Argv = []string{"${matrix.k}"} },
		"cycle": func(j *Job) {
			j.Units[0].Needs = []string{"b"}
			j.Units = append(j.Units, JobUnit{Id: "b", Needs: []string{"a"}, Argv: []string{"true"}, TimeoutSeconds: 1})
		},
	}
	if _, err := Expand(good()); err != nil {
		t.Fatalf("the good job is refused: %v", err)
	}
	required := good()
	required.Units[0].Requires = []string{"wasiSdk", "clang"}
	if _, err := Expand(required); err != nil {
		t.Fatalf("a unit requiring known toolchains is refused: %v", err)
	}
	for name, breakIt := range cases {
		job := good()
		breakIt(&job)
		if _, err := Expand(job); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestMatrixExpandsEveryCombinationInSortedKeyOrder(t *testing.T) {
	job := Job{Name: "j", Units: []JobUnit{{Id: "t", Matrix: map[string][]string{"os": {"linux", "darwin"}, "a": {"1", "2"}},
		Argv: []string{"run", "${matrix.os}-${matrix.a}"}, TimeoutSeconds: 1}}}
	plan, err := Expand(job)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"t[a=1,os=linux]", "t[a=1,os=darwin]", "t[a=2,os=linux]", "t[a=2,os=darwin]"}
	if !reflect.DeepEqual(ids(plan), want) {
		t.Fatalf("plan %v, want %v", ids(plan), want)
	}
	if plan[1].Unit.Argv[1] != "darwin-1" || plan[1].Unit.Matrix != nil {
		t.Fatalf("expanded unit: %+v", plan[1].Unit)
	}
}

// A run's events: each unit gets started, one output line and finished with the given status.
func events(run string, statuses map[string]string, order []string) []Event {
	var result []Event
	for _, unit := range order {
		result = append(result,
			Event{Run: run, Unit: unit, Sequence: 0, Type: "started"},
			Event{Run: run, Unit: unit, Sequence: 1, Type: "output", Stream: "stdout", Text: "x"},
			Event{Run: run, Unit: unit, Sequence: 2, Type: "finished", Status: statuses[unit]})
	}
	return result
}

func TestDecideIsGreenOnlyWhenEveryPlannedUnitPassedOnce(t *testing.T) {
	plan := []string{"a", "b"}
	all := map[string]string{"a": StatusPassed, "b": StatusPassed}
	if verdict := Decide("r", plan, events("r", all, plan)); verdict.Status != "green" {
		t.Fatalf("all passed: %+v", verdict)
	}
	red := Decide("r", plan, events("r", map[string]string{"a": StatusPassed, "b": StatusFailed}, plan))
	if red.Status != "red" || !reflect.DeepEqual(red.Failed, []string{"b"}) {
		t.Fatalf("one failed: %+v", red)
	}
	voids := map[string][]Event{
		"missing unit":   events("r", all, []string{"a"}),
		"stray unit":     events("r", map[string]string{"a": StatusPassed, "b": StatusPassed, "c": StatusPassed}, []string{"a", "b", "c"}),
		"duplicate unit": append(events("r", all, plan), events("r", all, []string{"b"})...),
		"broken unit":    events("r", map[string]string{"a": StatusPassed, "b": StatusBroken}, plan),
		"unknown status": events("r", map[string]string{"a": StatusPassed, "b": "fine"}, plan),
		"other run":      append(events("r", all, plan), Event{Run: "q", Unit: "a", Type: "output"}),
		"gap":            removeSequence(events("r", all, plan), "a", 1),
		"after finished": append(events("r", all, plan), Event{Run: "r", Unit: "a", Sequence: 3, Type: "output"}),
	}
	for name, stream := range voids {
		if verdict := Decide("r", plan, stream); verdict.Status != "void" || len(verdict.Problems) == 0 {
			t.Errorf("%s: %+v", name, verdict)
		}
	}
	// Void wins over red: a failed unit beside a lost one proved nothing.
	mixed := events("r", map[string]string{"a": StatusFailed}, []string{"a"})
	if verdict := Decide("r", plan, mixed); verdict.Status != "void" || !reflect.DeepEqual(verdict.Failed, []string{"a"}) {
		t.Fatalf("failed and missing: %+v", verdict)
	}
	if verdict := Decide("r", []string{"a", "a"}, events("r", all, []string{"a"})); verdict.Status != "void" {
		t.Fatalf("a plan naming a unit twice: %+v", verdict)
	}
}

func removeSequence(stream []Event, unit string, sequence int) []Event {
	var result []Event
	for _, event := range stream {
		if event.Unit == unit && event.Sequence == sequence {
			continue
		}
		result = append(result, event)
	}
	return result
}

// The Worker (wire/) checks the same vector, so Go and TypeScript sign the same bytes.
const tokenVector = "eyJydW4iOiJyLXZlY3RvciIsInNjb3BlIjoicnVubmVyIiwiZXhwaXJlcyI6NDEwMjQ0NDgwMH0.5yFFC9AOwC9L6zqwWz8V0aCJNrLwusF5LjbOv_hoDWY"

func TestTokenVector(t *testing.T) {
	secret := []byte("loom-test-secret")
	token, err := MintToken(secret, TokenClaims{Run: "r-vector", Scope: ScopeRunner, Expires: 4102444800})
	if err != nil || token != tokenVector {
		t.Fatalf("minted %q (%v), want the pinned vector", token, err)
	}
	now := time.Unix(1791486000, 0)
	if claims, err := VerifyToken(secret, token, now); err != nil || claims.Run != "r-vector" || claims.Scope != ScopeRunner {
		t.Fatalf("verify: %+v %v", claims, err)
	}
	bad := map[string]string{
		"other secret":  "",
		"tampered":      strings.Replace(token, "eyJydW4iOiJy", "eyJydW4iOiJz", 1),
		"no signature":  strings.Split(token, ".")[0],
		"bad signature": strings.Split(token, ".")[0] + ".AAAA",
	}
	for name, candidate := range bad {
		key := secret
		if name == "other secret" {
			candidate, key = token, []byte("other")
		}
		if _, err := VerifyToken(key, candidate, now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := VerifyToken(secret, token, time.Unix(4102444800, 0)); err == nil {
		t.Error("expired token accepted")
	}
	if _, err := MintToken(secret, TokenClaims{Run: "r", Scope: "admin", Expires: 1}); err == nil {
		t.Error("unknown scope minted")
	}
	board, err := MintToken(secret, TokenClaims{Run: BoardRun, Scope: ScopeBoard, Expires: 4102444800})
	if claims, verifyError := VerifyToken(secret, board, now); err != nil || verifyError != nil || claims.Scope != ScopeBoard {
		t.Fatalf("a board token: %v %v %+v", err, verifyError, claims)
	}
	// A pool token names its pool where other tokens name a run.
	pool, err := MintToken(secret, TokenClaims{Run: "codex", Scope: ScopePool, Expires: 4102444800})
	if claims, verifyError := VerifyToken(secret, pool, now); err != nil || verifyError != nil || claims.Scope != ScopePool || claims.Run != "codex" {
		t.Fatalf("a pool token: %v %v %+v", err, verifyError, claims)
	}
	// A submit token names its owner, and the Worker spells the scope the same way.
	submit, err := MintToken(secret, TokenClaims{Run: "system_adamic_loom_web", Scope: ScopeSubmit, Expires: 4102444800})
	if claims, verifyError := VerifyToken(secret, submit, now); err != nil || verifyError != nil || claims.Scope != "submit" || claims.Run != "system_adamic_loom_web" {
		t.Fatalf("a submit token: %v %v %+v", err, verifyError, claims)
	}
	// A build token names its builder, and the Worker spells the scope the same way.
	build, err := MintToken(secret, TokenClaims{Run: "workshop", Scope: ScopeBuild, Expires: 4102444800})
	if claims, verifyError := VerifyToken(secret, build, now); err != nil || verifyError != nil || claims.Scope != "build" || claims.Run != "workshop" {
		t.Fatalf("a build token: %v %v %+v", err, verifyError, claims)
	}
}

func TestVerdictIsLowercaseWithEmptyListsAsArrays(t *testing.T) {
	for _, verdict := range []Verdict{{Status: "green"}, Decide("r", []string{"a"}, events("r", map[string]string{"a": StatusPassed}, []string{"a"}))} {
		text, err := json.Marshal(verdict)
		if err != nil || string(text) != `{"status":"green","failed":[],"problems":[],"cached":[]}` {
			t.Fatalf("marshalled %s (%v)", text, err)
		}
	}
	var back Verdict
	if err := Decode(strings.NewReader(`{"status":"red","failed":["b"],"problems":[],"cached":[]}`), &back); err != nil || back.Failed[0] != "b" {
		t.Fatalf("decoded %+v (%v)", back, err)
	}
}

func TestPlanOfNamesEveryUnitInPlanOrderAndEveryInputOnce(t *testing.T) {
	plan, err := Expand(readJob(t, "../examples/adamic-gate.job.json"))
	if err != nil {
		t.Fatal(err)
	}
	body := PlanOf(plan)
	text, err := json.Marshal(body)
	if err != nil || !strings.HasPrefix(string(text), `{"units":["build","tests[shard=0]",`) {
		t.Fatalf("plan body %s (%v)", text, err)
	}
	if len(body.Inputs) == 0 || !sort.StringsAreSorted(body.Inputs) {
		t.Fatalf("inputs %v", body.Inputs)
	}
	shared := Input{Path: "x", Sha256: strings.Repeat("b", 64)}
	twice := PlanOf([]PlannedUnit{{Id: "a", Unit: JobUnit{Inputs: []Input{shared}}}, {Id: "b", Unit: JobUnit{Inputs: []Input{shared}}}})
	none := PlanOf([]PlannedUnit{{Id: "a"}})
	if len(twice.Inputs) != 1 || none.Inputs == nil {
		t.Fatalf("a shared input once, and none as []: %v %v", twice.Inputs, none.Inputs)
	}
}

func TestDecideListsCachedUnitsAndRefusesACachedFailure(t *testing.T) {
	plan := []string{"a", "b"}
	stream := append(events("r", map[string]string{"a": StatusPassed}, []string{"a"}),
		Event{Run: "r", Unit: "b", Sequence: 0, Type: "cached", Key: strings.Repeat("c", 64), FromRun: "r0", EventLog: strings.Repeat("d", 64)},
		Event{Run: "r", Unit: "b", Sequence: 1, Type: "finished", Status: StatusPassed})
	if verdict := Decide("r", plan, stream); verdict.Status != "green" || !reflect.DeepEqual(verdict.Cached, []string{"b"}) {
		t.Fatalf("a cached pass: %+v", verdict)
	}
	stream[len(stream)-1].Status = StatusFailed
	if verdict := Decide("r", plan, stream); verdict.Status != "void" {
		t.Fatalf("a cached failure: %+v", verdict)
	}
}

// A zero is left off the line and reads back as zero: the rule viewers rely on.
func TestZeroFieldsAreLeftOffAndReadBackAsZero(t *testing.T) {
	zero := 0
	for _, event := range []Event{
		{Run: "r", Unit: "a", Type: "output", Stream: "stdout"},
		{Run: "r", Unit: "a", Type: "uploaded", Path: "empty", Sha256: strings.Repeat("e", 64)},
		{Run: "r", Unit: "a", Type: "exit", Code: &zero},
	} {
		text, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		for _, absent := range []string{`"text"`, `"bytes"`, `"wallSeconds"`} {
			if strings.Contains(string(text), absent) {
				t.Errorf("%s carries %s", text, absent)
			}
		}
		if event.Type == "exit" && !strings.Contains(string(text), `"code":0`) {
			t.Errorf("exit code 0 was dropped: %s", text)
		}
		var back Event
		if err := Decode(strings.NewReader(string(text)), &back); err != nil || back.Text != "" || back.Bytes != 0 || back.WallSeconds != 0 {
			t.Errorf("read back %+v (%v)", back, err)
		}
	}
}

func TestCheckUnitRefusesWhatARunnerCouldOnlyRunWrongly(t *testing.T) {
	good := func() Unit {
		return Unit{Run: "r-1.a_b", Unit: "tests[shard=0]", Argv: []string{"true"}, TimeoutSeconds: 1,
			Inputs:  []Input{{Path: "bin/x", Sha256: strings.Repeat("a", 64), Mode: "755"}},
			Outputs: []Output{{Glob: "out/*.json"}}, Store: &Endpoint{Url: "https://wire.example/blobs"}}
	}
	if err := CheckUnit(good()); err != nil {
		t.Fatalf("the good unit is refused: %v", err)
	}
	cases := map[string]func(*Unit){
		"no run":            func(u *Unit) { u.Run = "" },
		"run with a slash":  func(u *Unit) { u.Run = "a/b" },
		"run starting dot":  func(u *Unit) { u.Run = ".a" },
		"run too long":      func(u *Unit) { u.Run = strings.Repeat("r", 129) },
		"no unit":           func(u *Unit) { u.Unit = "" },
		"unit too long":     func(u *Unit) { u.Unit = strings.Repeat("u", MaximumUnitIdLength+1) },
		"no argv":           func(u *Unit) { u.Argv = nil },
		"no timeout":        func(u *Unit) { u.TimeoutSeconds = 0 },
		"directory escapes": func(u *Unit) { u.Directory = "../x" },
		"bad variable name": func(u *Unit) { u.Environment = map[string]string{"A=B": "c"} },
		"input escapes":     func(u *Unit) { u.Inputs[0].Path = "/etc/passwd" },
		"bad hash":          func(u *Unit) { u.Inputs[0].Sha256 = "abc" },
		"setuid mode":       func(u *Unit) { u.Inputs[0].Mode = "4755" },
		"tar with a mode":   func(u *Unit) { u.Inputs[0].Archive = "tar" },
		"output escapes":    func(u *Unit) { u.Outputs[0].Glob = "../x" },
		"no store":          func(u *Unit) { u.Store = nil },
		"store not http":    func(u *Unit) { u.Store = &Endpoint{Url: "file:///tmp"} },
	}
	for name, breakIt := range cases {
		unit := good()
		breakIt(&unit)
		if err := CheckUnit(unit); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := CheckUnit(Unit{Run: strings.Repeat("r", 128), Unit: "a", Argv: []string{"true"}, TimeoutSeconds: 1}); err != nil {
		t.Errorf("a 128-character run id is refused: %v", err)
	}
}

// Signs arbitrary claim bytes, to show the claims check stands on its own behind a good signature.
func signedClaims(secret []byte, payload string) string {
	first := encoding.EncodeToString([]byte(payload))
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(first))
	return first + "." + encoding.EncodeToString(mac.Sum(nil))
}

func TestVerifyTokenRefusesWellSignedClaimsTheWorkerWouldRefuse(t *testing.T) {
	secret := []byte("loom-test-secret")
	now := time.Unix(1791486000, 0)
	if _, err := VerifyToken(secret, signedClaims(secret, `{"run":"r","scope":"viewer","expires":4102444800}`), now); err != nil {
		t.Fatalf("the good claims are refused: %v", err)
	}
	for _, payload := range []string{
		`{"Run":"r","scope":"viewer","expires":4102444800}`,
		`{"run":"r","SCOPE":"viewer","expires":4102444800}`,
		`{"run":"r","scope":"viewer","Expires":4102444800}`,
		`{"run":"r","Run":"q","scope":"viewer","expires":4102444800}`,
		`{"run":"","scope":"viewer","expires":4102444800}`,
		`{"run":"r","scope":"admin","expires":4102444800}`,
		`{"run":"r","scope":"viewer"}`,
		`{"run":"r","scope":"viewer","expires":4102444800,"extra":1}`,
		`{"run":"r","scope":"viewer","expires":0}`,
	} {
		if _, err := VerifyToken(secret, signedClaims(secret, payload), now); err == nil {
			t.Errorf("accepted %s", payload)
		}
	}
	if _, err := MintToken(secret, TokenClaims{Run: "a/b", Scope: ScopeRunner, Expires: 1}); err == nil {
		t.Error("minted a token for a run id the wire refuses")
	}
}

func TestReadTokenSecretSignsTheTrimmedTextNotDecodedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token-secret")
	if err := os.WriteFile(path, []byte("6c6f6f6d\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secret, err := ReadTokenSecret(path)
	if err != nil || string(secret) != "6c6f6f6d" {
		t.Fatalf("read %q (%v)", secret, err)
	}
	if err := os.WriteFile(path, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadTokenSecret(path); err == nil {
		t.Error("an empty secret was read")
	}
}

// The events the zero rule and the error phases produce, as Go marshals them. The Worker's tests post this
// same file (wire/test/Contract.test.ts), so a change on either side that breaks the other fails a test.
func TestEventsFixtureIsWhatGoWrites(t *testing.T) {
	zero := 0
	at := func(sequence int) string {
		return time.Unix(1791486000, int64(sequence)*int64(time.Millisecond)).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	}
	var fixture []Event
	add := func(unit string, event Event) {
		event.Run, event.Unit = "r-fixture", unit
		for _, existing := range fixture {
			if existing.Unit == unit {
				event.Sequence++
			}
		}
		event.Time = at(event.Sequence)
		fixture = append(fixture, event)
	}
	add("a", Event{Type: "started", Machine: "box", RunnerVersion: "v0-dev", Cpus: 4, MemoryMegabytes: 16384, HeartbeatSeconds: 120})
	add("a", Event{Type: "output", Stream: "stdout"})
	add("a", Event{Type: "output", Stream: "stderr", Text: "�", Replaced: true})
	add("a", Event{Type: "error", Phase: PhaseWire, Message: "posting to the wire failed, retrying"})
	add("a", Event{Type: "exit", Code: &zero})
	add("a", Event{Type: "uploaded", Path: "empty.txt", Sha256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"})
	add("a", Event{Type: "finished", Status: StatusPassed})
	add("tests[shard=0]", Event{Type: "started", Machine: "box", RunnerVersion: "v0-dev", Cpus: 4, MemoryMegabytes: 16384,
		InputHashes: map[string]string{"bin/x": strings.Repeat("a", 64)}})
	add("tests[shard=0]", Event{Type: "exit", Signal: "SIGTERM", TimedOut: true, WallSeconds: 600.002, UserSeconds: 1.5})
	add("tests[shard=0]", Event{Type: "error", Phase: PhaseRun, Message: "a process outside the unit's group held its output open 2s after the command exited; stopped reading"})
	add("tests[shard=0]", Event{Type: "finished", Status: StatusFailed})
	add("cached", Event{Type: "cached", Key: strings.Repeat("c", 64), FromRun: "r-earlier", EventLog: strings.Repeat("d", 64)})
	add("cached", Event{Type: "finished", Status: StatusPassed})

	var written strings.Builder
	for _, event := range fixture {
		text, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		written.Write(text)
		written.WriteByte('\n')
	}
	path := "testdata/events.jsonl"
	if os.Getenv("LOOM_WRITE_FIXTURES") == "1" {
		if err := os.WriteFile(path, []byte(written.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	held, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(held) != written.String() {
		t.Fatalf("%s no longer matches what Go writes; rerun with LOOM_WRITE_FIXTURES=1 if the change is meant:\n%s", path, written.String())
	}
	if verdict := Decide("r-fixture", []string{"a", "tests[shard=0]", "cached"}, fixture); verdict.Status != "red" || verdict.Failed[0] != "tests[shard=0]" || verdict.Cached[0] != "cached" {
		t.Fatalf("the fixture decides %+v", verdict)
	}
}

// #5pfcv0t: a unit its machine couldn't run finishes broken, the coordinator notes it placed it again, and the next
// attempt continues the stream; its last finished decides it. Anything else after a finished still voids the run.
func TestABrokenUnitPlacedAgainIsDecidedByItsLastAttempt(t *testing.T) {
	stream := func(statuses ...string) []Event {
		var events []Event
		add := func(kind string, status string, phase string) {
			events = append(events, Event{Run: "r", Unit: "u", Sequence: len(events), Time: "t", Type: kind, Status: status, Phase: phase})
		}
		for index, status := range statuses {
			if status == "place" {
				add("error", "", PhasePlace)
				continue
			}
			if status == "output" {
				add("output", "", "")
				continue
			}
			if status == "" {
				continue
			}
			if index == 0 || statuses[index-1] == "place" {
				add("started", "", "")
			}
			add("finished", status, "")
		}
		return events
	}
	for _, test := range []struct {
		name     string
		statuses []string
		want     string
	}{
		{"broken, placed again, passed", []string{StatusBroken, "place", StatusPassed}, "green"},
		{"broken twice, placed again twice, passed", []string{StatusBroken, "place", StatusBroken, "place", StatusPassed}, "green"},
		{"broken, placed again, failed", []string{StatusBroken, "place", StatusFailed}, "red"},
		{"broken, placed again, broken", []string{StatusBroken, "place", StatusBroken}, "void"},
		{"broken, placed again, never finished", []string{StatusBroken, "place"}, "void"},
		{"broken, then output without a placement", []string{StatusBroken, "output"}, "void"},
		{"failed, placed again, passed", []string{StatusFailed, "place", StatusPassed}, "void"},
		{"passed, placed again, passed", []string{StatusPassed, "place", StatusPassed}, "void"},
	} {
		if verdict := Decide("r", []string{"u"}, stream(test.statuses...)); verdict.Status != test.want {
			t.Errorf("%s: %s %v, want %s", test.name, verdict.Status, verdict.Problems, test.want)
		}
	}
	if err := CheckUnit(Unit{Run: "r", Unit: "u", Argv: []string{"true"}, TimeoutSeconds: 1, BrokenExit: 256}); err == nil {
		t.Errorf("brokenExit 256 accepted")
	}
}
