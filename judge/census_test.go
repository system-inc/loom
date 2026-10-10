package judge

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// The fixtures in testdata/census are copied unchanged from Adamic's devtools/fast-gate 348ac0ff: the live table
// (internal/skipcensus/testdata/skips.json and cloud/fast-gate/census-extra.json) and skipcensus's own real-log fixture
// (plain-skips.jsonl, from gate-logs/2e165469ec95/plain), so this port reads them as skipcensus does.

func loadRows(t *testing.T, names ...string) []CensusRow {
	t.Helper()
	rows := []CensusRow{}
	for _, name := range names {
		file, err := os.Open("testdata/census/" + name)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadCensusRows(file)
		file.Close()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rows = append(rows, loaded...)
	}
	return rows
}

func loadEvents(t *testing.T, name string) []TestEvent {
	t.Helper()
	file, err := os.Open("testdata/census/" + name)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	events := []TestEvent{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var event TestEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}

const nativePackage = "github.com/system-inc/adamic/internal/native"

func skipOf(test, message string) []TestEvent {
	return []TestEvent{{Action: "output", Package: nativePackage, Test: test, Output: "    wasm_test.go:12: " + message + "\n"},
		{Action: "skip", Package: nativePackage, Test: test}}
}

func TestTheLiveTableLoads(t *testing.T) {
	if rows := loadRows(t, "skips.json", "census-extra.json"); len(rows) != 141 {
		t.Fatalf("%d rows, want skips.json's 71 and census-extra.json's 70", len(rows))
	}
	if _, err := LoadCensusRows(strings.NewReader(`[{"file":"a_test.go","novel":1}]`)); err == nil {
		t.Fatal("a row with a field this port doesn't know was read")
	}
}

// skipcensus's TestHistoricalPlainSkips over the same fixture and table: 33 skips, 17 required inputs, none unknown.
func TestTheRealLogReadsAsSkipcensusReadsIt(t *testing.T) {
	result := Census(loadEvents(t, "plain-skips.jsonl"), loadRows(t, "skips.json"), nil, nil, "linux")
	if result.Summary() != "skips=33 required-input=17 unknown=0 pending=0 covered=0 heavy=0" || !result.Failed() {
		t.Fatalf("%s, failed %v\n%s", result.Summary(), result.Failed(), strings.Join(result.Lines, "\n"))
	}
	if len(result.Failing) != 17 || !strings.HasPrefix(result.Failing[0], "required-input ") {
		t.Fatalf("failing %v", result.Failing)
	}
	found := false
	for _, failing := range result.Failing {
		found = found || strings.HasSuffix(failing, " TestCompilerAndStage1Agree")
	}
	if !found {
		t.Fatalf("TestCompilerAndStage1Agree's required input isn't named: %v", result.Failing)
	}
}

func TestEachSkipClass(t *testing.T) {
	rows := []CensusRow{
		{File: "internal/native/a_test.go", ID: "m", Callers: []string{"TestMeasured"}, Message: `"a measurement"`, Class: "measurement", Provides: "timing"},
		{File: "internal/native/a_test.go", ID: "p", Callers: []string{"TestPending"}, Message: `"awaits codex/feature: not yet"`, Class: "pending", Awaits: "codex/feature", Provides: "the feature"},
		{File: "internal/native/a_test.go", ID: "s", Callers: []string{"TestLeft"}, Message: `"left to its sibling"`, Class: "not-applicable", Provides: "sibling runs it", Siblings: []string{"TestSibling"}},
		{File: "internal/native/a_test.go", ID: "o", Callers: []string{"TestDarwin"}, Message: `"darwin only"`, Class: "not-applicable", Provides: "a darwin path", Platforms: []string{"linux"}},
	}
	offMain := func(string) (bool, error) { return false, nil }
	onMain := func(string) (bool, error) { return true, nil }
	cases := []struct {
		name   string
		events []TestEvent
		landed Landed
		failed bool
		count  func(CensusResult) int
	}{
		{"a declared measurement passes", skipOf("TestMeasured", "a measurement"), nil, false, func(r CensusResult) int { return 1 - r.Unknown }},
		{"an undeclared skip is unknown and fails", skipOf("TestWASIUnit07", "no WASI sysroot"), nil, true, func(r CensusResult) int { return r.Unknown }},
		{"a declared row taking another reason is unknown", skipOf("TestMeasured", "something else"), nil, true, func(r CensusResult) int { return r.Unknown }},
		{"a pending skip off main passes", skipOf("TestPending", "awaits codex/feature: not yet"), offMain, false, func(r CensusResult) int { return r.Pending }},
		{"a pending skip whose branch landed fails", skipOf("TestPending", "awaits codex/feature: not yet"), onMain, true, func(r CensusResult) int { return r.Overdue }},
		{"a pending skip with nothing to ask is unknown", skipOf("TestPending", "awaits codex/feature: not yet"), nil, true, func(r CensusResult) int { return r.Unknown }},
		{"a branch named only in the message is pending off main", skipOf("TestOther", "awaits codex/other: soon"), offMain, false, func(r CensusResult) int { return r.Pending }},
		{"an unsure branch fails closed", skipOf("TestOther", "awaits codex/other: soon"), func(string) (bool, error) { return false, errors.New("no origin") }, true, func(r CensusResult) int { return r.Overdue }},
		{"a sibling's skip holds when the sibling passed", append(skipOf("TestLeft", "left to its sibling"), TestEvent{Action: "pass", Package: nativePackage, Test: "TestSibling"}), nil, false, func(r CensusResult) int { return 1 - r.Unknown }},
		{"a sibling's skip without the sibling's pass is unknown", skipOf("TestLeft", "left to its sibling"), nil, true, func(r CensusResult) int { return r.Unknown }},
		{"a platform row holds on its platform", skipOf("TestDarwin", "darwin only"), nil, false, func(r CensusResult) int { return 1 - r.Unknown }},
		{"a skip a later pass covers is never classed", append(skipOf("TestWASIUnit07", "no WASI sysroot"), TestEvent{Action: "pass", Package: nativePackage, Test: "TestWASIUnit07"}), nil, false, func(r CensusResult) int { return r.Covered }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := Census(c.events, rows, nil, c.landed, "linux")
			if result.Failed() != c.failed || c.count(result) != 1 {
				t.Fatalf("%s failed %v, want failed %v\n%s", result.Summary(), result.Failed(), c.failed, strings.Join(result.Lines, "\n"))
			}
		})
	}
	if result := Census(skipOf("TestDarwin", "darwin only"), rows, nil, nil, "darwin"); !result.Failed() {
		t.Fatal("a platform row held off its platform")
	}
}

