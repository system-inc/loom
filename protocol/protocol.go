// Package protocol holds Loom's wire types and the two decisions every part shares: expanding a job
// into its plan, and deciding a run from the plan and its events. docs/protocol.md explains them.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// A Job is what a person or a tool writes: named units, some expanded by a matrix.
type Job struct {
	Name  string    `json:"name"`
	Units []JobUnit `json:"units"`
}

type JobUnit struct {
	Id     string              `json:"id"`
	Needs  []string            `json:"needs,omitempty"`
	Matrix map[string][]string `json:"matrix,omitempty"`
	Argv   []string            `json:"argv,omitempty"`
	// Test is a structured go test of the adamic repository, in place of argv (test.go).
	Test *TestJob `json:"test,omitempty"`
	// BrokenExit is an exit code the command uses to say its machine couldn't run it (a full disk, a failed checkout):
	// the runner finishes the unit broken, never failed, and the coordinator places it again elsewhere. Zero: none.
	BrokenExit     int               `json:"brokenExit,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	Directory      string            `json:"directory,omitempty"`
	Inputs         []Input           `json:"inputs,omitempty"`
	Outputs        []Output          `json:"outputs,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Resources      Resources         `json:"resources,omitempty"`
	// Products are built actions the unit runs, fetched by key from the action store at ProductStore before the
	// command starts (contracts v1.1): refs/action/<key> names a manifest blob, the manifest lists the outputs, and every
	// hash is checked. A product the store can't give whole finishes the unit broken, never failed. Argv units only, for
	// now: a test job builds its own command and has no place for them yet.
	Products     []Product `json:"products,omitempty"`
	ProductStore string    `json:"productStore,omitempty"`
	// Cache says the unit is hermetic: its result depends on nothing but what its cache key holds, so a
	// pass may stand in for a later unit with the same key. Off by default, since a unit that reads a
	// machine's own state (a warm checkout, say) has inputs its key can't see.
	Cache bool `json:"cache,omitempty"`
	// ExpectedSeconds is the planner's estimate of the unit's wall, which the coordinator places by, longest first,
	// ahead of the wall it recorded for the same id last time: a planner that re-cuts its units every run reuses ids
	// for different work (Oct 9, main c869cea9: units of 310 and 265 s placed last on stale times). Zero: none given.
	ExpectedSeconds float64 `json:"expectedSeconds,omitempty"`
	// Requires names the toolchains the unit's machine must have, from Toolchains: the coordinator places it only on a
	// machine that has them all, and a unit no machine of the run has them for is never placed, so the run is void,
	// never green without it (Oct 9: a wasi shard on a runner without the WASI SDK skips every test and passes).
	// Placement only: the runner never sees it, and it isn't part of the cache key.
	Requires []string `json:"requires,omitempty"`
	// Kind is the unit's kind from the unit key's (contract 2): test, product or phase. A unit of a kind runs under
	// that kind's hard ceiling, its timeout no longer than KindCeilings says, so the runner kills it there at the
	// latest, and a kill is infra, never the change's red. Empty: no kind, no ceiling, as before.
	Kind string `json:"kind,omitempty"`
	// Portable says the unit's verdict is the same on any platform, so a run with a record platform may place it on a
	// machine of another (a Mac). Without it, such a run keeps the unit on the record platform: a platform-specific
	// test proves nothing about Linux on a Mac. Placement only.
	Portable bool `json:"portable,omitempty"`
}

// KindCeilings is each kind's hard ceiling in seconds, the longest timeout a unit of it may carry (Loom, contracts
// v1.1: a test unit is 60 s by design and killed at 90 s; a product gets 600 s). A phase gets the hour every phase
// unit carries today, until a ruling names its own.
var KindCeilings = map[string]int{"test": 90, "product": 600, "phase": 3600}

// Toolchains are the names a unit may require and a machine may have, the unit key's tools (contract v1).
var Toolchains = []string{"go", "clang", "node", "wasiSdk"}

// A Product is one built action a unit runs: its productKey (contract 2) and the directory in the workspace its outputs
// land in, each at its manifest path.
type Product struct {
	Key       string `json:"key"`
	Directory string `json:"directory"`
}

type Input struct {
	Path    string `json:"path"`
	Sha256  string `json:"sha256"`
	Mode    string `json:"mode,omitempty"`
	Archive string `json:"archive,omitempty"`
}

type Output struct {
	Glob string `json:"glob"`
}

