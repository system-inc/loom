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
		`","infra":null,"outputs":[],"rule":"judge-v1","run":"r1","status":"passed","tests":[],"unitKey":"` + strings.Repeat("a", 64) + `"}`
	if string(encoded) != want {
		t.Fatalf("got\n%s\nwant\n%s", encoded, want)
	}
	attempt, err := Verdict{Attempts: []Attempt{{Machine: "m", Runner: "r", StartedAt: "s", FinishedAt: "f", Exit: 2, WallSeconds: 1.5, Status: Broken}},
		Tests: []TestOutcome{outcome("TestA", "fail")}, Cause: CauseInfra, Infra: InfraKill}.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"exit":2`, `"finishedAt":"f"`, `"machine":"m"`, `"runner":"r"`, `"startedAt":"s"`, `"status":"broken"`, `"wallSeconds":1.5`,
		`"outcome":"fail"`, `"package":"` + testPackage + `"`, `"test":"TestA"`, `"cause":"infra"`, `"infra":"kill"`} {
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
