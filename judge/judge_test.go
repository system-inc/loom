package judge

import (
	"strings"
	"testing"
)

const testPackage = "github.com/system-inc/adamic/internal/native"

func outcome(test, result string) TestOutcome {
	return TestOutcome{Package: testPackage, Test: test, Outcome: result}
}

func failedFirst(tests ...TestOutcome) Evidence {
	return Evidence{First: Attempt{Status: Failed, Exit: 1}, FirstTests: tests}
}

func rerun(status string, tests ...TestOutcome) *Rerun {
	return &Rerun{Status: status, Tests: tests}
}

func TestTheRuleTable(t *testing.T) {
	ascii := outcome("TestDecodeASCIIUnit43", "fail")
	other := outcome("TestOther", "fail")
	cases := []struct {
		name     string
		evidence Evidence
		decided  bool
		status   string
		cause    string
		infra    string
		next     string
	}{
		{"a first pass is passed", Evidence{First: Attempt{Status: Passed}}, true, Passed, "", "", ""},
		{"a kill is infra, retried, never the change's", Evidence{First: Attempt{Status: Broken}, FirstInfra: InfraKill}, false, Void, CauseInfra, InfraKill, "retry"},
		{"a failure waits for both alone reruns", failedFirst(ascii), false, "", "", "", "rerunAlone"},
		{"a failure with only the candidate rerun still waits", func() Evidence { e := failedFirst(ascii); e.Candidate = rerun(Failed, ascii); return e }(), false, "", "", "", "rerunAlone"},
		{"a broken rerun is infra and reruns", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main = &Rerun{Status: Broken, Infra: InfraNeverPlaced}, rerun(Passed)
			return e
		}(), false, Void, CauseInfra, InfraNeverPlaced, "retry"},
		{"fails alone on the candidate, passes on main: the change's", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main = rerun(Failed, ascii), rerun(Passed)
			return e
		}(), true, Failed, CauseChange, "", ""},
		{"passes alone on both: a flake, passed", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main = rerun(Passed), rerun(Passed)
			return e
		}(), true, Passed, CauseFlake, "", ""},
		{"fails on both, main's record fails the same one test: main's red", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main, e.MainRecorded = rerun(Failed, ascii), rerun(Failed, ascii), []TestOutcome{ascii}
			return e
		}(), true, Failed, CauseMainRed, "", ""},
		{"fails on both but main has no record: the change's", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main = rerun(Failed, ascii), rerun(Failed, ascii)
			return e
		}(), true, Failed, CauseChange, "", ""},
		{"fails on both, main's record fails a different test: the change's", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main, e.MainRecorded = rerun(Failed, ascii), rerun(Failed, ascii), []TestOutcome{other}
			return e
		}(), true, Failed, CauseChange, "", ""},
		{"main's red plus a second failure in the unit: the change's", func() Evidence {
			e := failedFirst(ascii, other)
			e.Candidate, e.Main, e.MainRecorded = rerun(Failed, ascii, other), rerun(Failed, ascii), []TestOutcome{ascii}
			return e
		}(), true, Failed, CauseChange, "", ""},
		{"passes alone on the candidate, fails on main, on main's record: main's red", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main, e.MainRecorded = rerun(Passed), rerun(Failed, ascii), []TestOutcome{ascii}
			return e
		}(), true, Failed, CauseMainRed, "", ""},
		{"passes alone on the candidate, fails on main, no record: the change's", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main = rerun(Passed), rerun(Failed, ascii)
			return e
		}(), true, Failed, CauseChange, "", ""},
		{"a witness's one tree fails alone, then passes alone: a flake, never the change's", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main, e.SameTree = rerun(Failed, ascii), rerun(Passed), true
			return e
		}(), true, Passed, CauseFlake, "", ""},
		{"a witness's one tree passes alone, then fails alone: a flake, never main's or the change's", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main, e.SameTree = rerun(Passed), rerun(Failed, ascii), true
			return e
		}(), true, Passed, CauseFlake, "", ""},
		{"a witness's one tree fails alone both times: red", func() Evidence {
			e := failedFirst(ascii)
			e.Candidate, e.Main, e.SameTree = rerun(Failed, ascii), rerun(Failed, ascii), true
			return e
		}(), true, Failed, CauseChange, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decision, err := Decide(c.evidence)
			if err != nil {
				t.Fatal(err)
			}
			got := [5]string{map[bool]string{true: "decided", false: "open"}[decision.Decided], decision.Status, decision.Cause, decision.Infra, decision.Next}
			want := [5]string{map[bool]string{true: "decided", false: "open"}[c.decided], c.status, c.cause, c.infra, c.next}
			if got != want {
				t.Fatalf("got %v, want %v (%s)", got, want, decision.Why)
			}
			if decision.Why == "" {
				t.Fatal("every decision names its row")
			}
		})
	}
}

