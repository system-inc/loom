package judge

// The skip census on the new path (#a1rv4f3, Loom's ruling Oct 10 00:55Z, part of 1.0): every test a future's batch
// skipped must be a classified skip, or the run is red at the census step. This is Adamic's internal/skipcensus
// CheckLogOn (devtools/fast-gate 348ac0ff, log.go), ported over the units' test2json events instead of one merged
// go test -json log, with the same classes, conditions, reason matching and pending rule, so the box and the new path
// read a skip the same way. The rows are the tools tree's internal/skipcensus/testdata/skips.json plus
// cloud/fast-gate/census-extra.json, as run.py passes them (-table and -extra).
//
// Not ported: Validate, which scans the tree's source against the table. The judge has no checkout of the tree; a skip
// that fires with no row still reads unknown here, which is the rule's half that a batch can show.

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A CensusRow is one declared skip site, as skipcensus.Row writes it.
type CensusRow struct {
	File      string   `json:"file"`
	Test      string   `json:"test"`
	ID        string   `json:"id"`
	Line      int      `json:"line"`
	Condition string   `json:"condition"`
	Reads     []string `json:"reads"`
	Callers   []string `json:"callers"`
	Message   string   `json:"message"`
	Class     string   `json:"class"`
	Provides  string   `json:"provides"`
	OptInOn   []string `json:"opt_in_on,omitempty"`
	OptInOff  []string `json:"opt_in_off,omitempty"`
	Awaits    string   `json:"awaits,omitempty"`
	Siblings  []string `json:"siblings,omitempty"`
	Platforms []string `json:"platforms,omitempty"`
}

// LoadCensusRows reads one rows file, refusing unknown fields as skipcensus.Load does, so a table this port doesn't
// understand fails loud instead of reading every skip unknown.
func LoadCensusRows(input io.Reader) ([]CensusRow, error) {
	var rows []CensusRow
	decoder := json.NewDecoder(input)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&rows); err != nil {
		return nil, fmt.Errorf("census rows: %w", err)
	}
	return rows, nil
}

// A TestEvent is one test2json line of a unit's output.
type TestEvent struct {
	Action  string
	Package string
	Test    string
	Output  string
}

// Landed says whether a branch is on main; an error means it can't be told, which fails the census closed.
type Landed func(branch string) (bool, error)

// A CensusResult is the census over one batch: every classed skip, one line each, and the counts.
type CensusResult struct {
	Lines                                               []string
	Skips, Required, Unknown, Pending, Overdue, Covered int
	Heavy                                               int      // declared heavy deferrals, classed heavy
	Failing                                             []string // "<class> <package> <test>" for each skip that fails the census
}

// Failed says whether the batch fails the census: a required input skipped, an unclassified skip, or a pending skip
// past or unsure of its reason.
func (result CensusResult) Failed() bool { return result.Required+result.Unknown+result.Overdue > 0 }

// Summary is skipcensus's last line.
func (result CensusResult) Summary() string {
	return fmt.Sprintf("skips=%d required-input=%d unknown=%d pending=%d covered=%d heavy=%d", result.Skips, result.Required, result.Unknown, result.Pending, result.Covered, result.Heavy)
}

var (
	awaitedBranch  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	awaitsInOutput = regexp.MustCompile(`awaits (#[0-9a-z]{6,9}|[A-Za-z0-9][A-Za-z0-9._/-]*): \S`)
)

func censusConditional(row CensusRow) bool { return len(row.Siblings) > 0 || len(row.Platforms) > 0 }

func reviewLane(pkg, test string) bool {
	return pkg == "github.com/system-inc/adamic/internal/oracle" && strings.HasPrefix(test, "TestReviewPrograms")
}

// checkPending holds a pending row to its reason: it names the branch it awaits, and its message says so.
func checkPending(row CensusRow) error {
	if !awaitedBranch.MatchString(row.Awaits) {
		return fmt.Errorf("a pending skip names the branch it awaits")
	}
	literal, err := strconv.Unquote(row.Message)
	if err != nil {
		literal = row.Message
	}
	if !strings.Contains(literal, "awaits "+row.Awaits) && !strings.Contains(literal, "dependency: "+row.Awaits) {
		return fmt.Errorf("a pending skip's message says %q", "awaits "+row.Awaits)
	}
	return nil
}

// A HeavyUnit is one declared heavy deferral (the gate tools' cloud/fast-gate/heavy-units.tsv): a test or family
// whose coverage the fast gate leaves to main's whole gate or the 30-minute canary, and the owner paged on its red.
type HeavyUnit struct {
	Package string
	Test    string
	Owner   string
	Seconds float64
	Why     string
}

