package judge

// The verdict loop (slice 3's seam, Loom 23:29Z, on #82tz9ty): for each planned unit of a future, read its run's
// finished events, apply Decide, rerun a failure alone on the candidate and on main through Fabric before its cause is
// set, retry infra, then post one contract-3 record per unit to Queue with the future's decision from Green. Every
// collaborator is an interface, so a named stub stands in until the real part lands (StubFabric, StubQueue below).

import (
	"encoding/json"
	"fmt"
	"time"
)

// InfraRetries is how many times infra is placed again before a unit's verdict is void.
const InfraRetries = 2

// Finished is one attempt's finished events for a unit, as the runner reported them.
type Finished struct {
	Attempt      Attempt
	Infra        string // the runner's infra kind when Attempt.Status is broken
	Tests        []TestOutcome
	MissingTools []string
	Outputs      []string
}

// Runs reads a run's finished events for one unit; found is false when the unit never reported.
type Runs interface {
	Finished(run, unitKey string) (finished Finished, found bool, err error)
}

// Fabric places one unit alone on a tree and waits for it (contract v1: Fabric's placement).
type Fabric interface {
	RerunAlone(unitKey, tree string) (Finished, error)
}

// MainRecords gives main's latest recorded verdict for a unit at a base: its test outcomes, and whether there is one.
type MainRecords interface {
	Latest(base, unitKey string) (tests []TestOutcome, found bool, err error)
}

// Queue takes a decided future's verdicts.
type Queue interface {
	PostVerdicts(future string, post FuturePost) error
}

// A PlanUnit is one planned unit of a future: run, or reused from an earlier passed verdict of the exact same key.
type PlanUnit struct {
	UnitKey string
	Reused  string // the reused verdict's id; empty when the unit runs
}

// A Job is one future to judge. Its future is a prefix future whose newest change is Record's, so a red in it is
// that change's (a block's earlier changes were judged in their own futures; BlameFutures reads across them).
type Job struct {
	Record ChangeRecord
	Change string
	Future string // the tested tree's sha
	Base   string // main's sha the future is built on
	Run    string
	Plan   []PlanUnit
}

// A FuturePost is the body of Queue's futures verdicts route, as proposed to Queue (Oct 9, 23:35Z).
type FuturePost struct {
	Change     string            `json:"change"`
	Run        string            `json:"run"`
	Rule       string            `json:"rule"`
	Plan       []string          `json:"plan"`
	Verdicts   []json.RawMessage `json:"verdicts"`
	Decision   PostDecision      `json:"decision"`
	Quarantine []TestOutcome     `json:"quarantine"`
}

// A PostDecision is Green's decision with the kick for each red unit, which Queue logs verbatim on change.red.
type PostDecision struct {
	RunVerdict
	Kicks map[string]Kick `json:"kicks"`
}

// A Loop judges futures.
type Loop struct {
	Runs   Runs
	Fabric Fabric
	Main   MainRecords
	Queue  Queue
	Now    func() time.Time
}

// JudgeFuture decides every planned unit of one future, posts the result to Queue, and returns what it posted.
func (loop Loop) JudgeFuture(job Job) (FuturePost, error) {
	post := FuturePost{Change: job.Change, Run: job.Run, Rule: Rule, Plan: []string{}, Verdicts: []json.RawMessage{}, Quarantine: []TestOutcome{}}
	verdicts := []Verdict{}
	for _, unit := range job.Plan {
		post.Plan = append(post.Plan, unit.UnitKey)
		verdict, flaky, err := loop.judgeUnit(job, unit)
		if err != nil {
			return FuturePost{}, fmt.Errorf("unit %s: %w", unit.UnitKey, err)
		}
		verdicts = append(verdicts, verdict)
		post.Quarantine = append(post.Quarantine, flaky...)
		encoded, err := verdict.Canonical()
		if err != nil {
			return FuturePost{}, err
		}
		post.Verdicts = append(post.Verdicts, encoded)
	}
	post.Decision = PostDecision{RunVerdict: Green(post.Plan, verdicts), Kicks: map[string]Kick{}}
	for _, verdict := range verdicts {
		if verdict.Status != Failed || verdict.Cause != CauseChange {
			continue
		}
		kick, err := KickFor(Blame{Change: job.Change, UnitKey: verdict.UnitKey, Why: "the change's red in the future it adds"}, job.Record, verdict)
		if err != nil {
			return FuturePost{}, fmt.Errorf("unit %s: %w", verdict.UnitKey, err)
		}
		post.Decision.Kicks[verdict.UnitKey] = kick
	}
	if err := loop.Queue.PostVerdicts(job.Future, post); err != nil {
		return FuturePost{}, fmt.Errorf("posting future %s: %w", job.Future, err)
	}
	return post, nil
}