func TestStructureNotTextDecidesInfra(t *testing.T) {
	cases := []struct {
		name     string
		evidence Evidence
		status   string
		cause    string
		next     string
	}{
		{"a pass whose tests all reached pass or skip stays passed",
			Evidence{First: Attempt{Status: Passed}, FirstTests: []TestOutcome{outcome("TestA", "pass"), outcome("TestB", "skip")}}, Passed, "", ""},
		{"a unit run without its toolchain is void, even when it reports a pass",
			Evidence{First: Attempt{Status: Passed}, FirstTests: []TestOutcome{outcome("TestWASIUnit07", "skip")}, MissingTools: []string{"wasiSdk"}}, Void, CauseInfra, "retry"},
		{"a test that never reached a terminal action makes the attempt failed, not infra",
			Evidence{First: Attempt{Status: Passed}, FirstTests: []TestOutcome{outcome("TestPlantedZAfter", "run")}}, "", "", "rerunAlone"},
		{"an attempt reported passed with a failing test is failed",
			Evidence{First: Attempt{Status: Passed}, FirstTests: []TestOutcome{outcome("TestA", "fail")}}, "", "", "rerunAlone"},
		{"a panic that prints 'own work exceeded 90s' is a failure, and only the runner's exit makes a kill",
			Evidence{First: Attempt{Status: Failed, Exit: 2}, FirstTests: []TestOutcome{outcome("TestPlantedWatchdog", "fail")}}, "", "", "rerunAlone"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decision, err := Decide(c.evidence)
			if err != nil {
				t.Fatal(err)
			}
			if decision.Status != c.status || decision.Cause != c.cause || decision.Next != c.next {
				t.Fatalf("got %+v", decision)
			}
		})
	}
}

func TestAFlakeNamesTheTestsToQuarantine(t *testing.T) {
	e := failedFirst(outcome("TestA", "fail"), outcome("TestB", "pass"))
	e.Candidate, e.Main = rerun(Passed), rerun(Passed)
	decision, err := Decide(e)
	if err != nil {
		t.Fatal(err)
	}
	if len(decision.Flaky) != 1 || decision.Flaky[0].Test != "TestA" {
		t.Fatalf("flaky %v, want TestA alone", decision.Flaky)
	}
}

func TestUnknownStatusesAndInfraKindsAreRefused(t *testing.T) {
	for _, evidence := range []Evidence{
		{First: Attempt{Status: "green"}},
		{First: Attempt{Status: Broken}, FirstInfra: "load"},
		{First: Attempt{Status: Failed}, Candidate: &Rerun{Status: "maybe"}, Main: rerun(Passed)},
		{First: Attempt{Status: Failed}, Candidate: &Rerun{Status: Broken}, Main: rerun(Passed)},
	} {
		if _, err := Decide(evidence); err == nil {
			t.Fatalf("decided %+v, want refused", evidence)
		}
	}
}

