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
package judge

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Rule is this rule set's id and version, written into every verdict's rule field.
const Rule = "judge-v1"

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
)

var infraKinds = map[string]bool{InfraDisk: true, InfraKill: true, InfraNeverPlaced: true, InfraRefused: true, InfraSilent: true}

// An Attempt is one run of a unit on one machine.
type Attempt struct {
	Machine     string  `json:"machine"`
	Runner      string  `json:"runner"`
	StartedAt   string  `json:"startedAt"`
	FinishedAt  string  `json:"finishedAt"`
	Exit        int     `json:"exit"`
	WallSeconds float64 `json:"wallSeconds"`
	Status      string  `json:"status"` // passed, failed or broken
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
}

// Canonical writes the verdict as contract v1 asks of every record: sorted keys, no insignificant whitespace,
// empty lists as [], and an empty cause or infra as null.
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
	for _, name := range []string{"attempts", "tests", "outputs"} {
		if fields[name] == nil {
			fields[name] = []any{}
		}
	}
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
	Status string        // passed, failed or broken
	Infra  string        // the infra kind when Status is broken
	Tests  []TestOutcome // its test outcomes
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
	Candidate    *Rerun // the unit rerun alone on the candidate; nil until it has run
	Main         *Rerun // the unit rerun alone on main at the future's base; nil until it has run
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
}

// Decide applies the rule table to one unit's evidence.
func Decide(evidence Evidence) (Decision, error) {
	if len(evidence.MissingTools) > 0 {
		return Decision{Status: Void, Cause: CauseInfra, Infra: InfraRefused, Next: "retry",
			Why: "run without " + strings.Join(evidence.MissingTools, ", ") + ": its skips prove nothing; place it on a fit runner"}, nil
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
	if evidence.Candidate == nil || evidence.Main == nil {
		return Decision{Next: "rerunAlone", Why: "failed: rerun alone on the candidate and on main before a cause is set"}, nil
	}
	for _, rerun := range []*Rerun{evidence.Candidate, evidence.Main} {
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
	switch {
	case candidateFailed && !mainFailed:
		return Decision{Decided: true, Status: Failed, Cause: CauseChange, Why: "fails alone on the candidate, passes alone on main"}, nil
	case !candidateFailed && !mainFailed:
		return Decision{Decided: true, Status: Passed, Cause: CauseFlake, Flaky: failing(evidence.FirstTests),
			Why: "failed once, passes alone on the candidate and on main: a flake, quarantined and counted"}, nil
	}
	// Main fails alone too. It's main's red only on main's record, and only if it's the unit's one failing test.
	if excused(evidence) {
		return Decision{Decided: true, Status: Failed, Cause: CauseMainRed, Why: "main's recorded verdict fails the same test, the unit's only failure"}, nil
	}
	return Decision{Decided: true, Status: Failed, Cause: CauseChange,
		Why: "main fails alone too, but main's record doesn't fail exactly this unit's one failing test, so it's the change's"}, nil
}

// excused says whether a failure main shares is main's red: main's latest recorded verdict fails a test, and that
// same package and test is the only one the unit failed, across its first attempt and its candidate rerun.
func excused(evidence Evidence) bool {
	unitFailures := map[string]bool{}
	for _, outcome := range append(failing(evidence.FirstTests), failing(evidence.Candidate.Tests)...) {
		unitFailures[outcome.Package+" "+outcome.Test] = true
	}
	if len(unitFailures) != 1 {
		return false
	}
	for _, outcome := range failing(evidence.MainRecorded) {
		if unitFailures[outcome.Package+" "+outcome.Test] {
			return true
		}
	}
	return false
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
