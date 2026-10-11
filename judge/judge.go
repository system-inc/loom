// Package judge decides a unit's verdict by written rule, and a run's verdict from its units' (Loom's contract v1
// §3, the verdict record, #mvyq8h6). No mind sits in the loop: every decision is a pure function of the evidence
// handed in, and Rule names the rule set that made it, so a replay of the event log decides the same way. A rule
// changes only by landing code here, never by a verdict (contract v1 §4, rule.changed).
//
// A unit's first attempt either passes, breaks (infra: retried, never a verdict) or fails. A failure is rerun alone,
// once on the candidate and once on main at the future's base, and the four outcomes are decided by the table in
// Decide:
//
//	candidate alone  main alone  cause
//	failed           passed      change   the owner hears red
//	failed           failed      mainRed  excluded from the change's green, only on main's record (see Decide)
//	passed           passed      flake    passed, the test quarantined by event and its owner tasked
//	passed           failed      mainRed  the same exclusion as above
//
// Loom's rulings on the open rows (Oct 9, 23:15Z): a mainRed is excluded only when main's latest recorded verdict
// at the future's base fails the same package and test, and that test is the unit's only failing one; otherwise the
// failure is the change's. A flake is passed with cause flake, and every flake is recorded so flakes can be counted
// per test, because a flake the change introduced looks the same.
//
// A witness of main (base = sha) has no candidate apart from main: both reruns run its one tree, so only failed alone
// both times is red, and a failure that passes alone either time is a flake (#r0xntgv).
package judge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Rule is this rule set's id and version, written into every verdict's rule field.
const Rule = "judge-v1"

// PhaseCantJudge is the exit a gate phase gives when it can't judge (run.py's exit 2): the phase is void, never red.
const PhaseCantJudge = 2

// The statuses, causes and infra kinds of contract v1 §3.
const (
	Passed = "passed"
	Failed = "failed"
	Void   = "void"
	Broken = "broken" // an attempt's status when the machine, not the code, ended it

	CauseChange  = "change"
	CauseMainRed = "mainRed"
	CauseFlake   = "flake"
	CauseInfra   = "infra"

	InfraDisk        = "disk"
	InfraKill        = "kill" // over budget or signal: killed; never the change's red
	InfraNeverPlaced = "neverPlaced"
	InfraRefused     = "refused"
	InfraSilent      = "silent"
	// InfraNeedChanged is a failure whose declared need grew after its first placement (Release, Oct 10 02:26Z): its
	// alone reruns would run with more than the failing attempt had, so they can't judge it. Void, and the next
	// attempt runs whole at the declared need. Never a flake, which would quarantine tests that were only under-placed.
	InfraNeedChanged = "needChanged"
	// InfraBelowNeed is a failure that ran on less than its declared need (Loom, Oct 10 02:37Z: a 16-cpu unit placed
	// on a 4-cpu Codex instance, since the coordinator checked memory and not cpus). Its red proves nothing about the
	// change, and its reruns can't fix the placement it was judged on. Void, and the next attempt places it right.
	InfraBelowNeed = "belowNeed"
	// InfraWarmCache is an attempt that ran on a warm shared Go cache (Release, Oct 10 02:43Z: cloud-box-2's per-worker
	// cache, warm from attempt 3, served f5695d12 run -1's units). A warm pass is the stale-green the cold rule stops,
	// and a warm red is no cleaner: evidence, never the verdict. Void, and placed again on a cold pool.
	InfraWarmCache = "warmCache"
	// InfraOverBudget is a unit the runner stopped over its budget, 30 s to ready or 60 s to run (Kirk, Oct 10
	// 03:0xZ). It's Loom's, not the change's: the unit was planned or built too big. Its future never lands on it,
	// never green and never a flake, and it isn't retried or rerun alone, since the same unit runs over again; Planner
	// splits the test and the future reruns.
	InfraOverBudget = "overBudget"

	OverBudgetReady = "overBudgetReady"
	OverBudgetRun   = "overBudgetRun"

	// WarningOverBudget is a warning's kind for a test unit that passed past RunBudgetSeconds.
	WarningOverBudget = "overBudget"
	// OverBudgetDeadline is the runner's own deadline on the unit's exit event (timedOut), its budget's hard ceiling.
	OverBudgetDeadline = "deadline"
)

