package judge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var (
	futureTree = strings.Repeat("f", 40)
	baseTree   = strings.Repeat("b", 40)
)

type stubRuns map[string]Finished

func (runs stubRuns) Finished(run, unitKey string) (Finished, bool, error) {
	finished, found := runs[unitKey]
	return finished, found, nil
}

type stubMain map[string][]TestOutcome

func (records stubMain) Latest(base, unitKey string) ([]TestOutcome, bool, error) {
	tests, found := records[unitKey]
	return tests, found, nil
}

func passed() Finished {
	return Finished{Attempt: Attempt{Status: Passed}, Tests: []TestOutcome{outcome("TestA", "pass")}}
}

func failedWith(test string) Finished {
	return Finished{Attempt: Attempt{Status: Failed, Exit: 1}, Tests: []TestOutcome{outcome(test, "fail")}}
}

func broken(kind string) Finished {
	return Finished{Attempt: Attempt{Status: Broken, Exit: -1}, Infra: kind}
}

type harness struct {
	runs   stubRuns
	fabric *StubFabric
	main   stubMain
	queue  *StubQueue
}

func newHarness() harness {
	return harness{runs: stubRuns{}, fabric: &StubFabric{Script: map[string][]Finished{}}, main: stubMain{}, queue: &StubQueue{}}
}

func (h harness) script(unit, tree string, answers ...Finished) {
	h.fabric.Script[unit+" "+tree] = append(h.fabric.Script[unit+" "+tree], answers...)
}

func (h harness) judge(t *testing.T, plan ...PlanUnit) FuturePost {
	loop := Loop{Runs: h.runs, Fabric: h.fabric, Main: h.main, Queue: h.queue, Now: func() time.Time { return time.Date(2026, 10, 9, 23, 45, 0, 0, time.UTC) }}
	post, err := loop.JudgeFuture(Job{Record: ChangeRecord{Change: "chg_A", Sha: futureTree, Base: baseTree, Owner: "system_adamic_library"}, Change: "chg_A", Future: futureTree, Base: baseTree, Run: "run-1", Plan: plan})
	if err != nil {
		t.Fatal(err)
	}
	if posted := h.queue.Posts[futureTree]; len(posted) != 1 {
		t.Fatalf("queue got %d posts for the future, want 1", len(posted))
	}
	return post
}

