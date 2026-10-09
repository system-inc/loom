package planner

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/system-inc/loom/protocol"
)

// toolchainOf names each tools part a unit's machine must have, by protocol.Toolchains' names.
var toolchainOf = map[string]func(Tools) string{
	"go": func(tools Tools) string { return tools.Go }, "clang": func(tools Tools) string { return tools.Clang },
	"node": func(tools Tools) string { return tools.Node }, "wasiSdk": func(tools Tools) string { return tools.WasiSdk },
}

var notIdCharacters = regexp.MustCompile(`[^a-z0-9]+`)

// JobUnitFor is the job that runs one planned test unit at a commit, for Judge's reruns (#82tz9ty): a structured test
// job of the unit's package and selection, under the test kind's ceiling, requiring every toolchain its key names.
// It carries no cache flag, so a rerun reuses nothing. The id is the package's path and the commit, so a rerun of
// one unit on the candidate and on main's base never share one.
func JobUnitFor(parts KeyParts, sha string) (protocol.JobUnit, error) {
	if parts.Kind != "test" {
		return protocol.JobUnit{}, fmt.Errorf("a %q unit has no job yet; only test units rerun", parts.Kind)
	}
	test := &protocol.TestJob{Repository: protocol.AdamicRepository, Sha: sha, GateInputs: parts.GateInputs,
		Packages: []protocol.TestPackage{{Package: parts.Package, Run: parts.Select.Run, Skip: parts.Select.Skip}}}
	for name, value := range parts.Env {
		switch {
		case GateEnvironment[name] == value:
			// The strict runner sets the four switches itself.
		case name == "ADAMIC_GATE_SAMPLE":
			test.Sample = value
		default:
			// ADAMIC_GATE_CHANGED is in the key as its file's sha256, so its paths can't be rebuilt from the key.
			return protocol.JobUnit{}, fmt.Errorf("unit %s sets %s=%s, which a test job can't carry", parts.Package, name, value)
		}
	}
	if err := protocol.CheckTestJob(*test); err != nil {
		return protocol.JobUnit{}, err
	}
	requires := []string{}
	for _, name := range protocol.Toolchains {
		if toolchainOf[name](parts.Tools) != "" {
			requires = append(requires, name)
		}
	}
	directory := strings.TrimPrefix(parts.Package, protocol.AdamicModule+"/")
	id := strings.Trim(notIdCharacters.ReplaceAllString(strings.ToLower(directory), "-"), "-") + "-" + sha[:12]
	return protocol.JobUnit{
		Id: id, Test: test, Kind: parts.Kind, TimeoutSeconds: protocol.KindCeilings[parts.Kind], Requires: requires,
		Outputs: []protocol.Output{{Glob: "loom-out/test.jsonl.gz"}, {Glob: "loom-out/cpu.tsv"}},
	}, nil
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
	unit := Unit{Kind: parts.Kind, Package: parts.Package, Directory: directory, Run: parts.Select.Run, Skip: parts.Select.Skip,
		GateInputs: parts.GateInputs}
	keyed, err := KeyFor(tree, gateTools, unit, parts.Tools, compilers)
	if err != nil {
		return KeyParts{}, "", err
	}
	// The env part is already a key's (ADAMIC_GATE_CHANGED as a sha256), so it carries over as it is.
	keyed.Env = parts.Env
	key, err := UnitKey(keyed)
	return keyed, key, err
}