func TestTheRecordIsCanonicalWithContractFieldNames(t *testing.T) {
	encoded, err := Verdict{UnitKey: strings.Repeat("a", 64), Change: "chg_X", Future: strings.Repeat("b", 40), Run: "r1",
		Status: Passed, RuleId: Rule, DecidedAt: "2026-10-09T23:30:00Z"}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"attempts":[],"cause":null,"change":"chg_X","decidedAt":"2026-10-09T23:30:00Z","future":"` + strings.Repeat("b", 40) +
		`","infra":null,"outputs":[],"rule":"judge-v1","run":"r1","status":"passed","tests":{"failed":0,"inline":[],"passed":0,` +
		`"sha256":"4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945","skipped":0},"unitKey":"` + strings.Repeat("a", 64) + `"}`
	if string(encoded) != want {
		t.Fatalf("got\n%s\nwant\n%s", encoded, want)
	}
	attempt, err := Verdict{Attempts: []Attempt{{Machine: "m", Runner: "r", StartedAt: "s", FinishedAt: "f", Exit: 2, WallSeconds: 1.5, Status: Broken}},
		Tests: []TestOutcome{outcome("TestA", "fail")}, Cause: CauseInfra, Infra: InfraKill}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"exit":2`, `"finishedAt":"f"`, `"machine":"m"`, `"runner":"r"`, `"startedAt":"s"`, `"status":"broken"`, `"wallSeconds":1.5`,
		`"failed":1,"inline":[{"outcome":"fail","package":"` + testPackage + `","test":"TestA"}],"passed":0`, `"cause":"infra"`, `"infra":"kill"`} {
		if !strings.Contains(string(attempt), field) {
			t.Fatalf("%s lacks %s", attempt, field)
		}
	}
}

func verdict(key, status, cause string) Verdict {
	return Verdict{UnitKey: key, Status: status, Cause: cause}
}

func TestARunIsGreenOnlyWhenEveryPlannedUnitPassedOnce(t *testing.T) {
	plan := []string{"a", "b", "c"}
	cases := []struct {
		name     string
		verdicts []Verdict
		status   string
	}{
		{"every unit passed", []Verdict{verdict("a", Passed, ""), verdict("b", Passed, CauseFlake), verdict("c", Passed, "")}, "green"},
		{"main's red is excused", []Verdict{verdict("a", Passed, ""), verdict("b", Failed, CauseMainRed), verdict("c", Passed, "")}, "green"},
		{"the change's failure is red", []Verdict{verdict("a", Passed, ""), verdict("b", Failed, CauseChange), verdict("c", Passed, "")}, "red"},
		{"a missing unit is void", []Verdict{verdict("a", Passed, ""), verdict("b", Passed, "")}, "void"},
		{"an extra unit is void", []Verdict{verdict("a", Passed, ""), verdict("b", Passed, ""), verdict("c", Passed, ""), verdict("d", Passed, "")}, "void"},
		{"a repeated unit is void", []Verdict{verdict("a", Passed, ""), verdict("b", Passed, ""), verdict("b", Passed, "")}, "void"},
		{"a void unit is void", []Verdict{verdict("a", Passed, ""), verdict("b", Void, CauseInfra), verdict("c", Passed, "")}, "void"},
		{"a failure with no rule's cause is void", []Verdict{verdict("a", Passed, ""), verdict("b", Failed, CauseFlake), verdict("c", Passed, "")}, "void"},
		{"the change's red outranks a missing unit", []Verdict{verdict("a", Failed, CauseChange), verdict("b", Passed, "")}, "red"},
		{"an extra unit's red isn't the change's", []Verdict{verdict("a", Passed, ""), verdict("b", Passed, ""), verdict("c", Passed, ""), verdict("d", Failed, CauseChange)}, "void"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Green(plan, c.verdicts); got.Status != c.status {
				t.Fatalf("status %s, want %s (%+v)", got.Status, c.status, got)
			}
		})
	}
}

// The build law (Kirk, Oct 10 21:5xZ, #ccewvra): a test unit that passes past the 60 s budget is green with a warning,
// and red only when its alone reruns show the branch made it slow, past the budget and past 1.5x main's base.
func TestASlowPassIsGreenWithAWarningUnlessTheBranchMadeItSlow(t *testing.T) {
	slow := func(wall float64) Evidence {
		return Evidence{First: Attempt{Status: Passed, WallSeconds: wall}, Budgeted: true}
	}
	alone := func(status string, wall float64) *Rerun { return &Rerun{Status: status, WallSeconds: wall} }
	cases := []struct {
		name     string
		evidence Evidence
		decided  bool
		status   string
		cause    string
		next     string
		warned   bool
		baseWall float64
	}{
		{"a pass within the budget has no warning", slow(59.9), true, Passed, "", "", false, 0},
		{"exactly the budget is within it", slow(60), true, Passed, "", "", false, 0},
		{"a phase isn't held to the budget", func() Evidence { e := slow(300); e.Budgeted = false; return e }(), true, Passed, "", "", false, 0},
		{"a slow pass on a branch reruns alone on both before it decides", slow(74), false, "", "", "rerunAlone", false, 0},
		{"a slow pass on a verify is green with a warning, nothing rerun", func() Evidence { e := slow(74); e.SameTree = true; return e }(), true, Passed, "", "", true, 0},
		{"slow alone on the candidate, fast on the base: the branch's red", func() Evidence {
			e := slow(74)
			e.Candidate, e.Main = alone(Passed, 80), alone(Passed, 30)
			return e
		}(), true, Failed, CauseChange, "", true, 30},
		{"slow on the base too: green with a warning", func() Evidence {
			e := slow(74)
			e.Candidate, e.Main = alone(Passed, 80), alone(Passed, 70)
			return e
		}(), true, Passed, "", "", true, 70},
		{"far past a base that was itself over the budget: green with a warning, the branch didn't push it over", func() Evidence {
			e := slow(74)
			e.Candidate, e.Main = alone(Passed, 200), alone(Passed, 70)
			return e
		}(), true, Passed, "", "", true, 70},
		{"over the budget but within 1.5x a base inside it: noise, green with a warning", func() Evidence {
			e := slow(74)
			e.Candidate, e.Main = alone(Passed, 80), alone(Passed, 60)
			return e
		}(), true, Passed, "", "", true, 60},
		{"within the budget alone on the candidate: green with a warning", func() Evidence {
			e := slow(74)
			e.Candidate, e.Main = alone(Passed, 50), alone(Passed, 20)
			return e
		}(), true, Passed, "", "", true, 20},
		{"a candidate rerun that failed can't be compared: green with a warning, never void", func() Evidence {
			e := slow(74)
			e.Candidate, e.Main = alone(Failed, 90), alone(Passed, 20)
			return e
		}(), true, Passed, "", "", true, 0},
		{"a base rerun that broke can't be compared: green with a warning", func() Evidence {
			e := slow(74)
			e.Candidate, e.Main = alone(Passed, 90), &Rerun{Status: Broken, Infra: InfraNeverPlaced}
			return e
		}(), true, Passed, "", "", true, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			decision, err := Decide(c.evidence)
			if err != nil {
				t.Fatal(err)
			}
			got := [4]string{map[bool]string{true: "decided", false: "open"}[decision.Decided], decision.Status, decision.Cause, decision.Next}
			want := [4]string{map[bool]string{true: "decided", false: "open"}[c.decided], c.status, c.cause, c.next}
			if got != want {
				t.Fatalf("got %v, want %v (%s)", got, want, decision.Why)
			}
			if warned := len(decision.Warnings) == 1 && decision.Warnings[0].Kind == WarningOverBudget && decision.Warnings[0].WallSeconds == c.evidence.First.WallSeconds; warned != c.warned {
				t.Fatalf("warnings %+v, want warned %v", decision.Warnings, c.warned)
			}
			if c.warned && decision.Warnings[0].BaseWallSeconds != c.baseWall {
				t.Fatalf("base wall %v, want %v", decision.Warnings[0].BaseWallSeconds, c.baseWall)
			}
			if decision.Why == "" {
				t.Fatal("every decision names its row")
			}
		})
	}
}
