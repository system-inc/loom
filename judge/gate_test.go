package judge

import (
	"strings"
	"testing"
)

// The suite as Adamic's cloud/gate-mutants.tsv held it at c0232217 (comments trimmed).
const suiteText = "# the gate-mutant suite\n" +
	"plain-wrong-answer-leaf\t2df63b7390d02b3b70b9751369aa17069a30b640\ttests\tstage3/census/latent/statecopy Test[A-Za-z]+\n" +
	"binary-dies-partway\ta840ec0723dee3a596055f76146a36a44cc60b10\ttests\tinternal/corpusfiles\n" +
	"deferred-red-reaches-census\t0096078f544f90bb5d48febbc5099a906699e734\tcensus\n" +
	"wasi-family-must-run\t15e7f9faa8733da808570aa6b63777161d1402c0\ttests\tinternal/native TestWASIUnit[0-9]+\n" +
	"canary-empty-landings\tfae5b2c14f1f05b31e1dc9bda30f4d32f58222ca\tthin\t\n"

func suite(t *testing.T) []Mutant {
	mutants, err := ParseMutants(suiteText)
	if err != nil {
		t.Fatal(err)
	}
	return mutants
}

// Every mutant read as declared, and a healthy canary.
func correctReadings(mutants []Mutant) (map[string]Reading, Reading) {
	readings := map[string]Reading{}
	for _, mutant := range mutants {
		switch mutant.Name {
		case "plain-wrong-answer-leaf":
			readings[mutant.Name] = Reading{Sha: mutant.Sha, Status: "red", FirstStep: "tests", FirstFailure: "stage3/census/latent/statecopy TestPublishedCopyModeIsPrivate"}
		case "binary-dies-partway":
			readings[mutant.Name] = Reading{Sha: mutant.Sha, Status: "red", FirstStep: "tests", FirstFailure: "github.com/system-inc/adamic/internal/corpusfiles started 12, verdicts 11"}
		case "deferred-red-reaches-census":
			readings[mutant.Name] = Reading{Sha: mutant.Sha, Status: "red", FirstStep: "census"}
		case "wasi-family-must-run":
			readings[mutant.Name] = Reading{Sha: mutant.Sha, Status: "red", FirstStep: "tests", FirstFailure: "github.com/system-inc/adamic/internal/native TestWASIUnit07"}
		case "canary-empty-landings":
			readings[mutant.Name] = Reading{Sha: mutant.Sha, Status: "green", PassedTests: 0}
		}
	}
	return readings, Reading{Status: "green", PassedTests: 4340}
}

func TestTheSuiteParsesAsTheWatcherReadsIt(t *testing.T) {
	mutants := suite(t)
	if len(mutants) != 5 || mutants[2].Pattern != nil || mutants[4].Step != StepThin || mutants[0].Pattern == nil {
		t.Fatalf("parsed %+v", mutants)
	}
	for _, bad := range []string{"", "# only comments\n", "a\tnotasha\ttests\n", "a\t" + strings.Repeat("a", 40) + "\n",
		"a\t" + strings.Repeat("a", 40) + "\ttests\t(\n", "a\t" + strings.Repeat("a", 40) + "\ttests\na\t" + strings.Repeat("b", 40) + "\ttests\n"} {
		if _, err := ParseMutants(bad); err == nil {
			t.Fatalf("parsed %q, want refused", bad)
		}
	}
}

func TestNoSuiteAndNoReadingsHoldsEvenBesideAGreenCanary(t *testing.T) {
	if got := GateTheGate(nil, map[string]Reading{}, Reading{Status: "green", PassedTests: 4340}); got.Promote {
		t.Fatal("promoted with no mutant suite")
	}
}

func TestEveryMutantAsDeclaredAndAGreenCanaryPromote(t *testing.T) {
	mutants := suite(t)
	readings, canary := correctReadings(mutants)
	if got := GateTheGate(mutants, readings, canary); !got.Promote {
		t.Fatalf("held: %v", got.Hold)
	}
}

func TestEachWrongReadingHolds(t *testing.T) {
	mutants := suite(t)
	cases := []struct {
		name    string
		mutant  string
		reading func(Reading) Reading
		wrong   bool
	}{
		{"a mutant read green slipped through", "binary-dies-partway", func(r Reading) Reading { r.Status, r.FirstStep, r.PassedTests = "green", "", 900; return r }, true},
		{"a red at the wrong step", "deferred-red-reaches-census", func(r Reading) Reading { r.FirstStep = "tests"; return r }, true},
		{"a red whose first failure is another test", "wasi-family-must-run", func(r Reading) Reading { r.FirstFailure = "internal/native TestSomethingElse"; return r }, true},
		{"a thin canary read green with enough tests", "canary-empty-landings", func(r Reading) Reading { r.PassedTests = 2170; return r }, true},
		{"a reading of another sha", "plain-wrong-answer-leaf", func(r Reading) Reading { r.Sha = strings.Repeat("c", 40); return r }, true},
		{"a void is gated again, not wrong", "binary-dies-partway", func(r Reading) Reading { r.Status, r.VoidCause = "void", "over the ceiling"; return r }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			readings, canary := correctReadings(mutants)
			readings[c.mutant] = c.reading(readings[c.mutant])
			got := GateTheGate(mutants, readings, canary)
			if got.Promote {
				t.Fatal("promoted")
			}
			if wrong := len(got.Wrong) == 1 && got.Wrong[0] == c.mutant; wrong != c.wrong {
				t.Fatalf("wrong %v, want named %v (%v)", got.Wrong, c.wrong, got.Hold)
			}
		})
	}
}

func TestTheCanaryAndTheSuiteMustBeWhole(t *testing.T) {
	mutants := suite(t)
	cases := []struct {
		name   string
		change func(map[string]Reading, *Reading) []Mutant
	}{
		{"a red canary", func(_ map[string]Reading, canary *Reading) []Mutant { canary.Status = "red"; return mutants }},
		{"a thin canary", func(_ map[string]Reading, canary *Reading) []Mutant { canary.PassedTests = 499; return mutants }},
		{"a mutant with no reading", func(readings map[string]Reading, _ *Reading) []Mutant {
			delete(readings, "wasi-family-must-run")
			return mutants
		}},
		{"a reading the suite doesn't declare", func(readings map[string]Reading, _ *Reading) []Mutant {
			readings["stray"] = Reading{Status: "red"}
			return mutants
		}},
		{"no suite at all", func(_ map[string]Reading, _ *Reading) []Mutant { return nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			readings, canary := correctReadings(mutants)
			used := c.change(readings, &canary)
			if got := GateTheGate(used, readings, canary); got.Promote {
				t.Fatal("promoted")
			}
		})
	}
}
