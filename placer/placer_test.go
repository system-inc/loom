package placer

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/protocol"
)

var (
	tree, base, gateTools   = strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	testRunner, phaseRunner = strings.Repeat("d", 64), strings.Repeat("e", 64)
	testKey, productKey     = strings.Repeat("1", 64), strings.Repeat("2", 64)
	phaseKey, reusedKey     = strings.Repeat("3", 64), strings.Repeat("4", 64)
)

type listedFutures []judge.PlannedFuture

func (futures listedFutures) Planned() ([]judge.PlannedFuture, error) { return futures, nil }

// fixturePools are Oct 10's two kinds of pool: a test pool with every toolchain, and the phase pool, phase only.
func fixturePools() []Pool {
	return []Pool{
		{PoolEntry: judge.PoolEntry{Name: "codex-strict", Runner: testRunner, MemoryMegabytes: 16384, Cpus: 4, Cold: true}, Has: []string{"go", "clang", "node", "wasiSdk"}},
		{PoolEntry: judge.PoolEntry{Name: "box-phase", Runner: phaseRunner, MemoryMegabytes: 65536, Cpus: 8, Kinds: []string{"phase"}}, Has: []string{"go", "clang", "node"}},
	}
}

func gateEnv() map[string]string {
	env := map[string]string{}
	for name, value := range planner.GateEnvironment {
		env[name] = value
	}
	return env
}

func plannedUnit(t *testing.T, key, name, decision string, parts planner.KeyParts) judge.PlannedUnitWire {
	t.Helper()
	encoded, err := json.Marshal(parts)
	if err != nil {
		t.Fatal(err)
	}
	return judge.PlannedUnitWire{UnitKey: key, Name: name, KeyParts: encoded, Decision: decision}
}

// everyKind is a plan of one unit of each kind the planner keys, and one reused unit, which runs nowhere.
func everyKind(t *testing.T) []judge.PlannedUnitWire {
	tools := planner.Tools{Go: "go1.27.0", Clang: "clang 19", Node: "v22"}
	testTools, phaseTools := tools, tools
	testTools.Runner, phaseTools.Runner = testRunner, phaseRunner
	return []judge.PlannedUnitWire{
		plannedUnit(t, testKey, protocol.AdamicModule+"/internal/oracle", "run", planner.KeyParts{Kind: "test", Package: protocol.AdamicModule + "/internal/oracle",
			Select: planner.Select{Run: "^(TestA)$"}, Tools: testTools, Env: gateEnv(), GateInputs: strings.Repeat("f", 64)}),
		// A product key names no runner (ProductKeys), so any pool that takes products may run it.
		plannedUnit(t, productKey, "product:lower", "run", planner.KeyParts{Kind: "product", Package: protocol.AdamicModule + "/internal/lower",
			Select: planner.Select{Run: "^TestProduct_Lower$"}, Tools: tools, Env: gateEnv()}),
		plannedUnit(t, phaseKey, "phase:vet", "run", planner.KeyParts{Kind: "phase", Package: protocol.AdamicModule,
			Select: planner.Select{Run: "vet"}, Tools: phaseTools, Env: gateEnv(), GateTools: gateTools}),
		plannedUnit(t, reusedKey, protocol.AdamicModule+"/internal/upper", "reuse", planner.KeyParts{Kind: "test", Package: protocol.AdamicModule + "/internal/upper", Tools: testTools}),
	}
}

// harness is a Placer over fakes: every start and void it would make is kept, nothing reaches a network.
type harness struct {
	placer     *Placer
	placements []Placement
	voids      []string
	now        time.Time
}