type Resources struct {
	Cpus            int `json:"cpus,omitempty"`
	MemoryMegabytes int `json:"memoryMegabytes,omitempty"`
}

// A Unit is one command handed to one runner.
type Unit struct {
	Run  string   `json:"run"`
	Unit string   `json:"unit"`
	Argv []string `json:"argv,omitempty"`
	// Test is a structured go test of the adamic repository, in place of argv: a strict runner runs only these.
	Test *TestJob `json:"test,omitempty"`
	// BrokenExit is the exit code that means the machine couldn't run the unit: it finishes broken (JobUnit's).
	BrokenExit     int               `json:"brokenExit,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	Directory      string            `json:"directory,omitempty"`
	Inputs         []Input           `json:"inputs,omitempty"`
	Outputs        []Output          `json:"outputs,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Resources      Resources         `json:"resources,omitempty"`
	// Products and ProductStore: the built actions the runner fetches by key before the command (JobUnit's).
	Products     []Product `json:"products,omitempty"`
	ProductStore string    `json:"productStore,omitempty"`
	Store        *Endpoint `json:"store,omitempty"`
	Wire         *Endpoint `json:"wire,omitempty"`
	Token        string    `json:"token,omitempty"`
	// SequenceStart is the sequence the runner's first event takes: 0 for a first attempt. A pool unit placed
	// again continues its stream on the wire from where the record stands, since its runner posts there itself.
	SequenceStart int `json:"sequenceStart,omitempty"`
}

type Endpoint struct {
	Url string `json:"url"`
}

// An Event is one line of a unit's stream. Fields beyond the common five depend on Type. Those fields are
// written with omitempty, so a zero is left out (an empty output line has no text, a zero-byte upload no
// bytes, an instant exit no wallSeconds): a field the type allows but the line lacks reads as its zero.
// code is the exception, a pointer, so exit code 0 is always written and a missing code means a signal.
type Event struct {
	Run      string `json:"run"`
	Unit     string `json:"unit"`
	Sequence int    `json:"sequence"`
	Time     string `json:"time"`
	Type     string `json:"type"`

	// started
	Machine       string `json:"machine,omitempty"`
	RunnerVersion string `json:"runnerVersion,omitempty"`
	// RunnerSha256 is the runner binary's own sha256 (/proc/self/exe), the form a unit key's tools.runner part takes,
	// so the judge can void an attempt that ran on a runner its key doesn't name (Loom, Oct 10 01:52Z).
	RunnerSha256    string            `json:"runnerSha256,omitempty"`
	Cpus            int               `json:"cpus,omitempty"`
	MemoryMegabytes int               `json:"memoryMegabytes,omitempty"`
	InputHashes     map[string]string `json:"inputs,omitempty"`
	// HeartbeatSeconds is the longest the runner lets a unit go without an event: past it, the runner says the
	// unit is still running. A watcher may take a longer silence from such a runner as the runner gone.
	HeartbeatSeconds float64 `json:"heartbeatSeconds,omitempty"`
	// output
	Stream   string `json:"stream,omitempty"`
	Text     string `json:"text,omitempty"`
	Replaced bool   `json:"replaced,omitempty"`
	// exit
	Code          *int    `json:"code,omitempty"`
	Signal        string  `json:"signal,omitempty"`
	TimedOut      bool    `json:"timedOut,omitempty"`
	WallSeconds   float64 `json:"wallSeconds,omitempty"`
	UserSeconds   float64 `json:"userSeconds,omitempty"`
	SystemSeconds float64 `json:"systemSeconds,omitempty"`
	// uploaded
	Path   string `json:"path,omitempty"`
	Sha256 string `json:"sha256,omitempty"`
	Bytes  int64  `json:"bytes,omitempty"`
	// error
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message,omitempty"`
	// cached
	Key      string `json:"key,omitempty"`
	FromRun  string `json:"fromRun,omitempty"`
	EventLog string `json:"events,omitempty"`
	// finished
	Status string `json:"status,omitempty"`
}

// A unit's finished status. passed: exit 0 and every output uploaded. failed: the command's doing (a nonzero
// exit, a timeout, a signal, or an output it declared but didn't produce). broken: the runner couldn't do its
// job (a bad unit, a fetch, start or upload that failed, or the runner itself stopped).
const (
	StatusPassed = "passed"
	StatusFailed = "failed"
	StatusBroken = "broken"
)

