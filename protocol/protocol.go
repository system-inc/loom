// Package protocol holds Loom's wire types and the two decisions every part shares: expanding a job
// into its plan, and deciding a run from the plan and its events. docs/protocol.md explains them.
package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// A Job is what a person or a tool writes: named units, some expanded by a matrix.
type Job struct {
	Name  string    `json:"name"`
	Units []JobUnit `json:"units"`
}

type JobUnit struct {
	Id             string              `json:"id"`
	Needs          []string            `json:"needs,omitempty"`
	Matrix         map[string][]string `json:"matrix,omitempty"`
	Argv           []string            `json:"argv"`
	Environment    map[string]string   `json:"environment,omitempty"`
	Directory      string              `json:"directory,omitempty"`
	Inputs         []Input             `json:"inputs,omitempty"`
	Outputs        []Output            `json:"outputs,omitempty"`
	TimeoutSeconds int                 `json:"timeoutSeconds"`
	Resources      Resources           `json:"resources,omitempty"`
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
	Run            string            `json:"run"`
	Unit           string            `json:"unit"`
	Argv           []string          `json:"argv"`
	Environment    map[string]string `json:"environment,omitempty"`
	Directory      string            `json:"directory,omitempty"`
	Inputs         []Input           `json:"inputs,omitempty"`
	Outputs        []Output          `json:"outputs,omitempty"`
	TimeoutSeconds int               `json:"timeoutSeconds"`
	Resources      Resources         `json:"resources,omitempty"`
	Store          *Endpoint         `json:"store,omitempty"`
	Wire           *Endpoint         `json:"wire,omitempty"`
	Token          string            `json:"token,omitempty"`
}

type Endpoint struct {
	Url string `json:"url"`
}

// An Event is one line of a unit's stream. Fields beyond the common four depend on Type.
type Event struct {
	Run      string `json:"run"`
	Unit     string `json:"unit"`
	Sequence int    `json:"sequence"`
	Time     string `json:"time"`
	Type     string `json:"type"`

	// started
	Machine         string            `json:"machine,omitempty"`
	RunnerVersion   string            `json:"runnerVersion,omitempty"`
	Cpus            int               `json:"cpus,omitempty"`
	MemoryMegabytes int               `json:"memoryMegabytes,omitempty"`
	InputHashes     map[string]string `json:"inputs,omitempty"`
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
	// finished
	Status string `json:"status,omitempty"`
}

const (
	StatusPassed = "passed"
	StatusFailed = "failed"
	StatusBroken = "broken"
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
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
		if len(unit.Argv) == 0 {
			return nil, fmt.Errorf("unit %s has no argv", unit.Id)
		}
		if unit.TimeoutSeconds <= 0 {
			return nil, fmt.Errorf("unit %s needs a positive timeoutSeconds", unit.Id)
		}
		for _, need := range unit.Needs {
			if !bases[need] || need == unit.Id {
				return nil, fmt.Errorf("unit %s needs %q, which isn't another unit of this job", unit.Id, need)
			}
		}
		for _, input := range unit.Inputs {
			if !sha256Pattern.MatchString(input.Sha256) && !matrixReference.MatchString(input.Sha256) {
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
	expanded.Argv = make([]string, len(unit.Argv))
	for index, argument := range unit.Argv {
		expanded.Argv[index] = replace(argument)
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
		if !sha256Pattern.MatchString(input.Sha256) {
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

// A Verdict is the coordinator's decision on one run.
type Verdict struct {
	Status   string   // "green", "red" or "void"
	Failed   []string // planned unit ids that finished failed
	Problems []string // why a run is void, one line each
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
		if event.Type == "finished" {
			current.finished = append(current.finished, event.Status)
		} else if len(current.finished) > 0 {
			problems = append(problems, fmt.Sprintf("unit %s: %s event after finished", event.Unit, event.Type))
		}
	}
	var failed []string
	for _, id := range plan {
		current := streams[id]
		switch {
		case current == nil || len(current.finished) == 0:
			problems = append(problems, fmt.Sprintf("unit %s never finished", id))
		case len(current.finished) > 1:
			problems = append(problems, fmt.Sprintf("unit %s finished %d times", id, len(current.finished)))
		case current.finished[0] == StatusFailed:
			failed = append(failed, id)
		case current.finished[0] == StatusBroken:
			problems = append(problems, fmt.Sprintf("unit %s is broken: the runner couldn't do its job", id))
		case current.finished[0] != StatusPassed:
			problems = append(problems, fmt.Sprintf("unit %s finished with unknown status %q", id, current.finished[0]))
		}
	}
	if len(planned) != len(plan) {
		problems = append(problems, "the plan names a unit twice")
	}
	switch {
	case len(problems) > 0:
		return Verdict{Status: "void", Failed: failed, Problems: problems}
	case len(failed) > 0:
		return Verdict{Status: "red", Failed: failed}
	default:
		return Verdict{Status: "green"}
	}
}