// RunBudgetSeconds is the build law's run budget (Kirk, Oct 10 21:5xZ): a test unit that passes past it is green with a
// warning, named and its owner tasked, never killed for being slow. Only the ceiling, 15 minutes, ends a hang.
const RunBudgetSeconds = 60.0

// SlowdownRatio is the margin of the law's second rule, so noise isn't a red: a branch made a unit slow when its alone
// rerun on the candidate passes past RunBudgetSeconds and past this many times its alone rerun on main's base, which
// passed within the budget.
const SlowdownRatio = 1.5

var infraKinds = map[string]bool{InfraDisk: true, InfraKill: true, InfraNeverPlaced: true, InfraRefused: true, InfraSilent: true}

// An Attempt is one run of a unit on one machine.
type Attempt struct {
	Machine string `json:"machine"`
	Runner  string `json:"runner"`
	// RunnerSha256 is the runner binary's sha256 its started event reported, or RunnerUnreported for a runner that
	// doesn't send it yet, so the gap is named on the record.
	RunnerSha256 string  `json:"runnerSha256"`
	StartedAt    string  `json:"startedAt"`
	FinishedAt   string  `json:"finishedAt"`
	Exit         int     `json:"exit"`
	WallSeconds  float64 `json:"wallSeconds"`
	Status       string  `json:"status"` // passed, failed or broken
}

// A TestOutcome is one test's result inside a unit.
type TestOutcome struct {
	Package string `json:"package"`
	Test    string `json:"test"`
	Outcome string `json:"outcome"` // pass, fail or skip
}

// A Verdict is the record of contract v1 §3. An empty Cause or Infra is written as null.
type Verdict struct {
	UnitKey   string        `json:"unitKey"`
	Change    string        `json:"change"`
	Future    string        `json:"future"`
	Run       string        `json:"run"`
	Status    string        `json:"status"`
	Cause     string        `json:"cause"`
	Infra     string        `json:"infra"`
	Attempts  []Attempt     `json:"attempts"`
	Tests     []TestOutcome `json:"tests"`
	Outputs   []string      `json:"outputs"`
	RuleId    string        `json:"rule"`
	DecidedAt string        `json:"decidedAt"`
	// Warnings are what a verdict's status doesn't say: a test unit that passed past its budget. Written only when
	// there is one, so a record without a warning reads as it did before the build law.
	Warnings []Warning `json:"warnings,omitempty"`
	// censusFailing names the skips that failed the census when RuleId is RuleCensus ("<class> <package> <test>"),
	// for the kick; the record carries them as its tests' skip outcomes.
	censusFailing []string
	// reusedTests is the tests object of the verdict a reused unit reuses, written as the record's tests in its place.
	reusedTests json.RawMessage
	// slowdown is why a unit is red for the branch making it slow, for the kick; empty otherwise.
	slowdown string
}

// A Warning is one thing a verdict carries beside its status: a pass past the run budget, with the wall that went
// over and, when the alone reruns ran, main's base's wall for the same unit.
type Warning struct {
	Kind          string  `json:"kind"`
	WallSeconds   float64 `json:"wallSeconds"`
	BudgetSeconds float64 `json:"budgetSeconds"`
	// BaseWallSeconds is the unit's alone rerun on main's base, when it ran; zero when there's none to compare.
	BaseWallSeconds float64 `json:"baseWallSeconds,omitempty"`
}

// TestsRef is a record's tests field, by reference (Loom's ruling, Oct 10 01:16Z): a Durable Object's SQLite value caps
// at 2 MB and one package's record ran to 750 KB inline. The whole list is a blob in the action store, read tokenless at
// artifacts.loom.system.inc/blobs/<sha256>; the record carries its hash, the counts, and inline only the tests that
// failed or never ended, so a red names its test without a fetch.
type TestsRef struct {
	Sha256  string        `json:"sha256"`
	Passed  int           `json:"passed"`
	Failed  int           `json:"failed"`
	Skipped int           `json:"skipped"`
	Inline  []TestOutcome `json:"inline"`
}

// testsRow is one row of the tests list, its fields in key order so the list's JSON is canonical.
type testsRow struct {
	Outcome string `json:"outcome"`
	Package string `json:"package"`
	Test    string `json:"test"`
}