// An error event's phase: where the runner was when it went wrong.
const (
	PhaseFetch  = "fetch"  // fetching or verifying an input
	PhaseStart  = "start"  // checking the unit, making its workspace, starting its command
	PhaseRun    = "run"    // while the command runs: the runner stopped, output unreadable, a process escaping the group
	PhaseUpload = "upload" // finding, hashing or uploading a declared output
	PhaseWire   = "wire"   // posting events to the wire; never changes the status, since stdout holds the stream
	PhasePlace  = "place"  // the coordinator's own: a box that dropped the unit, and where it was placed again
)

// Decode reads exactly one JSON value into value, refusing unknown fields and trailing data.
func Decode(reader io.Reader, value any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("trailing data after the JSON value")
	}
	var rest bytes.Buffer
	if _, err := io.Copy(&rest, decoder.Buffered()); err == nil && len(bytes.TrimSpace(rest.Bytes())) > 0 {
		return fmt.Errorf("trailing data after the JSON value")
	}
	return nil
}

var unitIdPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
var matrixReference = regexp.MustCompile(`\$\{matrix\.([a-zA-Z0-9_]+)\}`)

// A PlannedUnit is a job unit after matrix expansion: its planned id, the base ids it needs, and its fields.
type PlannedUnit struct {
	Id    string
	Base  string
	Needs []string
	Unit  JobUnit
}

// Expand checks a job and expands its matrices into the plan, in job order, each matrix's combinations
// in sorted key order. The plan is the set of units a green run must account for, each exactly once.
func Expand(job Job) ([]PlannedUnit, error) {
	if job.Name == "" {
		return nil, fmt.Errorf("job has no name")
	}
	if len(job.Units) == 0 {
		return nil, fmt.Errorf("job %s has no units", job.Name)
	}
	bases := map[string]bool{}
	for _, unit := range job.Units {
		if !unitIdPattern.MatchString(unit.Id) {
			return nil, fmt.Errorf("unit id %q: lowercase letters, digits and dashes only", unit.Id)
		}
		if bases[unit.Id] {
			return nil, fmt.Errorf("unit id %s appears twice", unit.Id)
		}
		bases[unit.Id] = true
	}
	var plan []PlannedUnit
	for _, unit := range job.Units {
		if (len(unit.Argv) == 0) == (unit.Test == nil) {
			return nil, fmt.Errorf("unit %s needs argv or a test job, exactly one", unit.Id)
		}
		if unit.Test != nil {
			if len(unit.Matrix) > 0 {
				return nil, fmt.Errorf("unit %s: a test job takes no matrix", unit.Id)
			}
			if err := CheckTestJob(*unit.Test); err != nil {
				return nil, fmt.Errorf("unit %s: %w", unit.Id, err)
			}
		}
		if unit.TimeoutSeconds <= 0 {
			return nil, fmt.Errorf("unit %s needs a positive timeoutSeconds", unit.Id)
		}
		if unit.Test != nil && unit.Test.Phase != "" && unit.Kind != "phase" {
			return nil, fmt.Errorf("unit %s is a phase job, so its kind is phase, not %q", unit.Id, unit.Kind)
		}
		if unit.Kind != "" {
			ceiling, known := KindCeilings[unit.Kind]
			switch {
			case !known:
				return nil, fmt.Errorf("unit %s's kind %q isn't test, product or phase", unit.Id, unit.Kind)
			case unit.TimeoutSeconds > ceiling:
				return nil, fmt.Errorf("unit %s is a %s with a %d s timeout, over its kind's ceiling of %d s", unit.Id, unit.Kind, unit.TimeoutSeconds, ceiling)
			case unit.Test != nil && unit.Kind == "phase" && unit.Test.Phase == "":
				return nil, fmt.Errorf("unit %s is a go test job, so it's a test or a product, not a phase", unit.Id)
			}
		}
		for _, toolchain := range unit.Requires {
			if !slices.Contains(Toolchains, toolchain) {
				return nil, fmt.Errorf("unit %s requires %q, which isn't a toolchain (%s)", unit.Id, toolchain, strings.Join(Toolchains, ", "))
			}
		}
		for _, need := range unit.Needs {
			if !bases[need] || need == unit.Id {
				return nil, fmt.Errorf("unit %s needs %q, which isn't another unit of this job", unit.Id, need)
			}
		}
		for _, input := range unit.Inputs {
			if !Sha256Pattern.MatchString(input.Sha256) && !matrixReference.MatchString(input.Sha256) {
				return nil, fmt.Errorf("unit %s input %s: sha256 must be 64 lowercase hex digits", unit.Id, input.Path)
			}
			if input.Archive != "" && input.Archive != "tar" {
				return nil, fmt.Errorf("unit %s input %s: archive %q isn't tar", unit.Id, input.Path, input.Archive)
			}
		}
		for _, combination := range combinations(unit.Matrix) {
			expanded, err := substitute(unit, combination)
			if err != nil {
				return nil, err
			}
			plan = append(plan, PlannedUnit{Id: plannedId(unit.Id, combination), Base: unit.Id, Needs: unit.Needs, Unit: expanded})
		}
	}
	if err := acyclic(job); err != nil {
		return nil, err
	}
	return plan, nil
}

