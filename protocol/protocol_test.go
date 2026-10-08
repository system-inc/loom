package protocol

import (
	"os"
	"reflect"
	"strings"
	"testing"
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
		"no name":          func(j *Job) { j.Name = "" },
		"no units":         func(j *Job) { j.Units = nil },
		"bad id":           func(j *Job) { j.Units[0].Id = "A b" },
		"duplicate id":     func(j *Job) { j.Units = append(j.Units, j.Units[0]) },
		"no argv":          func(j *Job) { j.Units[0].Argv = nil },
		"no timeout":       func(j *Job) { j.Units[0].TimeoutSeconds = 0 },
		"unknown need":     func(j *Job) { j.Units[0].Needs = []string{"z"} },
		"self need":        func(j *Job) { j.Units[0].Needs = []string{"a"} },
		"bad hash":         func(j *Job) { j.Units[0].Inputs = []Input{{Path: "x", Sha256: "abc"}} },
		"bad archive":      func(j *Job) { j.Units[0].Inputs = []Input{{Path: "x", Sha256: strings.Repeat("a", 64), Archive: "zip"}} },
		"undefined matrix": func(j *Job) { j.Units[0].Argv = []string{"${matrix.k}"} },
		"cycle": func(j *Job) {
			j.Units[0].Needs = []string{"b"}
			j.Units = append(j.Units, JobUnit{Id: "b", Needs: []string{"a"}, Argv: []string{"true"}, TimeoutSeconds: 1})
		},
	}
	if _, err := Expand(good()); err != nil {
		t.Fatalf("the good job is refused: %v", err)
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