// ParseHeavyUnits reads heavy-units.tsv as run.py's heavyUnits does: package, test or family, owner, budget seconds
// and why, tab-separated; a malformed, unnamed, patterned or repeated row fails closed.
func ParseHeavyUnits(text string) ([]HeavyUnit, error) {
	units := []HeavyUnit{}
	seen := map[string]bool{}
	for number, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(fields) != 5 || slices.ContainsFunc(fields, func(field string) bool { return strings.TrimSpace(field) == "" }) {
			return nil, fmt.Errorf("heavy-units.tsv:%d: a heavy unit needs package, test or family, owner, seconds, why", number+1)
		}
		seconds, err := strconv.ParseFloat(fields[3], 64)
		if err != nil || seconds <= 0 || !strings.HasPrefix(fields[1], "Test") || strings.ContainsAny(fields[1], "*?[]") {
			return nil, fmt.Errorf("heavy-units.tsv:%d: an invalid heavy unit name or budget", number+1)
		}
		if seen[fields[0]+" "+fields[1]] {
			return nil, fmt.Errorf("heavy-units.tsv:%d: heavy unit %s %s is declared twice", number+1, fields[0], fields[1])
		}
		seen[fields[0]+" "+fields[1]] = true
		units = append(units, HeavyUnit{Package: fields[0], Test: fields[1], Owner: fields[2], Seconds: seconds, Why: fields[4]})
	}
	return units, nil
}

var familyShard = regexp.MustCompile(`^(Unit\d+|Points\d+|_\d+)$`)

// familyMember is run.py's: a top-level test is the requested name or one of its split's generated shards (the name
// then Unit<n>, Points<n> or _<n>), never a bare prefix.
func familyMember(name, requested string) bool {
	rest, found := strings.CutPrefix(name, requested)
	return found && (rest == "" || familyShard.MatchString(rest))
}

// heavyDeclaration is the declaration a heavy skip of package and test falls under, as run.py's heavyUnit finds it:
// the exact test, a subtest of it, or a shard of its family. More than one is ambiguous, and fails closed.
func heavyDeclaration(units []HeavyUnit, pkg, test string) (HeavyUnit, bool) {
	test = strings.TrimSuffix(test, " (setup)")
	matches := []HeavyUnit{}
	for _, unit := range units {
		if unit.Package == pkg && (test == unit.Test || strings.HasPrefix(test, unit.Test+"/") || familyMember(strings.SplitN(test, "/", 2)[0], unit.Test)) {
			matches = append(matches, unit)
		}
	}
	if len(matches) != 1 {
		return HeavyUnit{}, false
	}
	return matches[0], true
}

var heavyDeferred = regexp.MustCompile(`heavy: deferred\b`)