func newHarness(t *testing.T, source judge.FutureSource, ledger Ledger) *harness {
	h := &harness{now: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)}
	h.placer = &Placer{
		Source:      source,
		Carried:     func(judge.PlannedFuture, int) ([]judge.CarriedUnit, error) { return nil, nil },
		ChangePaths: func(string) ([]string, error) { return nil, nil },
		Pools:       func() ([]Pool, error) { return fixturePools(), nil },
		Needs:       func() (planner.UnitNeeds, error) { return planner.UnitNeeds{}, nil },
		Start: func(placement Placement) error {
			h.placements = append(h.placements, placement)
			return nil
		},
		RunStarted: func(string) (bool, error) { return true, nil },
		Void: func(future judge.PlannedFuture, attempt int, cause string) error {
			h.voids = append(h.voids, cause)
			return nil
		},
		Ledger:     ledger,
		UnfitEvery: 30 * time.Minute,
		Now:        func() time.Time { return h.now },
		Log:        io.Discard,
	}
	return h
}

func (h *harness) placeOnce(t *testing.T, want int) {
	t.Helper()
	started, err := h.placer.PlaceOnce()
	if err != nil || started != want {
		t.Fatalf("started %d runs (%v), want %d", started, err, want)
	}
}

func unitOf(t *testing.T, placement Placement, key string) (protocol.JobUnit, PlacedUnit) {
	t.Helper()
	for index, unit := range placement.Job.Units {
		if unit.Id == key {
			return unit, placement.Units[index]
		}
	}
	t.Fatalf("run %s doesn't place unit %.8s", placement.Run, key)
	return protocol.JobUnit{}, PlacedUnit{}
}

// A plan with test, product and phase units is placed whole in the attempt's run, each unit as its key's job under
// its own id (what the judge reads), on the pools that take it: the test and product on the test pool, the phase on
// the phase pool only. The reused unit runs nowhere.
func TestAPlanOfEveryKindPlacesEachOnAPoolThatTakesIt(t *testing.T) {
	future := judge.PlannedFuture{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)}
	ledger := &MemoryLedger{}
	h := newHarness(t, listedFutures{future}, ledger)
	h.placeOnce(t, 1)
	if len(h.voids) != 0 || len(h.placements) != 1 {
		t.Fatalf("voids %v, placements %d", h.voids, len(h.placements))
	}
	placement := h.placements[0]
	if placement.Run != "future-"+tree+"-1" || len(placement.Job.Units) != 3 {
		t.Fatalf("run %s with %d units, want future-<tree>-1 with the three that run", placement.Run, len(placement.Job.Units))
	}
	test, testPlaced := unitOf(t, placement, testKey)
	if test.Kind != "test" || test.Test.Sha != tree || test.Test.Packages[0].Run != "^(TestA)$" || test.TimeoutSeconds != protocol.KindCeilings["test"] ||
		strings.Join(testPlaced.Pools, ",") != "codex-strict" {
		t.Errorf("the test unit is %+v on %v", test, testPlaced.Pools)
	}
	product, productPlaced := unitOf(t, placement, productKey)
	if product.Kind != "product" || product.Test.Packages[0].Run != "^TestProduct_Lower$" || product.TimeoutSeconds != protocol.KindCeilings["product"] ||
		strings.Join(productPlaced.Pools, ",") != "codex-strict" {
		t.Errorf("the product unit is %+v on %v", product, productPlaced.Pools)
	}
	phase, phasePlaced := unitOf(t, placement, phaseKey)
	if phase.Kind != "phase" || phase.Test.Phase != "vet" || phase.Test.Base != base || phase.Test.Tools != gateTools || len(phase.Outputs) != 0 ||
		strings.Join(phase.Requires, ",") != "go,clang,node" || strings.Join(phasePlaced.Pools, ",") != "box-phase" {
		t.Errorf("the phase unit is %+v on %v", phase, phasePlaced.Pools)
	}
	if poolNames(placement.Pools) != "box-phase=1 codex-strict=2" {
		t.Errorf("the run's pools are %s, want each pool with a slot per unit it may take", poolNames(placement.Pools))
	}
	if record, found := ledger.Find(tree, 1); !found || len(record.Placed) != 3 || record.Reused != 1 {
		t.Errorf("the ledger holds %+v, want three placed and one reused", record)
	}
}