// TestsList is the canonical tests list blob for tests, every row sorted by package then test as canonical JSON (keys
// sorted, no whitespace), and the reference a record carries to it. Never-ended rows (outcome run) are in the list and
// inline, and in no count.
func TestsList(tests []TestOutcome) ([]byte, TestsRef) {
	rows := make([]testsRow, 0, len(tests))
	ref := TestsRef{Inline: []TestOutcome{}}
	for _, outcome := range tests {
		rows = append(rows, testsRow{Outcome: outcome.Outcome, Package: outcome.Package, Test: outcome.Test})
		switch outcome.Outcome {
		case "pass":
			ref.Passed++
		case "skip":
			ref.Skipped++
		case "fail":
			ref.Failed++
			ref.Inline = append(ref.Inline, outcome)
		default:
			ref.Inline = append(ref.Inline, outcome)
		}
	}
	sort.SliceStable(rows, func(left, right int) bool {
		if rows[left].Package != rows[right].Package {
			return rows[left].Package < rows[right].Package
		}
		return rows[left].Test < rows[right].Test
	})
	content, _ := json.Marshal(rows) // rows of three strings always encode
	sum := sha256.Sum256(content)
	ref.Sha256 = hex.EncodeToString(sum[:])
	return content, ref
}

// Canonical writes the verdict as contract v1 asks of every record: sorted keys, no insignificant whitespace,
// empty lists as [], an empty cause or infra as null, and its tests by reference (TestsRef), whose list blob must be in
// the store before the record is posted.
func (verdict Verdict) Canonical() ([]byte, error) {
	type plain Verdict
	encoded, err := json.Marshal(plain(verdict))
	if err != nil {
		return nil, err
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"attempts", "outputs"} {
		if fields[name] == nil {
			fields[name] = []any{}
		}
	}
	_, ref := TestsList(verdict.Tests)
	encodedRef, err := json.Marshal(ref)
	if err != nil {
		return nil, err
	}
	if verdict.reusedTests != nil {
		encodedRef = verdict.reusedTests
	}
	var tests map[string]any
	if err := json.Unmarshal(encodedRef, &tests); err != nil {
		return nil, err
	}
	fields["tests"] = tests
	for _, name := range []string{"cause", "infra"} {
		if fields[name] == "" {
			fields[name] = nil
		}
	}
	// encoding/json writes a map's keys sorted, at every depth.
	return json.Marshal(fields)
}

// A Rerun is one alone rerun of a failed unit: on the candidate, or on main at the future's base.
type Rerun struct {
	Status       string        // passed, failed or broken
	Infra        string        // the infra kind when Status is broken
	Tests        []TestOutcome // its test outcomes
	RunnerSha256 string        // the runner binary it ran on, as its attempt reported it
	OverBudget   string        // the runner's budget cause when it stopped the rerun over budget
	WallSeconds  float64       // its exit event's wall
}

// RunnerUnreported is an attempt's RunnerSha256 when its runner didn't send one.
const RunnerUnreported = "unreported"

// runnerMismatch says why an attempt can't be a verdict on its key, or "" when it can: it ran on a runner binary other
// than the one its key names (Loom, Oct 10 01:52Z: no pass is stored under a runner that didn't compute it). A runner
// that reports nothing is accepted, the gap on the record, until requireReported (the logged fail-closed switch, a
// cutover condition). A key with no runner part names nothing to check.
func runnerMismatch(keyRunner, ran string, requireReported bool) string {
	switch {
	case keyRunner == "":
		return ""
	case ran == "" || ran == RunnerUnreported:
		if requireReported {
			return "its runner reported no sha256, and the judge requires one"
		}
		return ""
	case ran != keyRunner:
		return fmt.Sprintf("it ran on runner %.12s, and its key names runner %.12s", ran, keyRunner)
	}
	return ""
}