// 0096078f (deferred-red-reaches-census) adds a skip site no census declares to runWASIUnit, so every TestWASIUnit
// shard skips: through the live table that must fail the census, naming each shard.
func TestTheCensusMutantsSkipsFailTheLiveTable(t *testing.T) {
	events := []TestEvent{}
	for _, shard := range []string{"TestWASIUnit00", "TestWASIUnit35"} {
		events = append(events, skipOf(shard, "gate mutant 3: a deferred red that must reach the census")...)
	}
	result := Census(events, loadRows(t, "skips.json", "census-extra.json"), nil, func(string) (bool, error) { return false, nil }, "linux")
	if !result.Failed() || result.Unknown != 2 || strings.Join(result.Failing, ",") != "unknown "+nativePackage+" TestWASIUnit00,unknown "+nativePackage+" TestWASIUnit35" {
		t.Fatalf("%s %v", result.Summary(), result.Failing)
	}
}

func TestFinishedKeepsTheTestEventsForTheCensus(t *testing.T) {
	skip := `{"Action":"skip","Package":"p","Test":"TestS"}`
	output := `{"Action":"output","Package":"p","Test":"TestS","Output":"why\n"}`
	finished, _ := FinishedFromEvents([]protocol.Event{started(), {Type: "output", Text: output + "\n" + skip + "\n" + `{"Action":"output","Package":"p","Output":"ok\n"}`}, {Type: "finished", Status: "passed"}})
	if len(finished.Events) != 2 || finished.Events[0].Output != "why\n" || finished.Events[1].Action != "skip" {
		t.Fatalf("events %+v", finished.Events)
	}
}