// A unit needing a toolchain no pool has stops the whole attempt: nothing starts, and the attempt is posted void as
// Loom's naming the unit and what each pool lacks. The next attempt failing the same way waits out UnfitEvery, then
// is voided again, so a future no pool serves is said every half hour, never every pass and never not at all.
func TestAUnitNeedingAToolNoPoolHasIsReportedNotDropped(t *testing.T) {
	units := everyKind(t)
	// The phase unit now needs the WASI SDK, which only the test pool has.
	var parts planner.KeyParts
	json.Unmarshal(units[2].KeyParts, &parts)
	parts.Tools.WasiSdk = "wasi-sdk-25"
	units[2] = plannedUnit(t, phaseKey, "phase:vet", "run", parts)
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: units}}
	ledger := &MemoryLedger{}
	h := newHarness(t, source, ledger)
	h.placeOnce(t, 0)
	if len(h.placements) != 0 || len(h.voids) != 1 {
		t.Fatalf("placements %d, voids %v: want nothing started and one void", len(h.placements), h.voids)
	}
	if cause := h.voids[0]; !strings.Contains(cause, "phase unit phase:vet") || !strings.Contains(cause, "box-phase lacks wasiSdk") {
		t.Fatalf("the void says %q, want the unit and the toolchain its pool lacks", cause)
	}
	if record, found := ledger.Find(tree, 1); !found || record.Void != h.voids[0] || len(record.Unplaced) != 1 || len(record.Placed) != 0 {
		t.Fatalf("the ledger holds %+v", record)
	}
	h.placeOnce(t, 0)
	source[0].Attempt = 2
	h.now = h.now.Add(10 * time.Minute)
	h.placeOnce(t, 0)
	if len(h.voids) != 1 {
		t.Fatalf("voids %v: the same attempt or the same cause within the window was voided again", h.voids)
	}
	h.now = h.now.Add(25 * time.Minute)
	h.placeOnce(t, 0)
	if len(h.voids) != 2 || len(h.placements) != 0 {
		t.Fatalf("voids %v after the window, want attempt 2 voided too", h.voids)
	}
}

// gofmt's phase unit, keyed on the change's paths, is placed on the phase pool carrying them; paths that don't hash
// to its key void the attempt naming the mismatch.
func TestTheGofmtPhaseIsPlacedCarryingTheChangesPaths(t *testing.T) {
	units := everyKind(t)
	var parts planner.KeyParts
	json.Unmarshal(units[2].KeyParts, &parts)
	parts.Select.Run = protocol.GofmtPhase
	parts.Env["ADAMIC_GATE_CHANGED"] = planner.ChangedPathsSum([]string{"b.go", "a.go"})
	units[2] = plannedUnit(t, phaseKey, "phase:gofmt", "run", parts)
	future := judge.PlannedFuture{Future: tree, Base: base, Attempt: 1, Change: judge.PlannedChange{Change: "chg_A"}, Units: units}
	h := newHarness(t, listedFutures{future}, &MemoryLedger{})
	h.placer.ChangePaths = func(change string) ([]string, error) { return []string{"a.go", "b.go"}, nil }
	h.placeOnce(t, 1)
	gofmt, placed := unitOf(t, h.placements[0], phaseKey)
	if gofmt.Test.Phase != protocol.GofmtPhase || strings.Join(gofmt.Test.ChangedPaths, ",") != "a.go,b.go" || strings.Join(placed.Pools, ",") != "box-phase" {
		t.Fatalf("gofmt is %+v on %v", gofmt.Test, placed.Pools)
	}
	other := newHarness(t, listedFutures{future}, &MemoryLedger{})
	other.placer.ChangePaths = func(change string) ([]string, error) { return []string{"a.go"}, nil }
	other.placeOnce(t, 0)
	if len(other.voids) != 1 || !strings.Contains(other.voids[0], `phase unit phase:gofmt: phase "gofmt" is keyed on changed paths`) {
		t.Fatalf("voids %v, want the mismatch named", other.voids)
	}
}