func (loop Loop) judgeUnit(job Job, unit PlanUnit) (Verdict, []TestOutcome, error) {
	verdict := Verdict{UnitKey: unit.UnitKey, Change: job.Change, Future: job.Future, Run: job.Run, RuleId: Rule,
		Attempts: []Attempt{}, Tests: []TestOutcome{}, Outputs: []string{}}
	if unit.Reused != "" {
		// Planner reuses only an exact key that passed, never uncached (planner/select.go).
		verdict.Status, verdict.RuleId = Passed, Rule+" reused "+unit.Reused
		verdict.DecidedAt = loop.Now().UTC().Format(time.RFC3339)
		return verdict, []TestOutcome{}, nil
	}
	first, found, err := loop.Runs.Finished(job.Run, unit.UnitKey)
	if err != nil {
		return Verdict{}, nil, err
	}
	if !found {
		// A unit that never reported is placed again like any infra.
		first = Finished{Attempt: Attempt{Status: Broken}, Infra: InfraSilent}
	}
	evidence := evidenceOf(first)
	verdict.Attempts = append(verdict.Attempts, first.Attempt)
	verdict.Tests, verdict.Outputs = nonNil(first.Tests), nonNilStrings(first.Outputs)
	budget := InfraRetries
	var decision Decision
	for {
		decision, err = Decide(evidence)
		if err != nil {
			return Verdict{}, nil, err
		}
		if decision.Decided {
			break
		}
		if decision.Next == "retry" {
			if budget == 0 {
				// Infra past its retries: void, never a verdict on the change.
				break
			}
			budget--
			if evidence.Candidate == nil {
				// The attempt itself broke: place it again on the candidate.
				again, err := loop.Fabric.RerunAlone(unit.UnitKey, job.Future)
				if err != nil {
					return Verdict{}, nil, err
				}
				verdict.Attempts = append(verdict.Attempts, again.Attempt)
				verdict.Tests, verdict.Outputs = nonNil(again.Tests), nonNilStrings(again.Outputs)
				evidence = evidenceOf(again)
			} else {
				// An alone rerun broke: run both again.
				evidence.Candidate, evidence.Main = nil, nil
			}
			continue
		}
		// "rerunAlone": a failure is rerun alone on the candidate and on main before its cause is set.
		if err := loop.rerunBoth(job, unit, &evidence, &verdict); err != nil {
			return Verdict{}, nil, err
		}
	}
	verdict.Status, verdict.Cause, verdict.Infra = decision.Status, decision.Cause, decision.Infra
	verdict.DecidedAt = loop.Now().UTC().Format(time.RFC3339)
	return verdict, nonNil(decision.Flaky), nil
}

// rerunBoth runs the unit alone on the candidate and on main at the base, and reads main's record for it.
func (loop Loop) rerunBoth(job Job, unit PlanUnit, evidence *Evidence, verdict *Verdict) error {
	candidate, err := loop.Fabric.RerunAlone(unit.UnitKey, job.Future)
	if err != nil {
		return err
	}
	main, err := loop.Fabric.RerunAlone(unit.UnitKey, job.Base)
	if err != nil {
		return err
	}
	recorded, found, err := loop.Main.Latest(job.Base, unit.UnitKey)
	if err != nil {
		return err
	}
	verdict.Attempts = append(verdict.Attempts, candidate.Attempt, main.Attempt)
	evidence.Candidate = &Rerun{Status: candidate.Attempt.Status, Infra: candidate.Infra, Tests: candidate.Tests}
	evidence.Main = &Rerun{Status: main.Attempt.Status, Infra: main.Infra, Tests: main.Tests}
	evidence.MainRecorded = nil
	if found {
		evidence.MainRecorded = recorded
	}
	return nil
}

func evidenceOf(finished Finished) Evidence {
	return Evidence{First: finished.Attempt, FirstInfra: finished.Infra, FirstTests: finished.Tests, MissingTools: finished.MissingTools}
}

func nonNil(outcomes []TestOutcome) []TestOutcome {
	if outcomes == nil {
		return []TestOutcome{}
	}
	return outcomes
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}

// StubFabric stands in for Fabric's placement until it lands: it answers each (unitKey, tree) from a script, in
// order, and records every placement it was asked for. Remove it when Fabric's RerunAlone lands.
type StubFabric struct {
	Script map[string][]Finished // keyed by unitKey + " " + tree
	Asked  []string
}

// RerunAlone answers from the script.
func (stub *StubFabric) RerunAlone(unitKey, tree string) (Finished, error) {
	key := unitKey + " " + tree
	stub.Asked = append(stub.Asked, key)
	queue := stub.Script[key]
	if len(queue) == 0 {
		return Finished{}, fmt.Errorf("stub fabric has no answer left for %s", key)
	}
	stub.Script[key] = queue[1:]
	return queue[0], nil
}

// StubQueue stands in for Queue's futures verdicts route until it lands: it keeps every post. Remove it when the
// route lands.
type StubQueue struct {
	Posts map[string][]FuturePost
}

// PostVerdicts keeps the post.
func (stub *StubQueue) PostVerdicts(future string, post FuturePost) error {
	if stub.Posts == nil {
		stub.Posts = map[string][]FuturePost{}
	}
	stub.Posts[future] = append(stub.Posts[future], post)
	return nil
}
