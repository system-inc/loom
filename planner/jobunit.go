package planner

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/system-inc/loom/protocol"
)

// toolchainOf names each tools part a unit's machine must have, by protocol.Toolchains' names.
var toolchainOf = map[string]func(Tools) string{
	"go": func(tools Tools) string { return tools.Go }, "clang": func(tools Tools) string { return tools.Clang },
	"node": func(tools Tools) string { return tools.Node }, "wasiSdk": func(tools Tools) string { return tools.WasiSdk },
}

var notIdCharacters = regexp.MustCompile(`[^a-z0-9]+`)

// testOutputs are what a strict runner's go test job writes, and the only outputs it may declare.
var testOutputs = []protocol.Output{{Glob: "loom-out/test.jsonl.gz"}, {Glob: "loom-out/cpu.tsv"}}

// JobUnitFor is the job that runs one planned test unit at a commit, for Judge's reruns (#82tz9ty): a structured test
// job of the unit's package and selection, under the test kind's ceiling, requiring every toolchain its key names.
// It carries no cache flag, so a rerun reuses nothing. The id is the package's path and the commit, so a rerun of
// one unit on the candidate and on main's base never share one.
func JobUnitFor(parts KeyParts, sha string) (protocol.JobUnit, error) {
	if parts.Kind != "test" {
		return protocol.JobUnit{}, fmt.Errorf("a %q unit has no job yet; only test units rerun", parts.Kind)
	}
	test, err := goTestJob(parts, sha, nil)
	if err != nil {
		return protocol.JobUnit{}, err
	}
	directory := strings.TrimPrefix(parts.Package, protocol.AdamicModule+"/")
	id := strings.Trim(notIdCharacters.ReplaceAllString(strings.ToLower(directory), "-"), "-") + "-" + sha[:12]
	return jobUnitOf(id, parts, test, testOutputs)
}

// FutureJobUnit is the job that runs one planned unit of any kind in its future's run, for the placer (`loom place`).
// Its id is the unit's key, which is how the judge reads a future run's units (coordinator.FutureRun). A test or
// product unit is a go test job of its package and selection; a phase unit is a phase job of its run.py line at the
// key's gate tools commit, merged onto base, as PhaseUnits says the placer builds it (gofmt, which the runner runs
// itself, likewise). changed are the future's changed paths, carried only for a unit keyed on them, and only when they
// hash to the key's ADAMIC_GATE_CHANGED. A unit
// that can't be said exactly as a job is refused with why, never placed as something near it.
func FutureJobUnit(unitKey string, parts KeyParts, sha, base string, changed []string) (protocol.JobUnit, error) {
	switch parts.Kind {
	case "test", "product":
		// A product unit is its package's TestProduct_X under the product kind's ceiling (ProductKeys).
		test, err := goTestJob(parts, sha, changed)
		if err != nil {
			return protocol.JobUnit{}, err
		}
		return jobUnitOf(unitKey, parts, test, testOutputs)
	case "phase":
		test, err := phaseJob(parts, sha, base, changed)
		if err != nil {
			return protocol.JobUnit{}, err
		}
		// run.py writes its own record under loom-out/phase; a phase has no test log, and the judge asks for none.
		return jobUnitOf(unitKey, parts, test, nil)
	}
	return protocol.JobUnit{}, fmt.Errorf("a %q unit has no job: its kind isn't test, product or phase", parts.Kind)
}

// goTestJob is the go test job of a test or product unit's package and selection at sha. The four gate switches are
// the strict runner's own; a sample and changed paths are carried as the job's fields, the paths only when they hash
// to what the key holds. Any other env the key names can't be said by a test job, so the unit is refused.
func goTestJob(parts KeyParts, sha string, changed []string) (*protocol.TestJob, error) {
	test := &protocol.TestJob{Repository: protocol.AdamicRepository, Sha: sha, GateInputs: parts.GateInputs,
		Packages: []protocol.TestPackage{{Package: parts.Package, Run: parts.Select.Run, Skip: parts.Select.Skip}}}
	for name, value := range parts.Env {
		switch {
		case GateEnvironment[name] == value:
			// The strict runner sets the four switches itself.
		case name == "ADAMIC_GATE_SAMPLE":
			test.Sample = value
		case name == "ADAMIC_GATE_CHANGED" && changed != nil:
			// The key holds the file's sha256, so the paths come from the change's record, checked against it.
			if sum := ChangedPathsSum(changed); sum != value {
				return nil, fmt.Errorf("unit %s is keyed on changed paths %.12s, and the change's %d paths hash to %.12s", parts.Package, value, len(changed), sum)
			}
			test.ChangedPaths = sortedCopy(changed)
		default:
			// ADAMIC_GATE_CHANGED is in the key as its file's sha256, so its paths can't be rebuilt from the key.
			return nil, fmt.Errorf("unit %s sets %s=%s, which a test job can't carry", parts.Package, name, value)
		}
	}
	return test, nil
}