// The ledger outlives the placer: a restart reads it and starts nothing it recorded, a crash's cut-short last line
// included, which never finished and so never started anything. While one placer holds it, another can't open it.
func TestARestartPlacesNothingTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "placed.jsonl")
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)}}
	ledger, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	newHarness(t, source, ledger).placeOnce(t, 1)
	// A second placer beside the first is refused before it reads a record: both would start every attempt.
	if second, err := OpenLedger(path); err == nil || !strings.Contains(err.Error(), "another placer holds it") {
		t.Fatalf("a second placer took the held ledger (%v)", err)
	} else if second != nil {
		t.Fatal("a refused ledger came back")
	}
	ledger.Close()
	file, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	file.WriteString(`{"future":"` + strings.Repeat("9", 40) + `","attempt":1,"ru`)
	file.Close()
	reopened, err := OpenLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := newHarness(t, source, reopened)
	restarted.placeOnce(t, 0)
	if len(restarted.placements) != 0 || len(restarted.voids) != 0 {
		t.Fatalf("after a restart: placements %d, voids %v", len(restarted.placements), restarted.voids)
	}
	if content, _ := os.ReadFile(path); strings.Count(string(content), "\n") != 1 || !strings.HasSuffix(string(content), "}\n") {
		t.Fatalf("the ledger is %q, want its one whole line", content)
	}
}

// After a void Queue lists the next attempt, and it is placed in a fresh run, future-<tree>-2, without the unit the
// judge carries from attempt 1; attempt 1 is never placed again.
func TestASecondAttemptAfterAVoidGetsAFreshRun(t *testing.T) {
	source := listedFutures{{Future: tree, Base: base, Attempt: 1, Units: everyKind(t)}}
	h := newHarness(t, source, &MemoryLedger{})
	h.placeOnce(t, 1)
	source[0].Attempt = 2
	h.placer.Carried = func(future judge.PlannedFuture, attempt int) ([]judge.CarriedUnit, error) {
		if attempt != 2 {
			t.Errorf("asked what attempt %d carries", attempt)
		}
		return []judge.CarriedUnit{{UnitKey: productKey, Run: "future-" + tree + "-1"}}, nil
	}
	h.placeOnce(t, 1)
	h.placeOnce(t, 0)
	if len(h.placements) != 2 || h.placements[0].Run != "future-"+tree+"-1" || h.placements[1].Run != "future-"+tree+"-2" {
		t.Fatalf("placements %d, want runs -1 then -2", len(h.placements))
	}
	second := h.placements[1]
	if len(second.Job.Units) != 2 || second.Attempt != 2 {
		t.Fatalf("attempt 2 placed %d units, want the two the judge doesn't carry", len(second.Job.Units))
	}
	for _, unit := range second.Job.Units {
		if unit.Id == productKey {
			t.Fatal("attempt 2 placed the carried product unit again")
		}
	}
}

// The coordinator places a unit on any pool of its run that takes its kind, size and toolchains, knowing no runner: a
// pool serving another runner, there only for a product unit, would take the test unit too. It leaves the run, and the
// product goes to the pool it shares with the test.
func TestARunHoldsNoPoolThatWouldTakeAUnitOnTheWrongRunner(t *testing.T) {
	units := everyKind(t)[:2]
	h := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: units}}, &MemoryLedger{})
	h.placer.Pools = func() ([]Pool, error) {
		stale := Pool{PoolEntry: judge.PoolEntry{Name: "stale-strict", Runner: strings.Repeat("0", 64), MemoryMegabytes: 16384, Cpus: 4}, Has: []string{"go", "clang", "node"}}
		return append(fixturePools(), stale), nil
	}
	h.placeOnce(t, 1)
	placement := h.placements[0]
	if _, product := unitOf(t, placement, productKey); strings.Join(product.Pools, ",") != "codex-strict" || poolNames(placement.Pools) != "codex-strict=2" {
		t.Fatalf("the product may go to %v, the run's pools are %s", product.Pools, poolNames(placement.Pools))
	}
}