func combinations(matrix map[string][]string) []map[string]string {
	keys := make([]string, 0, len(matrix))
	for key := range matrix {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := []map[string]string{{}}
	for _, key := range keys {
		var next []map[string]string
		for _, partial := range result {
			for _, value := range matrix[key] {
				combination := map[string]string{key: value}
				for k, v := range partial {
					combination[k] = v
				}
				next = append(next, combination)
			}
		}
		result = next
	}
	return result
}

func plannedId(base string, combination map[string]string) string {
	if len(combination) == 0 {
		return base
	}
	keys := make([]string, 0, len(combination))
	for key := range combination {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for index, key := range keys {
		parts[index] = key + "=" + combination[key]
	}
	return base + "[" + strings.Join(parts, ",") + "]"
}

func substitute(unit JobUnit, combination map[string]string) (JobUnit, error) {
	var missing error
	replace := func(text string) string {
		return matrixReference.ReplaceAllStringFunc(text, func(reference string) string {
			key := matrixReference.FindStringSubmatch(reference)[1]
			value, ok := combination[key]
			if !ok {
				missing = fmt.Errorf("unit %s refers to ${matrix.%s}, which its matrix doesn't define", unit.Id, key)
			}
			return value
		})
	}
	expanded := unit
	expanded.Matrix = nil
	if unit.Argv != nil {
		expanded.Argv = make([]string, len(unit.Argv))
		for index, argument := range unit.Argv {
			expanded.Argv[index] = replace(argument)
		}
	}
	if unit.Environment != nil {
		expanded.Environment = map[string]string{}
		for key, value := range unit.Environment {
			expanded.Environment[key] = replace(value)
		}
	}
	expanded.Directory = replace(unit.Directory)
	expanded.Inputs = make([]Input, len(unit.Inputs))
	for index, input := range unit.Inputs {
		input.Path = replace(input.Path)
		input.Sha256 = replace(input.Sha256)
		if !Sha256Pattern.MatchString(input.Sha256) {
			return JobUnit{}, fmt.Errorf("unit %s input %s: sha256 must be 64 lowercase hex digits after expansion", unit.Id, input.Path)
		}
		expanded.Inputs[index] = input
	}
	expanded.Outputs = make([]Output, len(unit.Outputs))
	for index, output := range unit.Outputs {
		expanded.Outputs[index] = Output{Glob: replace(output.Glob)}
	}
	if len(unit.Inputs) == 0 {
		expanded.Inputs = nil
	}
	if len(unit.Outputs) == 0 {
		expanded.Outputs = nil
	}
	return expanded, missing
}

func acyclic(job Job) error {
	needs := map[string][]string{}
	for _, unit := range job.Units {
		needs[unit.Id] = unit.Needs
	}
	state := map[string]int{} // 0 unseen, 1 on the path, 2 done
	var visit func(id string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("unit %s needs itself through a cycle", id)
		case 2:
			return nil
		}
		state[id] = 1
		for _, need := range needs[id] {
			if err := visit(need); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	for _, unit := range job.Units {
		if err := visit(unit.Id); err != nil {
			return err
		}
	}
	return nil
}

// A Verdict is the coordinator's decision on one run, and the body of the wire's verdict endpoint.
type Verdict struct {
	Status   string   `json:"status"`   // "green", "red" or "void"
	Failed   []string `json:"failed"`   // planned unit ids that finished failed
	Problems []string `json:"problems"` // why a run is void, one line each
	Cached   []string `json:"cached"`   // planned unit ids served from the cache instead of run
}

// MarshalJSON writes an empty list as [], never null, so every reader sees the same shape.
func (verdict Verdict) MarshalJSON() ([]byte, error) {
	type plain Verdict
	if verdict.Failed == nil {
		verdict.Failed = []string{}
	}
	if verdict.Problems == nil {
		verdict.Problems = []string{}
	}
	if verdict.Cached == nil {
		verdict.Cached = []string{}
	}
	return json.Marshal(plain(verdict))
}

// A Plan is the body of the wire's plan endpoint: every planned unit id in plan order, and every input hash
// the plan names, sorted. A runner token may read only those inputs and what its own run uploads.
type Plan struct {
	Units  []string `json:"units"`
	Inputs []string `json:"inputs"`
}

// PlanOf names an expanded job's units and inputs in the form the wire takes.
func PlanOf(plan []PlannedUnit) Plan {
	units := make([]string, len(plan))
	seen := map[string]bool{}
	inputs := []string{}
	for index, unit := range plan {
		units[index] = unit.Id
		for _, input := range unit.Unit.Inputs {
			if !seen[input.Sha256] {
				seen[input.Sha256] = true
				inputs = append(inputs, input.Sha256)
			}
		}
	}
	sort.Strings(inputs)
	return Plan{Units: units, Inputs: inputs}
}

// Decide reads a run's events against its plan. Green needs every planned unit to have finished passed,
// each exactly once, with an unbroken sequence; any failed unit makes it red; anything else is void.
// Void wins over red: a run with a lost event or a stray unit proved nothing either way.
func Decide(run string, plan []string, events []Event) Verdict {
	planned := map[string]bool{}
	for _, id := range plan {
		planned[id] = true
	}
	type stream struct {
		next     int
		finished []string
		cached   bool
		reopened bool // placed again after a broken attempt: events may follow its finished
	}
	streams := map[string]*stream{}
	var problems []string
	for _, event := range events {
		if event.Run != run {
			problems = append(problems, fmt.Sprintf("event of run %q in run %s", event.Run, run))
			continue
		}
		if !planned[event.Unit] {
			problems = append(problems, fmt.Sprintf("unit %s isn't in the plan", event.Unit))
			continue
		}
		current := streams[event.Unit]
		if current == nil {
			current = &stream{}
			streams[event.Unit] = current
		}
		if event.Sequence != current.next {
			problems = append(problems, fmt.Sprintf("unit %s: event %d where %d was next", event.Unit, event.Sequence, current.next))
		}
		current.next = event.Sequence + 1
		if event.Type == "cached" {
			current.cached = true
		}
		switch {
		case event.Type == "finished":
			current.finished = append(current.finished, event.Status)
			current.reopened = false
		case len(current.finished) == 0 || current.reopened:
		case current.finished[len(current.finished)-1] == StatusBroken && event.Type == "error" && event.Phase == PhasePlace:
			// The coordinator placed a broken unit again (its machine couldn't run it): its stream goes on, and the
			// last attempt's finished decides it.
			current.reopened = true
		default:
			problems = append(problems, fmt.Sprintf("unit %s: %s event after finished", event.Unit, event.Type))
		}
	}
	var failed, cached []string
	for _, id := range plan {
		current := streams[id]
		if current != nil && current.cached {
			if len(current.finished) == 1 && current.finished[0] != StatusPassed {
				problems = append(problems, fmt.Sprintf("unit %s came from the cache but finished %s; only passed units are cached", id, current.finished[0]))
			}
			cached = append(cached, id)
		}
		// Every attempt but the last finished broken and was placed again; the last decides the unit.
		last := ""
		if current != nil && len(current.finished) > 0 {
			last = current.finished[len(current.finished)-1]
		}
		switch {
		case current == nil || len(current.finished) == 0:
			problems = append(problems, fmt.Sprintf("unit %s never finished", id))
		case current.reopened:
			problems = append(problems, fmt.Sprintf("unit %s was placed again and never finished", id))
		case slices.ContainsFunc(current.finished[:len(current.finished)-1], func(status string) bool { return status != StatusBroken }):
			problems = append(problems, fmt.Sprintf("unit %s finished %d times", id, len(current.finished)))
		case last == StatusFailed:
			failed = append(failed, id)
		case last == StatusBroken:
			problems = append(problems, fmt.Sprintf("unit %s is broken: the runner couldn't do its job", id))
		case last != StatusPassed:
			problems = append(problems, fmt.Sprintf("unit %s finished with unknown status %q", id, last))
		}
	}
	if len(planned) != len(plan) {
		problems = append(problems, "the plan names a unit twice")
	}
	switch {
	case len(problems) > 0:
		return Verdict{Status: "void", Failed: failed, Problems: problems, Cached: cached}
	case len(failed) > 0:
		return Verdict{Status: "red", Failed: failed, Cached: cached}
	default:
		return Verdict{Status: "green", Cached: cached}
	}
}