const lintPackage = "github.com/system-inc/adamic/stage1/cohere/lint"

func heavySkip(test, message string) []TestEvent {
	return []TestEvent{{Action: "output", Package: lintPackage, Test: test, Output: "    lint_test.go:40: " + message + "\n"}, {Action: "skip", Package: lintPackage, Test: test}}
}

// The live heavy-units.tsv (devtools/fast-gate 348ac0ff) declares stage1/cohere/lint's TestCompilerAndStage1Agree_CheckerSanitized.
func TestADeclaredHeavyDeferralIsClassedHeavyAndAnyOtherIsRed(t *testing.T) {
	content, err := os.ReadFile("testdata/census/heavy-units.tsv")
	if err != nil {
		t.Fatal(err)
	}
	heavy, err := ParseHeavyUnits(string(content))
	if err != nil || len(heavy) != 1 {
		t.Fatalf("heavy units %+v (%v)", heavy, err)
	}
	cases := []struct {
		name   string
		events []TestEvent
		failed bool
	}{
		{"the declared test", heavySkip("TestCompilerAndStage1Agree_CheckerSanitized", "heavy: deferred to main's whole gate"), false},
		{"a subtest of it", heavySkip("TestCompilerAndStage1Agree_CheckerSanitized/all", "heavy: deferred"), false},
		{"a shard of its family", heavySkip("TestCompilerAndStage1Agree_CheckerSanitizedUnit03", "heavy: deferred"), false},
		{"an undeclared heavy deferral", heavySkip("TestSomethingElse", "heavy: deferred"), true},
		{"a bare prefix is not its family", heavySkip("TestCompilerAndStage1Agree_CheckerSanitizedExtra", "heavy: deferred"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			result := Census(c.events, nil, heavy, nil, "linux")
			if result.Failed() != c.failed || (!c.failed && result.Heavy != 1) {
				t.Fatalf("%s failed %v, want failed %v\n%s", result.Summary(), result.Failed(), c.failed, strings.Join(result.Lines, "\n"))
			}
		})
	}
	// With no heavy-units.tsv, a heavy deferral is never excused.
	if result := Census(heavySkip("TestCompilerAndStage1Agree_CheckerSanitized", "heavy: deferred"), nil, nil, nil, "linux"); !result.Failed() {
		t.Fatal("a heavy deferral passed with nothing declaring it")
	}
	for _, bad := range []string{"p\tTestA\towner\t10\n", "p\tTestA\towner\tten\twhy\n", "p\tTest*\towner\t10\twhy\n", "p\tTestA\to\t10\twhy\np\tTestA\to\t10\twhy\n"} {
		if _, err := ParseHeavyUnits(bad); err == nil {
			t.Fatalf("read %q as heavy units", bad)
		}
	}
}

func TestAnAmbiguousHeavyDeclarationFailsClosed(t *testing.T) {
	heavy := []HeavyUnit{{Package: lintPackage, Test: "TestA", Owner: "o", Seconds: 10, Why: "w"}, {Package: lintPackage, Test: "TestA/sub", Owner: "o", Seconds: 10, Why: "w"}}
	if result := Census(heavySkip("TestA/sub", "heavy: deferred"), nil, heavy, nil, "linux"); !result.Failed() {
		t.Fatalf("%s: a skip two declarations claim was excused", result.Summary())
	}
}