// A unit keyed on the change's paths carries them, as the change's record names them, only when they hash to its key.
func TestAUnitKeyedOnChangedPathsCarriesThemOnlyWhenTheyMatch(t *testing.T) {
	units := everyKind(t)
	var parts planner.KeyParts
	json.Unmarshal(units[0].KeyParts, &parts)
	parts.Env["ADAMIC_GATE_CHANGED"] = planner.ChangedPathsSum([]string{"b.go", "a.go"})
	units[0] = plannedUnit(t, testKey, "oracle", "run", parts)
	future := judge.PlannedFuture{Future: tree, Base: base, Attempt: 1, Change: judge.PlannedChange{Change: "chg_A"}, Units: units}
	h := newHarness(t, listedFutures{future}, &MemoryLedger{})
	h.placer.ChangePaths = func(change string) ([]string, error) { return []string{"a.go", "b.go"}, nil }
	h.placeOnce(t, 1)
	if test, _ := unitOf(t, h.placements[0], testKey); strings.Join(test.Test.ChangedPaths, ",") != "a.go,b.go" {
		t.Fatalf("the unit carries %v", test.Test.ChangedPaths)
	}
	other := newHarness(t, listedFutures{future}, &MemoryLedger{})
	other.placer.ChangePaths = func(change string) ([]string, error) { return []string{"a.go"}, nil }
	other.placeOnce(t, 0)
	if len(other.voids) != 1 || !strings.Contains(other.voids[0], "keyed on changed paths") {
		t.Fatalf("voids %v, want the mismatch named", other.voids)
	}
}

// A build unit (Kirk's build law, #8j1qygw) needs go on its workers whatever its key names, since its tests build: it
// goes only to a pool that has go, as a build job on the tree, and with none it voids the attempt naming what's
// missing. Mutant: the placer reading only the key's requirements.
func TestABuildUnitGoesOnlyToAPoolWithGo(t *testing.T) {
	buildKey := strings.Repeat("9", 64)
	units := everyKind(t)[:1]
	units = append(units, plannedUnit(t, buildKey, protocol.AdamicModule+"/internal/native", "run", planner.KeyParts{Kind: "build",
		Package: protocol.AdamicModule + "/internal/native", Select: planner.Select{Run: "^(TestBuildsTheArchive)$"},
		Tools: planner.Tools{Runner: testRunner}, Env: gateEnv(), GateInputs: strings.Repeat("f", 64)}))
	h := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: units}}, &MemoryLedger{})
	h.placeOnce(t, 1)
	build, placed := unitOf(t, h.placements[0], buildKey)
	if build.Kind != "build" || !build.Test.Build || build.TimeoutSeconds != protocol.KindCeilings["build"] || strings.Join(placed.Pools, ",") != "codex-strict" {
		t.Fatalf("the build unit is %+v on %v", build, placed.Pools)
	}
	// The build unit alone, its key naming no toolchain, on a pool without go.
	goless := newHarness(t, listedFutures{{Future: tree, Base: base, Attempt: 1, Units: units[1:]}}, &MemoryLedger{})
	goless.placer.Pools = func() ([]Pool, error) {
		pools := fixturePools()
		pools[0].Has = []string{"clang", "node", "wasiSdk"}
		return pools, nil
	}
	goless.placeOnce(t, 0)
	if len(goless.voids) != 1 || !strings.Contains(goless.voids[0], "build unit") || !strings.Contains(goless.voids[0], "codex-strict lacks go") {
		t.Fatalf("voids %v, want the build unit refused naming the missing go", goless.voids)
	}
}
