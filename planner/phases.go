package planner

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GofmtPhase is the phase unit run.py doesn't hold: gofmt -l over the change's .go files, red on any output (Loom,
// Oct 10 01:42Z). Its key carries the change's paths, since what it checks is exactly them.
const GofmtPhase = "gofmt"

// PhaseUnits are a future's phase units (Loom, Oct 10 01:41Z): one per unit the box fast gate runs on this change, as
// run.py itself lists them at the gate tools (run.py --list-units, never retyped here), plus gofmt. A phase is keyed on
// the whole tree (its git tree object) and the gate tools commit, so it never reuses across trees, and it always runs
// tonight: its reads closure is 1.1. Its unit line is the key's select.run, from which the placer builds
// run.py --phase <first word> [--unit <rest>].
func PhaseUnits(tree, gateTools, base, sha string, tools Tools, inputs ParityInputs) ([]PlannedResult, error) {
	module, err := modulePath(tree)
	if err != nil {
		return nil, err
	}
	command := exec.Command("python3", filepath.Join(gateTools, "cloud/fast-gate/run.py"), "--list-units", "--tree", tree, "--base", base, "--sha", sha, "--tools", gateTools)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("run.py --list-units: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	lines := []string{}
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("run.py --list-units listed no phase units for %s", short(sha))
	}
	treeObject, err := gitOutput(tree, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return nil, err
	}
	toolsCommit, err := gitOutput(gateTools, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	closure, reads := phaseHash("loom-phase-tree", treeObject), phaseHash("loom-phase-tools", toolsCommit)
	environment := map[string]string{}
	for name, value := range GateEnvironment {
		environment[name] = value
	}
	gofmtEnvironment := environment
	if len(inputs.ChangedPaths) > 0 {
		directory, err := os.MkdirTemp("", "loom-plan-phase-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(directory)
		changed, err := ChangedPathsFile(directory, inputs.ChangedPaths)
		if err != nil {
			return nil, err
		}
		gofmtEnvironment = map[string]string{"ADAMIC_GATE_CHANGED": changed}
		for name, value := range environment {
			gofmtEnvironment[name] = value
		}
	}
	results := []PlannedResult{}
	for _, line := range append(lines, GofmtPhase) {
		env, err := KeyEnv(environment)
		if line == GofmtPhase {
			env, err = KeyEnv(gofmtEnvironment)
		}
		if err != nil {
			return nil, err
		}
		parts := KeyParts{Kind: "phase", Package: module, Select: Select{Run: line}, Closure: closure, Reads: reads,
			Products: []string{}, Tools: tools, Env: env, GateInputs: inputs.GateInputs}
		key, err := UnitKey(parts)
		if err != nil {
			return nil, err
		}
		results = append(results, PlannedResult{Name: "phase:" + line, UnitKey: key, KeyParts: parts, Decision: "run",
			Reason: "phase: uncached, keyed on the whole tree and the gate tools"})
	}
	return results, nil
}

func phaseHash(label, value string) string {
	sum := sha256.Sum256([]byte(label + "\n" + value))
	return hex.EncodeToString(sum[:])
}

func gitOutput(directory string, arguments ...string) (string, error) {
	output, err := exec.Command("git", append([]string{"-C", directory}, arguments...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w", strings.Join(arguments, " "), directory, err)
	}
	return strings.TrimSpace(string(output)), nil
}
