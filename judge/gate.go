package judge

// Gating the gate (#82tz9ty): a tools change rides the queue like code, and its hash becomes current only when every
// planted mutant reads as declared through the new path and a canary of main's tip reads green with enough tests run.
// The suite is Adamic's cloud/gate-mutants.tsv (@system_adamic's ruling, Oct 9 06:21Z), and the readings here are the
// ones the box watcher's judgeMutant and thinCanary already make (cloud/fast-gate-watch.sh), so the new path and the
// old one agree on what a correct gate looks like.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// CanaryMinPass is the fewest passed tests a canary's green needs; under it, the canary proved nothing about the tests
// and reads void (@system_adamic, Oct 9 10:50Z).
const CanaryMinPass = 500

// StepThin is a mutant's declared step when the correct reading is a thin canary: a green under CanaryMinPass passed
// tests, which reads void and promotes nothing (hole 5).
const StepThin = "thin"

// A Mutant is one planted candidate whose verdict a correct gate already knows.
type Mutant struct {
	Name    string
	Sha     string
	Step    string         // the step its red must be at, or StepThin
	Pattern *regexp.Regexp // must match the first failure's block; nil when the suite gives none
}

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ParseMutants reads the suite: name, sha, step and pattern, tab-separated, one per line; # lines are comments. An empty
// suite is refused, since no suite proves nothing and must hold every promotion.
func ParseMutants(text string) ([]Mutant, error) {
	mutants := []Mutant{}
	seen := map[string]bool{}
	for number, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) < 3 || fields[0] == "" || fields[2] == "" {
			return nil, fmt.Errorf("line %d: name, sha and step, tab-separated, then an optional pattern", number+1)
		}
		if !shaPattern.MatchString(fields[1]) {
			return nil, fmt.Errorf("line %d: %q isn't a 40-hex sha", number+1, fields[1])
		}
		if seen[fields[0]] {
			return nil, fmt.Errorf("line %d: mutant %s is named twice", number+1, fields[0])
		}
		seen[fields[0]] = true
		mutant := Mutant{Name: fields[0], Sha: fields[1], Step: fields[2]}
		if len(fields) > 3 && fields[3] != "" {
			pattern, err := regexp.Compile(fields[3])
			if err != nil {
				return nil, fmt.Errorf("line %d: pattern %q: %v", number+1, fields[3], err)
			}
			mutant.Pattern = pattern
		}
		mutants = append(mutants, mutant)
	}
	if len(mutants) == 0 {
		return nil, fmt.Errorf("the suite names no mutant, and an empty suite holds every promotion")
	}
	return mutants, nil
}

// A Reading is what one gate run of a mutant, or a canary, came to.
type Reading struct {
	Sha          string
	Status       string // green, red or void
	VoidCause    string // why, when void
	FirstStep    string // the step of the first failure, when red
	FirstFailure string // the first failure's block, when red
	PassedTests  int
}

// JudgeMutant reads one mutant's run against its declaration: "ok", "void <cause>" (gate it again) or "wrong <what
// it read>" (the tools let a planted fault through, or caught it in the wrong place).
func JudgeMutant(mutant Mutant, reading Reading) string {
	if reading.Status == "void" {
		return "void " + reading.VoidCause
	}
	if reading.Sha != mutant.Sha {
		return fmt.Sprintf("wrong: a reading of %s, not the mutant's %s", reading.Sha, mutant.Sha)
	}
	if mutant.Step == StepThin {
		if reading.Status == "green" && reading.PassedTests < CanaryMinPass {
			return "ok"
		}
		return fmt.Sprintf("wrong %s with %d passed tests (expected a canary green under %d, which reads void)", reading.Status, reading.PassedTests, CanaryMinPass)
	}
	if reading.Status != "red" || reading.FirstStep != mutant.Step {
		return fmt.Sprintf("wrong %s at %q (expected red at %s)", reading.Status, reading.FirstStep, mutant.Step)
	}
	if mutant.Pattern != nil && !mutant.Pattern.MatchString(reading.FirstFailure) {
		return fmt.Sprintf("wrong red at %s whose first failure doesn't match %s", mutant.Step, mutant.Pattern)
	}
	return "ok"
}

