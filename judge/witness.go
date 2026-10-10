package judge

// The uncached witness (#82d430f): until read tracing lands, the lock on key reuse is an uncached whole run of main
// every hour, compared with what the planner would have reused at the same tree. A unit the planner reused, whose
// verdict at that key passed, but which reds when run uncached, is a key that misses an input: a P0 against the keys,
// paged to Loom. The comparison here is pure; the hourly run itself goes through Queue's witness future.

// A WitnessReport is what the uncached run says about the planner's reuse at one tree.
type WitnessReport struct {
	KeyFaults   []string // reused by the planner, red uncached: a P0 against the keys
	Unwitnessed []string // reused by the planner, with no uncached verdict to check it against: the witness is incomplete
	Reds        []string // run (not reused) by the planner and red uncached: an ordinary red, judged as any other
	Voids       []string // the witness's own voids: rerun them before the witness counts
}

// Clean says the witness found no key fault and checked every reused unit.
func (report WitnessReport) Clean() bool {
	return len(report.KeyFaults) == 0 && len(report.Unwitnessed) == 0 && len(report.Voids) == 0
}

// CompareWitness reads the uncached run's verdicts against the planner's plan for the same tree. Keys match exactly:
// uncached mode ignores reuse, it doesn't change a key (planner), so the reused unit and its uncached run share one.
func CompareWitness(plan []PlannedUnitWire, witness []Verdict) WitnessReport {
	report := WitnessReport{KeyFaults: []string{}, Unwitnessed: []string{}, Reds: []string{}, Voids: []string{}}
	byKey := map[string]Verdict{}
	for _, verdict := range witness {
		byKey[verdict.UnitKey] = verdict
	}
	for _, unit := range plan {
		verdict, found := byKey[unit.UnitKey]
		reused := unit.Decision == "reuse"
		switch {
		case !found && reused:
			report.Unwitnessed = append(report.Unwitnessed, unit.UnitKey)
		case !found:
		case verdict.Status == Void:
			report.Voids = append(report.Voids, unit.UnitKey)
		case verdict.Status == Failed && reused:
			report.KeyFaults = append(report.KeyFaults, unit.UnitKey)
		case verdict.Status == Failed:
			report.Reds = append(report.Reds, unit.UnitKey)
		}
	}
	return report
}