// Evidence is everything Decide reads for one unit.
//
// Infra comes from structure, never from text (Loom's binding rule, from Release's mutant inventory #1c7z4dh): an
// attempt is broken, and a kill is infra, only when the runner's own exit event says so (the signal it observed, or
// its deadline), which the runner records as First.Status broken with FirstInfra. Decide never reads what a test
// printed. The old path's provenreds.py reads "signal: killed" from test output, which a test can print itself (the
// binary-dies-partway mutant panics at 50 ms saying "own work exceeded 90s" and must read red); that weakness isn't
// carried over.
type Evidence struct {
	First      Attempt
	FirstInfra string        // the infra kind when First.Status is broken, from the runner's exit event
	FirstTests []TestOutcome // the first attempt's test outcomes
	// MissingTools names the toolchains the unit needs that its runner lacked (the wasi SDK, say). A unit run without
	// them proves nothing, whatever it reported: its skips aren't passes, so it's void and placed again (the
	// wasi-family-must-run mutant).
	MissingTools []string
	// KeyRunner is the unit key's tools.runner part, the runner binary's sha256; RequireRunner turns a runner that
	// reports none from accepted into void.
	KeyRunner     string
	RequireRunner bool
	// Phase marks a kind phase unit, one of the box fast gate's non-test stages (Loom, Oct 10 01:41Z): decided by its
	// exit, as the box decides a stage, with no alone reruns.
	Phase bool
	// NeedGrew says how the unit's declared need now exceeds what its first attempt was placed with; empty when it
	// doesn't, or before the loop has asked (it asks only when a failure would go to alone reruns).
	NeedGrew string
	// FirstOverBudget is the first attempt's budget cause when the runner stopped it over budget.
	FirstOverBudget string
	// Warm marks a first attempt listed as run on a warm shared cache; it's placed again before anything decides.
	Warm bool
	// BelowNeed says how the unit's declared need exceeds what its first attempt's runner reported it ran with.
	BelowNeed string
	Candidate *Rerun // the unit rerun alone on the candidate; nil until it has run
	Main      *Rerun // the unit rerun alone on main at the future's base; nil until it has run
	// SameTree marks a future whose base is its own tree, a witness of main (base = sha): its two alone reruns run one
	// tree twice, so there is no change between them to blame.
	SameTree bool
	// Budgeted marks a test unit, held to RunBudgetSeconds; phases and products aren't.
	Budgeted bool
	// MainRecorded is main's latest recorded verdict for this unit at the future's base, its test outcomes; nil when
	// main has none.
	MainRecorded []TestOutcome
}

// A Decision is what Decide says of one unit: a verdict status and cause, or what has to run first.
type Decision struct {
	Decided bool
	Status  string
	Cause   string
	Infra   string
	Next    string        // when not decided: "rerunAlone" (both reruns), or "retry" for infra
	Flaky   []TestOutcome // the tests to quarantine when the cause is flake
	Why     string        // the table row that decided it, for the record and the log
	// Warnings go on the record beside its status: a pass past the run budget.
	Warnings []Warning
}