// phaseJob is the phase job of a phase unit: its run.py unit line at the gate tools commit its key names, at sha
// merged onto base. gofmt's (protocol.GofmtPhase), which the runner runs itself, is keyed on the change's paths, so it
// carries them, checked against the key as goTestJob checks a test's; any other phase carries none. Any env a phase
// job can't say is refused.
func phaseJob(parts KeyParts, sha, base string, changed []string) (*protocol.TestJob, error) {
	line := parts.Select.Run
	test := &protocol.TestJob{Repository: protocol.AdamicRepository, Sha: sha, Base: base, GateInputs: parts.GateInputs,
		Phase: line, Tools: parts.GateTools}
	for name, value := range parts.Env {
		switch {
		case GateEnvironment[name] == value:
			// The strict runner sets the four switches itself.
		case name == "ADAMIC_GATE_CHANGED" && line == protocol.GofmtPhase && changed != nil:
			if sum := ChangedPathsSum(changed); sum != value {
				return nil, fmt.Errorf("phase %q is keyed on changed paths %.12s, and the change's %d paths hash to %.12s", line, value, len(changed), sum)
			}
			test.ChangedPaths = sortedCopy(changed)
		default:
			return nil, fmt.Errorf("phase %q sets %s=%s, which a phase job can't carry", line, name, value)
		}
	}
	return test, nil
}

// jobUnitOf wraps a checked test job as a job unit of the key's kind, under its ceiling, requiring every toolchain
// its key names.
func jobUnitOf(id string, parts KeyParts, test *protocol.TestJob, outputs []protocol.Output) (protocol.JobUnit, error) {
	if err := protocol.CheckTestJob(*test); err != nil {
		return protocol.JobUnit{}, err
	}
	requires := []string{}
	for _, name := range protocol.Toolchains {
		if toolchainOf[name](parts.Tools) != "" {
			requires = append(requires, name)
		}
	}
	return protocol.JobUnit{
		Id: id, Test: test, Kind: parts.Kind, TimeoutSeconds: protocol.KindCeilings[parts.Kind], Requires: requires, Outputs: outputs,
	}, nil
}

func sortedCopy(paths []string) []string {
	sorted := append([]string{}, paths...)
	slices.Sort(sorted)
	return sorted
}

// KeyAt keys a planned unit's identity (its kind, package, selection, env, gate inputs and tools) on another tree, as
// PlanTree would key it there: Judge reruns a failed unit on main's base, whose closure, reads and products differ.
func KeyAt(tree, gateTools string, parts KeyParts) (KeyParts, string, error) {
	module, err := modulePath(tree)
	if err != nil {
		return KeyParts{}, "", err
	}
	declared, err := compilerDeclarations(tree)
	if err != nil {
		return KeyParts{}, "", err
	}
	directory := strings.TrimPrefix(strings.TrimPrefix(parts.Package, module), "/")
	compilers := []string{}
	for _, input := range declared[directory] {
		compilers = append(compilers, module+"/"+input)
	}
	productKeys, err := TestProductKeys(tree, gateTools, parts.Tools)
	if err != nil {
		return KeyParts{}, "", err
	}
	unit := Unit{Kind: parts.Kind, Package: parts.Package, Directory: directory, Run: parts.Select.Run, Skip: parts.Select.Skip,
		GateInputs: parts.GateInputs, Products: UnitProducts(productKeys, parts.Package, compilers)}
	keyed, err := KeyFor(tree, gateTools, unit, parts.Tools, compilers)
	if err != nil {
		return KeyParts{}, "", err
	}
	// The env part is already a key's (ADAMIC_GATE_CHANGED as a sha256), so it carries over as it is.
	keyed.Env = parts.Env
	key, err := UnitKey(keyed)
	return keyed, key, err
}