// A ToolsVerdict says whether a tools hash may become current.
type ToolsVerdict struct {
	Promote bool
	Hold    []string // one line per reason it can't yet: a void to gate again, or a wrong reading
	Wrong   []string // the mutants the tools read wrongly: page developer tools
}

// GateTheGate decides a tools change: it promotes only when every mutant in the suite has exactly one reading and it
// reads ok, and the canary of main's tip is green with at least CanaryMinPass passed tests. A void anywhere holds it
// (gate that one again); a wrong reading holds it and names the mutant.
func GateTheGate(suite []Mutant, readings map[string]Reading, canary Reading) ToolsVerdict {
	result := ToolsVerdict{Hold: []string{}, Wrong: []string{}}
	if len(suite) == 0 {
		result.Hold = append(result.Hold, "no mutant suite: an empty suite holds every promotion")
	}
	declared := map[string]bool{}
	for _, mutant := range suite {
		declared[mutant.Name] = true
		reading, found := readings[mutant.Name]
		if !found {
			result.Hold = append(result.Hold, "mutant "+mutant.Name+" has no reading")
			continue
		}
		switch said := JudgeMutant(mutant, reading); {
		case said == "ok":
		case strings.HasPrefix(said, "void"):
			result.Hold = append(result.Hold, "mutant "+mutant.Name+": "+said+", gate it again")
		default:
			result.Hold = append(result.Hold, "mutant "+mutant.Name+": "+said)
			result.Wrong = append(result.Wrong, mutant.Name)
		}
	}
	for name := range readings {
		if !declared[name] {
			result.Hold = append(result.Hold, "a reading of "+name+", which the suite doesn't declare")
		}
	}
	switch {
	case canary.Status != "green":
		result.Hold = append(result.Hold, fmt.Sprintf("main's canary read %s, not green", canary.Status))
	case canary.PassedTests < CanaryMinPass:
		result.Hold = append(result.Hold, fmt.Sprintf("main's canary ran %d tests, under the canary's %d: void", canary.PassedTests, CanaryMinPass))
	}
	result.Promote = len(result.Hold) == 0
	return result
}

// ReadingOf reads a future's posted batch as a gate reading, so GateTheGate judges the new path's runs the way it
// judges the box's: the decision's status; the step of a red, "tests" when a red unit failed a test, else "census"
// when a red unit's tests passed and its skips failed the census (RuleCensus), as the box runs its census after its
// tests; the red units' failing tests (or a census red's skips) as the first failure's text; and every passed test
// counted for the thin-canary rule.
func ReadingOf(sha string, post FuturePost) (Reading, error) {
	reading := Reading{Sha: sha, Status: post.Decision.Status}
	red := map[string]bool{}
	for _, key := range post.Decision.Red {
		red[key] = true
	}
	failures, skips := []string{}, []string{}
	for _, raw := range post.Verdicts {
		var record struct {
			UnitKey string        `json:"unitKey"`
			Tests   []TestOutcome `json:"tests"`
			Rule    string        `json:"rule"`
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return Reading{}, err
		}
		for _, outcome := range record.Tests {
			switch {
			case outcome.Outcome == "pass":
				reading.PassedTests++
			case outcome.Outcome == "fail" && red[record.UnitKey]:
				failures = append(failures, outcome.Package+" "+outcome.Test)
			case outcome.Outcome == "skip" && red[record.UnitKey] && record.Rule == RuleCensus:
				skips = append(skips, outcome.Package+" "+outcome.Test)
			}
		}
	}
	switch {
	case reading.Status == "red" && len(failures) == 0 && len(skips) > 0:
		reading.FirstStep, reading.FirstFailure = "census", strings.Join(skips, "\n")
	case reading.Status == "red":
		reading.FirstStep, reading.FirstFailure = "tests", strings.Join(failures, "\n")
	case reading.Status == "void":
		reading.VoidCause = strings.Join(post.Decision.Problems, "; ")
	}
	return reading, nil
}