// Decide applies the rule table to one unit's evidence.
func Decide(evidence Evidence) (Decision, error) {
	if len(evidence.MissingTools) > 0 {
		return Decision{Status: Void, Cause: CauseInfra, Infra: InfraRefused, Next: "retry",
			Why: "run without " + strings.Join(evidence.MissingTools, ", ") + ": its skips prove nothing; place it on a fit runner"}, nil
	}
	if evidence.First.Status != Broken {
		if why := runnerMismatch(evidence.KeyRunner, evidence.First.RunnerSha256, evidence.RequireRunner); why != "" {
			return Decision{Status: Void, Cause: CauseInfra, Infra: InfraRefused, Next: "retry", Why: why + ": void, placed again"}, nil
		}
	}
	if evidence.Warm {
		return Decision{Status: Void, Cause: CauseInfra, Infra: InfraWarmCache, Next: "retry",
			Why: "ran on a warm shared cache: evidence, never the verdict; placed again cold"}, nil
	}
	if evidence.FirstOverBudget != "" {
		return overBudget("the attempt", evidence.FirstOverBudget), nil
	}
	if evidence.Phase {
		switch evidence.First.Status {
		case Passed, Failed:
			switch {
			case evidence.First.Exit == PhaseCantJudge:
				return Decision{Status: Void, Cause: CauseInfra, Infra: InfraRefused, Next: "retry",
					Why: "the phase exited 2, the gate's own can't-judge exit: void, placed again"}, nil
			case evidence.First.Status == Passed && evidence.First.Exit == 0:
				return Decision{Decided: true, Status: Passed, Why: "the phase exited 0"}, nil
			}
			return Decision{Decided: true, Status: Failed, Cause: CauseChange,
				Why: fmt.Sprintf("the phase exited %d: red, as the box reads a stage, with no alone rerun", evidence.First.Exit)}, nil
		}
		// A broken attempt falls through: a runner kill is infra like any unit's.
	}
	// A test that never reached a terminal action (pass, fail or skip) is red, never infra: the process ended under it
	// without the runner observing a kill, so the code ended it.
	for _, outcome := range evidence.FirstTests {
		switch outcome.Outcome {
		case "pass", "fail", "skip":
		default:
			if evidence.First.Status != Broken {
				evidence.First.Status = Failed
			}
		}
	}
	if evidence.First.Status == Passed && len(failing(evidence.FirstTests)) > 0 {
		// An attempt can't pass with a failing test in it.
		evidence.First.Status = Failed
	}
	switch evidence.First.Status {
	case Passed:
		if evidence.Budgeted && evidence.First.WallSeconds > RunBudgetSeconds {
			return slowPass(evidence), nil
		}
		return Decision{Decided: true, Status: Passed, Why: "first attempt passed"}, nil
	case Broken:
		if !infraKinds[evidence.FirstInfra] {
			return Decision{}, fmt.Errorf("a broken attempt names its infra kind, one of disk, kill, neverPlaced, refused or silent, not %q", evidence.FirstInfra)
		}
		return Decision{Status: Void, Cause: CauseInfra, Infra: evidence.FirstInfra, Next: "retry", Why: "first attempt broken: infra, retried, never a verdict"}, nil
	case Failed:
	default:
		return Decision{}, fmt.Errorf("an attempt's status is passed, failed or broken, not %q", evidence.First.Status)
	}
	if evidence.BelowNeed != "" {
		return Decision{Decided: true, Status: Void, Cause: CauseInfra, Infra: InfraBelowNeed,
			Why: "failed below its declared need (" + evidence.BelowNeed + "): proves nothing about the change; void, the next attempt places it at its need"}, nil
	}
	if evidence.NeedGrew != "" {
		return Decision{Decided: true, Status: Void, Cause: CauseInfra, Infra: InfraNeedChanged,
			Why: "failed, and its declared need grew since its placement (" + evidence.NeedGrew + "): reruns placed with more can't judge it; void, the next attempt runs whole at the declared need"}, nil
	}
	if evidence.Candidate == nil || evidence.Main == nil {
		return Decision{Next: "rerunAlone", Why: "failed: rerun alone on the candidate and on main before a cause is set"}, nil
	}
	for _, rerun := range []*Rerun{evidence.Candidate, evidence.Main} {
		if why := runnerMismatch(evidence.KeyRunner, rerun.RunnerSha256, evidence.RequireRunner); why != "" && rerun.Status != Broken {
			return Decision{Status: Void, Cause: CauseInfra, Infra: InfraRefused, Next: "retry", Why: "an alone rerun: " + why + ", rerun again"}, nil
		}
		if rerun.OverBudget != "" {
			return overBudget("an alone rerun", rerun.OverBudget), nil
		}
		switch rerun.Status {
		case Passed, Failed:
		case Broken:
			if !infraKinds[rerun.Infra] {
				return Decision{}, fmt.Errorf("a broken rerun names its infra kind, not %q", rerun.Infra)
			}
			return Decision{Status: Void, Cause: CauseInfra, Infra: rerun.Infra, Next: "retry", Why: "an alone rerun broke: infra, rerun again"}, nil
		default:
			return Decision{}, fmt.Errorf("a rerun's status is passed, failed or broken, not %q", rerun.Status)
		}
	}
	candidateFailed, mainFailed := evidence.Candidate.Status == Failed, evidence.Main.Status == Failed
	if evidence.SameTree && !(candidateFailed && mainFailed) {
		// One tree run three times that passed alone at least once: the table's candidate-or-main rows would name a
		// change that isn't there, and a witness's red holds main for everyone, so it's the tree's flake.
		return Decision{Decided: true, Status: Passed, Cause: CauseFlake, Flaky: failing(evidence.FirstTests),
			Why: "a witness's one tree failed, then passed alone at least once of its two alone reruns: a flake, quarantined and counted"}, nil
	}
	switch {
	case candidateFailed && !mainFailed:
		return Decision{Decided: true, Status: Failed, Cause: CauseChange, Why: "fails alone on the candidate, passes alone on main"}, nil
	case !candidateFailed && !mainFailed:
		return Decision{Decided: true, Status: Passed, Cause: CauseFlake, Flaky: failing(evidence.FirstTests),
			Why: "failed once, passes alone on the candidate and on main: a flake, quarantined and counted"}, nil
	}
	if evidence.SameTree {
		// A verify is main: its red is main's red, named in main.red, never excused against main's own earlier record
		// (#x3vaz9k, the fresh verify of ebdb6c53 at seq 2247 excused flow, load, selector and yaml against the verify
		// before it, so main.red named only scanner; had every failure matched, it would have read green on a red main).
		return Decision{Decided: true, Status: Failed, Cause: CauseChange,
			Why: "a verify's one tree fails alone twice: main's red, recorded as the verify's, never excused against main's earlier record"}, nil
	}
	// Main fails alone too. It's main's red only on main's record, and only if it's the unit's one failing test.
	if excused(evidence) {
		return Decision{Decided: true, Status: Failed, Cause: CauseMainRed, Why: "main's recorded verdict fails the same test, the unit's only failure"}, nil
	}
	return Decision{Decided: true, Status: Failed, Cause: CauseChange,
		Why: "main fails alone too, but main's record doesn't fail exactly this unit's one failing test, so it's the change's"}, nil
}

