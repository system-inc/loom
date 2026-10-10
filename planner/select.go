package planner

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// A Verdict is the part of contract section 3's verdict record selection reads.
type Verdict struct {
	UnitKey string `json:"unitKey"`
	Status  string `json:"status"` // passed | failed | void
	Run     string `json:"run"`
}

// A VerdictIndex answers the latest decided verdict for a unit key: Queue serves it as GET /verdicts/<unitKey>
// (contract v1.1), from the verdict.decided events in its log.
type VerdictIndex interface {
	Latest(unitKey string) (Verdict, bool, error)
}

// MemoryIndex is the stub for Queue's verdict index until loom serves it: a map from unit key to its latest
// verdict, which the caller fills.
type MemoryIndex map[string]Verdict

func (index MemoryIndex) Latest(unitKey string) (Verdict, bool, error) {
	verdict, found := index[unitKey]
	return verdict, found, nil
}

// A PlannedUnit is one unit of a candidate's plan with its key.
type PlannedUnit struct {
	Name    string `json:"name"`
	UnitKey string `json:"unitKey"`
}

// A Choice is what the plan does with a unit, and why.
type Choice struct {
	PlannedUnit
	Action string `json:"action"` // reuse | run
	Reason string `json:"reason"`
	Reused string `json:"reused,omitempty"` // the run whose passed verdict is reused
}

// Choose decides each unit of a plan: a unit reuses a verdict only when its exact key has a decided passed verdict,
// and never in uncached mode (the witness and main's landing). Everything else runs. The plan holds every unit once.
func Choose(units []PlannedUnit, index VerdictIndex, uncached bool) ([]Choice, error) {
	seen := map[string]bool{}
	choices := make([]Choice, 0, len(units))
	for _, unit := range units {
		if !Sha256Hex(unit.UnitKey) {
			return nil, fmt.Errorf("unit %s has no unit key", unit.Name)
		}
		if seen[unit.Name] {
			return nil, fmt.Errorf("unit %s is planned twice", unit.Name)
		}
		seen[unit.Name] = true
		choice := Choice{PlannedUnit: unit, Action: "run"}
		switch {
		case uncached:
			choice.Reason = "uncached mode: no verdict is reused"
		default:
			verdict, found, err := index.Latest(unit.UnitKey)
			if err != nil {
				return nil, fmt.Errorf("verdict index for %s: %w", unit.Name, err)
			}
			switch {
			case !found:
				choice.Reason = "new key: no decided verdict"
			case verdict.UnitKey != unit.UnitKey:
				choice.Reason = "the index answered another key"
			case verdict.Status != "passed":
				choice.Reason = "latest verdict for this key is " + verdict.Status
			default:
				choice.Action, choice.Reason, choice.Reused = "reuse", "this exact key passed", verdict.Run
			}
		}
		choices = append(choices, choice)
	}
	sort.Slice(choices, func(left, right int) bool { return choices[left].Name < choices[right].Name })
	return choices, nil
}

// Sha256Hex is the contract's hash shape: 64 lowercase hex digits.
func Sha256Hex(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

// NoReuseFile lists unit keys whose passed verdicts may not be reused, one per line as "<unitKey> <why>", with #
// comments: a pass that ran on a warm shared cache is evidence, never a verdict to reuse (Release, Oct 10 02:43Z). A
// missing file lists none. ~/.loom/no-reuse unless a command names another (--no-reuse).
var NoReuseFile = func() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".loom", "no-reuse")
}()

// LoadNoReuse reads a no-reuse file into each key's why.
func LoadNoReuse(path string) (map[string]string, error) {
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("the no-reuse file: %w", err)
	}
	noReuse := map[string]string{}
	for number, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, why, _ := strings.Cut(line, " ")
		why = strings.TrimSpace(why)
		if !Sha256Hex(key) || why == "" {
			return nil, fmt.Errorf("%s:%d: want <unitKey> <why>, got %q", path, number+1, line)
		}
		noReuse[key] = why
	}
	return noReuse, nil
}

// Unreused turns every reuse of a listed key back into a run, naming why, so a stale pass can't reach a plan.
func Unreused(choices []Choice, noReuse map[string]string) []Choice {
	for index, choice := range choices {
		if why, listed := noReuse[choice.UnitKey]; listed && choice.Action == "reuse" {
			choices[index].Action, choices[index].Reused = "run", ""
			choices[index].Reason = "its passed verdict isn't reusable: " + why
		}
	}
	return choices
}