func recordOf(t *testing.T, post FuturePost, unit string) Verdict {
	for _, raw := range post.Verdicts {
		var record struct {
			UnitKey string  `json:"unitKey"`
			Status  string  `json:"status"`
			Cause   *string `json:"cause"`
			Infra   *string `json:"infra"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		if record.UnitKey == unit {
			verdict := Verdict{UnitKey: record.UnitKey, Status: record.Status}
			if record.Cause != nil {
				verdict.Cause = *record.Cause
			}
			if record.Infra != nil {
				verdict.Infra = *record.Infra
			}
			return verdict
		}
	}
	t.Fatalf("no record for %s", unit)
	return Verdict{}
}

func TestEachPathThroughTheLoop(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(harness)
		plan   PlanUnit
		status string
		cause  string
		infra  string
		asked  int
		run    string
	}{
		{"a pass posts passed, nothing rerun", func(h harness) { h.runs["u"] = passed() }, PlanUnit{UnitKey: "u"}, Passed, "", "", 0, "green"},
		{"a reused unit is passed without reading its run", func(h harness) {}, PlanUnit{UnitKey: "u", Reused: "verdict-7"}, Passed, "", "", 0, "green"},
		{"a kill is placed again and its pass stands", func(h harness) {
			h.runs["u"] = broken(InfraKill)
			h.script("u", futureTree, passed())
		}, PlanUnit{UnitKey: "u"}, Passed, "", "", 1, "green"},
		{"infra past its retries is void", func(h harness) {
			h.runs["u"] = broken(InfraDisk)
			h.script("u", futureTree, broken(InfraDisk), broken(InfraDisk))
		}, PlanUnit{UnitKey: "u"}, Void, CauseInfra, InfraDisk, 2, "void"},
		{"a never-reported unit is placed again", func(h harness) { h.script("u", futureTree, passed()) }, PlanUnit{UnitKey: "u"}, Passed, "", "", 1, "green"},
		{"a failure that fails alone on the candidate only is the change's", func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, failedWith("TestB"))
			h.script("u", baseTree, passed())
		}, PlanUnit{UnitKey: "u"}, Failed, CauseChange, "", 2, "red"},
		{"a failure that passes alone on both is a flake, passed", func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, passed())
			h.script("u", baseTree, passed())
		}, PlanUnit{UnitKey: "u"}, Passed, CauseFlake, "", 2, "green"},
		{"a failure main shares on its record is main's red, excused", func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, failedWith("TestB"))
			h.script("u", baseTree, failedWith("TestB"))
			h.main["u"] = []TestOutcome{outcome("TestB", "fail")}
		}, PlanUnit{UnitKey: "u"}, Failed, CauseMainRed, "", 2, "green"},
		{"a rerun that breaks reruns both again", func(h harness) {
			h.runs["u"] = failedWith("TestB")
			h.script("u", futureTree, broken(InfraNeverPlaced), failedWith("TestB"))
			h.script("u", baseTree, passed(), passed())
		}, PlanUnit{UnitKey: "u"}, Failed, CauseChange, "", 4, "red"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness()
			c.setup(h)
			post := h.judge(t, c.plan)
			record := recordOf(t, post, c.plan.UnitKey)
			if record.Status != c.status || record.Cause != c.cause || record.Infra != c.infra {
				t.Fatalf("record %+v, want %s %s %s", record, c.status, c.cause, c.infra)
			}
			if len(h.fabric.Asked) != c.asked {
				t.Fatalf("fabric asked %v, want %d placements", h.fabric.Asked, c.asked)
			}
			if post.Decision.Status != c.run {
				t.Fatalf("run %s, want %s", post.Decision.Status, c.run)
			}
		})
	}
}

func TestAFlakeIsPostedForQuarantine(t *testing.T) {
	h := newHarness()
	h.runs["u"] = failedWith("TestB")
	h.script("u", futureTree, passed())
	h.script("u", baseTree, passed())
	post := h.judge(t, PlanUnit{UnitKey: "u"})
	if len(post.Quarantine) != 1 || post.Quarantine[0].Test != "TestB" {
		t.Fatalf("quarantine %v, want TestB", post.Quarantine)
	}
}

func TestTheReranRedRunsOnTheCandidateAndOnMain(t *testing.T) {
	h := newHarness()
	h.runs["u"] = failedWith("TestB")
	h.script("u", futureTree, failedWith("TestB"))
	h.script("u", baseTree, passed())
	h.judge(t, PlanUnit{UnitKey: "u"})
	if strings.Join(h.fabric.Asked, ",") != "u "+futureTree+",u "+baseTree {
		t.Fatalf("placements %v, want the candidate's tree then main's base", h.fabric.Asked)
	}
}

func TestThePostCarriesThePlanAndARecordPerUnit(t *testing.T) {
	h := newHarness()
	h.runs["u"], h.runs["v"] = passed(), passed()
	post := h.judge(t, PlanUnit{UnitKey: "u"}, PlanUnit{UnitKey: "v"}, PlanUnit{UnitKey: "w", Reused: "verdict-3"})
	if strings.Join(post.Plan, ",") != "u,v,w" || len(post.Verdicts) != 3 || post.Rule != Rule || post.Change != "chg_A" || post.Run != "run-1" {
		t.Fatalf("post %+v", post)
	}
	for _, raw := range post.Verdicts {
		if !strings.Contains(string(raw), `"future":"`+futureTree+`"`) || !strings.Contains(string(raw), `"change":"chg_A"`) {
			t.Fatalf("record %s lacks the future or the change", raw)
		}
	}
}

func TestARedPostCarriesItsKickAndTheDecisionReadsAsQueueParsesIt(t *testing.T) {
	h := newHarness()
	h.runs["u"], h.runs["v"] = failedWith("TestB"), passed()
	h.script("u", futureTree, failedWith("TestB"))
	h.script("u", baseTree, passed())
	post := h.judge(t, PlanUnit{UnitKey: "u"}, PlanUnit{UnitKey: "v"})
	kick, found := post.Decision.Kicks["u"]
	if !found || len(post.Decision.Kicks) != 1 || kick.Repro != "loom repro u" || kick.Owner != "system_adamic_library" || kick.Tests[0].Test != "TestB" {
		t.Fatalf("kicks %+v", post.Decision.Kicks)
	}
	encoded, err := json.Marshal(post)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"decision":{"status":"red"`, `"red":["u"]`, `"excused":[]`, `"problems":[]`, `"kicks":{"u":{`, `"plan":["u","v"]`, `"rule":"judge-v1"`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("%s lacks %s", encoded, field)
		}
	}
}