// slowPass decides a test unit whose first attempt passed past the run budget (the build law, Kirk Oct 10 21:5xZ): green
// with a warning, unless its alone reruns show the branch made it slow, its candidate rerun passing past the budget and
// past SlowdownRatio times main's base, which passed within it. That is the branch's red, with both walls. A verify has
// no base apart from its tree, so it's green with the warning and nothing reruns. Reruns that can't be compared (one
// failed, broke, ran on another runner or over budget) leave the pass a pass: slowness never voids a green.
func slowPass(evidence Evidence) Decision {
	warning := Warning{Kind: WarningOverBudget, WallSeconds: evidence.First.WallSeconds, BudgetSeconds: RunBudgetSeconds}
	green := Decision{Decided: true, Status: Passed, Warnings: []Warning{warning}}
	if evidence.SameTree {
		green.Why = fmt.Sprintf("passed in %.1f s, past its %.0f s budget: green with a warning; a verify has no base to compare", warning.WallSeconds, RunBudgetSeconds)
		return green
	}
	if evidence.Candidate == nil || evidence.Main == nil {
		return Decision{Next: "rerunAlone", Why: "passed past its budget: rerun alone on the candidate and on main's base to see whether the branch made it slow"}
	}
	candidate, base := evidence.Candidate, evidence.Main
	comparable := true
	for _, rerun := range []*Rerun{candidate, base} {
		if rerun.Status != Passed || rerun.OverBudget != "" || runnerMismatch(evidence.KeyRunner, rerun.RunnerSha256, evidence.RequireRunner) != "" {
			comparable = false
		}
	}
	green.Warnings[0].BaseWallSeconds = base.WallSeconds
	switch {
	case !comparable:
		green.Why = fmt.Sprintf("passed in %.1f s, past its %.0f s budget: green with a warning; its alone reruns can't be compared (candidate %s, base %s)",
			warning.WallSeconds, RunBudgetSeconds, candidate.Status, base.Status)
		green.Warnings[0].BaseWallSeconds = 0
	case candidate.WallSeconds > RunBudgetSeconds && base.WallSeconds <= RunBudgetSeconds && candidate.WallSeconds > SlowdownRatio*base.WallSeconds:
		return Decision{Decided: true, Status: Failed, Cause: CauseChange, Warnings: green.Warnings,
			Why: fmt.Sprintf("the branch made it slow: alone it passes in %.1f s on the candidate and %.1f s on main's base, past the %.0f s budget and past %.1fx the base",
				candidate.WallSeconds, base.WallSeconds, RunBudgetSeconds, SlowdownRatio)}
	default:
		green.Why = fmt.Sprintf("passed in %.1f s, past its %.0f s budget: green with a warning; alone %.1f s on the candidate and %.1f s on main's base, so the branch didn't make it slow",
			warning.WallSeconds, RunBudgetSeconds, candidate.WallSeconds, base.WallSeconds)
	}
	return green
}

// overBudget is the row for a unit the runner stopped over its budget: void, Loom's, decided with no retry.
func overBudget(which, cause string) Decision {
	if cause == OverBudgetDeadline {
		// The ceiling, 15 minutes under the build law, ends only a hang: a slow unit passes with a warning before it.
		return Decision{Decided: true, Status: Void, Cause: CauseInfra, Infra: InfraOverBudget,
			Why: which + " hung to the runner's ceiling: void, never red; nothing reruns, since a hang hangs again"}
	}
	return Decision{Decided: true, Status: Void, Cause: CauseInfra, Infra: InfraOverBudget,
		Why: which + " ran over its budget (" + cause + "): Loom's, never the change's; its test is split and the future reruns"}
}

