package judge

// Blame from the futures (#r3ngzew). A block of changes c1..ck runs as prefix futures: F1 is main plus c1, F2 is main
// plus c1 and c2, and so on (contract v1, the queue's speculation). Those runs already say who broke what, so a red
// needs no fresh bisect in the common case: a unit whose verdict is the change's red first at Fi, and wasn't at F(i-1),
// was broken by ci. What the futures can't settle is left unblamed and named, never guessed.

import (
	"fmt"
	"sort"
	"strings"
)

// A Future is one prefix future of a block: the changes in it, in order (Changes[len-1] is the one this future adds),
// the tested tree's sha, and its units' verdicts.
type Future struct {
	Changes  []string
	Tree     string
	Verdicts []Verdict
}

// A Blame names the change that broke a unit, and the future that showed it.
type Blame struct {
	Change  string
	UnitKey string
	Future  int    // the index into the block's futures where the unit first turned red
	Why     string // how the futures settled it
}

// Unblamed is a red the futures can't attribute, and why.
type Unblamed struct {
	UnitKey string
	Future  int
	Why     string
}

// BlameFutures attributes every change-caused red in a block. For a unit red with cause change first at future i:
//   - at future i-1 (or main, for i = 0) it passed, or it wasn't planned there: blame change i. "Not planned" rests on
//     selection being sound: a unit outside F(i-1)'s plan reads nothing the earlier changes touched.
//   - at future i-1 it was void: unblamed, since the earlier future never said.
//
// A unit red at i-1 too was blamed at its first red, and isn't blamed again. A main's red (cause mainRed) is nobody in
// the block's. The futures must be prefixes of one another, in order, or the block is refused.
func BlameFutures(futures []Future) ([]Blame, []Unblamed, error) {
	for index, future := range futures {
		if len(future.Changes) != index+1 {
			return nil, nil, fmt.Errorf("future %d holds %d changes; prefix future %d holds %d", index, len(future.Changes), index, index+1)
		}
		if index > 0 {
			for position, change := range futures[index-1].Changes {
				if future.Changes[position] != change {
					return nil, nil, fmt.Errorf("future %d isn't future %d plus one change", index, index-1)
				}
			}
		}
	}
	blames, unblamed := []Blame{}, []Unblamed{}
	settled := map[string]bool{}
	for index, future := range futures {
		keys := []string{}
		byKey := map[string]Verdict{}
		for _, verdict := range future.Verdicts {
			byKey[verdict.UnitKey] = verdict
			keys = append(keys, verdict.UnitKey)
		}
		sort.Strings(keys)
		for _, key := range keys {
			verdict := byKey[key]
			if settled[key] || verdict.Status != Failed || verdict.Cause != CauseChange {
				continue
			}
			settled[key] = true
			change := future.Changes[index]
			if index == 0 {
				blames = append(blames, Blame{Change: change, UnitKey: key, Future: index, Why: "red in the block's first future, over main"})
				continue
			}
			before, planned := verdictOf(futures[index-1], key)
			switch {
			case !planned:
				blames = append(blames, Blame{Change: change, UnitKey: key, Future: index, Why: "red here, and not planned in the future before: the earlier changes don't reach it"})
			case before.Status == Passed:
				blames = append(blames, Blame{Change: change, UnitKey: key, Future: index, Why: "passed in the future before, red here"})
			case before.Status == Void:
				unblamed = append(unblamed, Unblamed{UnitKey: key, Future: index, Why: "void in the future before, so the futures can't say which change broke it"})
			default:
				unblamed = append(unblamed, Unblamed{UnitKey: key, Future: index, Why: fmt.Sprintf("%s (%s) in the future before, which no blame rule reads", before.Status, before.Cause)})
			}
		}
	}
	return blames, unblamed, nil
}

func verdictOf(future Future, key string) (Verdict, bool) {
	for _, verdict := range future.Verdicts {
		if verdict.UnitKey == key {
			return verdict, true
		}
	}
	return Verdict{}, false
}

// A ChangeRecord is what Judge reads of contract v1 §1's change record.
type ChangeRecord struct {
	Change string
	Sha    string
	Base   string
	Owner  string
}

// A Kick is the data of a change.kicked event, and what the owner's change.red carries (contract v1 §5): the failing
// tests, the output's hashes, the diff to read, and the command that reproduces it.
type Kick struct {
	Change  string        `json:"change"`
	Owner   string        `json:"owner"`
	UnitKey string        `json:"unitKey"`
	Future  string        `json:"future"`
	Tests   []TestOutcome `json:"tests"`
	Outputs []string      `json:"outputs"`
	Diff    string        `json:"diff"`
	Repro   string        `json:"repro"`
	Why     string        `json:"why"`
}

// KickFor builds the kick for a blame, from the blamed change's record and the red unit's verdict in the future that
// showed it. Its tests are the failing ones only, and its diff is the change's own, base..sha.
func KickFor(blame Blame, record ChangeRecord, verdict Verdict) (Kick, error) {
	if record.Change != blame.Change {
		return Kick{}, fmt.Errorf("the record is %s's, the blame %s's", record.Change, blame.Change)
	}
	if verdict.UnitKey != blame.UnitKey || verdict.Status != Failed || verdict.Cause != CauseChange {
		return Kick{}, fmt.Errorf("the verdict isn't the blamed unit's change red")
	}
	outputs := verdict.Outputs
	if outputs == nil {
		outputs = []string{}
	}
	tests, why := failing(verdict.Tests), blame.Why
	if verdict.RuleId == RuleCensus {
		// A census red's tests passed: the kick names the skips the census refused, each as its skip outcome.
		tests = []TestOutcome{}
		for _, refused := range verdict.censusFailing {
			fields := strings.Fields(refused)
			if len(fields) == 3 {
				tests = append(tests, TestOutcome{Package: fields[1], Test: fields[2], Outcome: "skip"})
			}
		}
		why += "; census: " + strings.Join(verdict.censusFailing, ", ") + " (classify each skip in the census, or make it run)"
	}
	return Kick{Change: record.Change, Owner: record.Owner, UnitKey: blame.UnitKey, Future: verdict.Future,
		Tests: tests, Outputs: outputs, Diff: record.Base + ".." + record.Sha,
		Repro: "loom repro " + blame.UnitKey, Why: why}, nil
}