// Census classes every skip in events, which are the batch's units' test2json events in plan order, read whole first
// because a pass later in the batch covers an earlier skip of the same test. platform is the GOOS the units ran on.
// Without landed, a pending skip can't be checked and reads unknown.
//
// First, as run.py's checkCensus does, its heavy census (heavyCensus): a skip whose output says "heavy: deferred" is
// classed heavy when heavy-units.tsv declares it, and unknown otherwise, and never reaches the skip rows; any other
// skip is classed by the rows.
func Census(events []TestEvent, rows []CensusRow, heavy []HeavyUnit, landed Landed, platform string) CensusResult {
	result := CensusResult{Lines: []string{}, Failing: []string{}}
	classed := []TestEvent{}
	passed := map[string]bool{}
	lastPass := map[string]int{}
	for _, event := range events {
		switch event.Action {
		case "output", "skip":
			classed = append(classed, event)
		case "pass":
			passed[event.Package+"/"+event.Test] = true
			lastPass[event.Package+"/"+event.Test] = len(classed)
		}
	}
	line := func(format string, values ...any) {
		result.Lines = append(result.Lines, fmt.Sprintf(format, values...))
	}
	fail := func(class string, event TestEvent) {
		result.Failing = append(result.Failing, class+" "+event.Package+" "+event.Test)
	}
	messages := map[string]string{}
	for position, event := range classed {
		key := event.Package + "/" + event.Test
		if event.Action == "output" {
			messages[key] += event.Output
			continue
		}
		if event.Test == "" {
			continue
		}
		result.Skips++
		if heavyDeferred.MatchString(messages[key]) {
			if declared, found := heavyDeclaration(heavy, event.Package, event.Test); found {
				result.Heavy++
				line("heavy\t%s\t%s\t%s\t%.1f s\t%s", event.Package, event.Test, declared.Owner, declared.Seconds, declared.Why)
			} else {
				result.Unknown++
				fail("unknown", event)
				line("unknown\t%s\t%s\theavy: deferred, and heavy-units.tsv declares no such unit", event.Package, event.Test)
			}
			continue
		}
		if last, ok := lastPass[key]; ok && last > position {
			result.Covered++
			line("covered\t%s\t%s\tpassed later in the same batch", event.Package, event.Test)
			continue
		}
		directory := strings.TrimPrefix(event.Package, "github.com/system-inc/adamic/")
		test := strings.Split(event.Test, "/")[0]
		var candidates []CensusRow
		for _, row := range rows {
			if row.File[:max(strings.LastIndex(row.File, "/"), 0)] != directory {
				continue
			}
			if slices.Contains(row.Callers, test) {
				candidates = append(candidates, row)
			}
		}
		if len(candidates) > 1 {
			var matched []CensusRow
			for _, row := range candidates {
				literal, err := strconv.Unquote(row.Message)
				if err == nil && skipReasonMatches(literal, messages[key]) {
					matched = append(matched, row)
				}
			}
			candidates = matched
		}
		// A lone candidate is held to its reason too, so a by-name row can't take a different skip of the same test.
		if len(candidates) == 1 && candidates[0].Class != "pending" {
			if literal, ok := declaredReason(candidates[0]); ok && !skipReasonMatches(literal, messages[key]) {
				candidates = nil
			}
		}
		if found := awaitsInOutput.FindStringSubmatch(messages[key]); len(candidates) == 0 && found != nil {
			target := found[1]
			if strings.HasPrefix(target, "#") {
				if reviewLane(event.Package, test) {
					result.Pending++
					line("pending-task\t%s\t%s\tawaits %s", event.Package, event.Test, target)
					continue
				}
			} else if landed != nil {
				on, err := landed(target)
				switch {
				case err != nil:
					result.Overdue++
					fail("pending-unknown", event)
					line("pending-unknown\t%s\t%s\tskip message\tawaits %s: %v", event.Package, event.Test, target, err)
				case on:
					result.Overdue++
					fail("pending-landed", event)
					line("pending-landed\t%s\t%s\tskip message\tawaits %s, which is on main, and the test still skips", event.Package, event.Test, target)
				default:
					result.Pending++
					line("pending\t%s\t%s\tskip message\tawaits %s", event.Package, event.Test, target)
				}
				continue
			}
		}
		if len(candidates) != 1 {
			result.Unknown++
			fail("unknown", event)
			line("unknown\t%s\t%s", event.Package, event.Test)
			continue
		}
		row := candidates[0]
		if why := unmet(row, event.Package, platform, passed); why != "" {
			result.Unknown++
			fail("unknown", event)
			line("unknown\t%s\t%s\t%s reads unknown: %s", event.Package, event.Test, event.Test, why)
			continue
		}
		if row.Class == "pending" {
			if landed == nil || checkPending(row) != nil {
				result.Unknown++
				fail("unknown", event)
				line("unknown\t%s\t%s", event.Package, event.Test)
				continue
			}
			on, err := landed(row.Awaits)
			switch {
			case err != nil:
				result.Overdue++
				fail("pending-unknown", event)
				line("pending-unknown\t%s\t%s\t%s\tawaits %s: %v", event.Package, event.Test, row.ID, row.Awaits, err)
			case on:
				result.Overdue++
				fail("pending-landed", event)
				line("pending-landed\t%s\t%s\t%s\tawaits %s, which is on main, and the test still skips", event.Package, event.Test, row.ID, row.Awaits)
			default:
				result.Pending++
				line("pending\t%s\t%s\t%s\tawaits %s", event.Package, event.Test, row.ID, row.Awaits)
			}
			continue
		}
		switch row.Class {
		case "required-input", "measurement", "not-applicable", "opt-in-lane":
		default:
			result.Unknown++
			fail("unknown", event)
			line("unknown\t%s\t%s", event.Package, event.Test)
			continue
		}
		line("%s\t%s\t%s\t%s\t%s", row.Class, event.Package, event.Test, row.ID, row.Provides)
		if row.Class == "required-input" {
			result.Required++
			fail("required-input", event)
		}
	}
	return result
}

// unmet says why a conditional row doesn't hold for this skip, or "" when it does (or has no condition).
func unmet(row CensusRow, pkg, platform string, passed map[string]bool) string {
	if !censusConditional(row) {
		return ""
	}
	if row.Class != "not-applicable" {
		return fmt.Sprintf("row %s carries a condition but is class %q, not not-applicable", row.ID, row.Class)
	}
	if len(row.Platforms) > 0 && !slices.Contains(row.Platforms, platform) {
		return fmt.Sprintf("not-applicable only on %s, and this batch ran on %s", strings.Join(row.Platforms, ", "), platform)
	}
	for _, sibling := range row.Siblings {
		if !passed[pkg+"/"+sibling] {
			return fmt.Sprintf("its sibling %s, which runs the work it leaves, has no pass in this batch", sibling)
		}
	}
	return ""
}

// declaredReason is a row's skip reason when the census can hold a skip's output to it: a string literal whose only
// directives are %d.
func declaredReason(row CensusRow) (string, bool) {
	literal, err := strconv.Unquote(row.Message)
	if err != nil || literal == "" || strings.Count(literal, "%") != strings.Count(literal, "%d") {
		return "", false
	}
	return literal, true
}

// skipReasonMatches holds a skip's output to its declared reason; a %d matches any integer.
func skipReasonMatches(literal, output string) bool {
	if strings.Contains(output, literal) {
		return true
	}
	if !strings.Contains(literal, "%d") {
		return false
	}
	pattern := strings.ReplaceAll(regexp.QuoteMeta(literal), "%d", "[+-]?[0-9]+")
	matched, _ := regexp.MatchString(pattern, output)
	return matched
}