// excused says whether a failure main shares is main's red (Loom's ruling, Oct 9 23:15Z, read with subtests): every test
// the unit failed, across its first attempt and its candidate rerun, belongs to one top-level test, and main's latest
// recorded verdict fails every one of them. A subtest is part of its top-level test, so a test failing with its subtests
// is one failure (internal/flow's TestFlowCorpusRemainder fails with each corpus file it reads); any failure main's
// record doesn't have, or a second top-level test, is the change's.
func excused(evidence Evidence) bool {
	unitFailures := map[string]bool{}
	topLevel := map[string]bool{}
	for _, outcome := range append(failing(evidence.FirstTests), failing(evidence.Candidate.Tests)...) {
		unitFailures[outcome.Package+" "+outcome.Test] = true
		top, _, _ := strings.Cut(outcome.Test, "/")
		topLevel[outcome.Package+" "+top] = true
	}
	if len(topLevel) != 1 {
		return false
	}
	mainFailures := map[string]bool{}
	for _, outcome := range failing(evidence.MainRecorded) {
		mainFailures[outcome.Package+" "+outcome.Test] = true
	}
	for failure := range unitFailures {
		if !mainFailures[failure] {
			return false
		}
	}
	return true
}

func failing(outcomes []TestOutcome) []TestOutcome {
	found := []TestOutcome{}
	for _, outcome := range outcomes {
		if outcome.Outcome == "fail" {
			found = append(found, outcome)
		}
	}
	return found
}

// A RunVerdict is the run's decision from its units' verdicts.
type RunVerdict struct {
	Status   string   `json:"status"`   // green, red or void
	Red      []string `json:"red"`      // unit keys whose verdict is the change's red
	Excused  []string `json:"excused"`  // unit keys failed as main's red, excluded from the green
	Problems []string `json:"problems"` // why the run is void, one line each
}

// Green decides a run: green only when the units' verdicts cover exactly the planned unit keys, once each, and
// every one passed, or failed as main's red. A missing, extra or repeated unit, or any void, makes the run void,
// never green. A failure caused by the change makes it red, ahead of any void: it was confirmed alone on the
// candidate and on main, so it's the change's whatever else is missing. A verdict for a unit outside the plan never
// counts toward red or green.
func Green(plan []string, verdicts []Verdict) RunVerdict {
	result := RunVerdict{Red: []string{}, Excused: []string{}, Problems: []string{}}
	planned := map[string]bool{}
	for _, key := range plan {
		planned[key] = true
	}
	seen := map[string]bool{}
	for _, verdict := range verdicts {
		switch {
		case !planned[verdict.UnitKey]:
			result.Problems = append(result.Problems, "unit "+verdict.UnitKey+" isn't in the plan")
			continue
		case seen[verdict.UnitKey]:
			result.Problems = append(result.Problems, "unit "+verdict.UnitKey+" has more than one verdict")
			continue
		}
		seen[verdict.UnitKey] = true
		switch {
		case verdict.Status == Passed:
		case verdict.Status == Failed && verdict.Cause == CauseChange:
			result.Red = append(result.Red, verdict.UnitKey)
		case verdict.Status == Failed && verdict.Cause == CauseMainRed:
			result.Excused = append(result.Excused, verdict.UnitKey)
		case verdict.Status == Void:
			result.Problems = append(result.Problems, "unit "+verdict.UnitKey+" is void ("+verdict.Infra+")")
		default:
			result.Problems = append(result.Problems, fmt.Sprintf("unit %s has status %q with cause %q, which no rule decides", verdict.UnitKey, verdict.Status, verdict.Cause))
		}
	}
	missing := []string{}
	for _, key := range plan {
		if !seen[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	for _, key := range missing {
		result.Problems = append(result.Problems, "unit "+key+" has no verdict")
	}
	switch {
	case len(result.Red) > 0:
		result.Status = "red"
	case len(result.Problems) > 0:
		result.Status = "void"
	default:
		result.Status = "green"
	}
	return result
}
