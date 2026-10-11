package runner

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/toolchains"
)

// stubChecks fails each toolchain in broken and records every probe it was asked for, at a clock the test moves.
func stubChecks(broken ...string) (*toolchainChecks, *[][]string, *time.Time) {
	asked, now := &[][]string{}, &time.Time{}
	*now = time.Unix(1791680000, 0)
	checks := &toolchainChecks{passed: map[string]time.Time{}, now: func() time.Time { return *now }}
	checks.check = func(_ context.Context, claims []string, _ string) []toolchains.Failure {
		*asked = append(*asked, append([]string{}, claims...))
		failures := []toolchains.Failure{}
		for _, claim := range claims {
			if slices.Contains(broken, claim) {
				failures = append(failures, toolchains.Failure{Toolchain: claim, Why: "wasm-ld: cannot open libclang_rt.builtins.a"})
			}
		}
		return failures
	}
	return checks, asked, now
}

// A unit whose required toolchain is broken here never runs: it finishes broken, says which and why, and its finished
// event names it, so the judge reads it void, never red (#vv28ewd). Mutants: the check skipped (the command runs);
// missingTools left off finished.
func TestAUnitWithoutAToolchainItRequiresFinishesBrokenNamingIt(t *testing.T) {
	options := testOptions(t)
	checks, _, _ := stubChecks("wasiSdk")
	options.toolchainChecks = checks
	marker := filepath.Join(t.TempDir(), "ran")
	unit := testUnit("touch", marker)
	unit.Requires = []string{"go", "wasiSdk"}
	result, events, _ := runUnit(t, unit, options)
	if result.Status != protocol.StatusBroken {
		t.Fatalf("finished %s, want broken", result.Status)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the unit ran without the toolchain it requires")
	}
	last := events[len(events)-1]
	if strings.Join(last.MissingTools, ",") != "wasiSdk" {
		t.Fatalf("finished names missing tools %q, want wasiSdk", last.MissingTools)
	}
	if !strings.Contains(errorPhases(events), "wasm-ld") {
		t.Errorf("the unit doesn't say why: %q", errorPhases(events))
	}
	finished, _ := judge.FinishedFromEvents(events)
	decision, err := judge.Decide(judge.Evidence{First: finished.Attempt, FirstInfra: finished.Infra, MissingTools: finished.MissingTools})
	if err != nil || decision.Status != judge.Void {
		t.Fatalf("the judge decides %+v (%v), want void", decision, err)
	}
}

// A toolchain that passed is trusted for ToolchainsTrusted, so units that follow don't each probe it; past that it's
// probed again, and a failure is never remembered. A unit that requires nothing probes nothing. Mutants: every pass
// remembered forever; a failure remembered as a pass.
func TestAPassingToolchainIsTrustedAWhileAndAFailureNever(t *testing.T) {
	checks, asked, now := stubChecks("node")
	environment := "/home/box/adamic-tools/env.sh"
	if failures := checks.failures(context.Background(), []string{"go", "node"}, environment); len(failures) != 1 || failures[0].Toolchain != "node" {
		t.Fatalf("failures %+v, want node's", failures)
	}
	*now = now.Add(ToolchainsTrusted / 2)
	checks.failures(context.Background(), []string{"go", "node"}, environment)
	if got := (*asked)[1]; strings.Join(got, ",") != "node" {
		t.Fatalf("within the window it probed %q, want only the failed node", got)
	}
	*now = now.Add(ToolchainsTrusted)
	checks.failures(context.Background(), []string{"go"}, environment)
	if got := (*asked)[2]; strings.Join(got, ",") != "go" {
		t.Fatalf("past the window it probed %q, want go again", got)
	}
	checks.failures(context.Background(), []string{"go"}, "/home/other/.adamic-tools/env.sh")
	if len(*asked) != 4 {
		t.Fatal("a pass in one environment was trusted in another")
	}
	options := testOptions(t)
	options.toolchainChecks = checks
	before := len(*asked)
	if result, _, _ := runUnit(t, testUnit("true"), options); result.Status != protocol.StatusPassed || len(*asked) != before {
		t.Fatalf("a unit requiring nothing finished %s after %d probes", result.Status, len(*asked)-before)
	}
}

// A WASI spec requires the WASI SDK whatever its key names, and a unit names each toolchain once.
func TestAWASISpecRequiresTheWASISDK(t *testing.T) {
	unit := protocol.Unit{Requires: []string{"go"}, Test: &protocol.TestJob{Packages: []protocol.TestPackage{{Package: "p", Run: "^(TestWASIUnit07)$"}}}}
	if got := strings.Join(requiredToolchains(unit), ","); got != "go,wasiSdk" {
		t.Fatalf("a WASI spec requires %q", got)
	}
	unit.Requires = []string{"wasiSdk"}
	if got := strings.Join(requiredToolchains(unit), ","); got != "wasiSdk" {
		t.Fatalf("a WASI spec whose key names the SDK requires %q", got)
	}
	unit.Test.Packages[0].Run = ""
	unit.Requires = nil
	if got := requiredToolchains(unit); len(got) != 0 {
		t.Fatalf("a whole package whose key names nothing requires %q", got)
	}
}

// A unit's preparation sees where the machine's environment says adamic's toolchain is (ADAMIC_TOOLS, a Codex
// instance's), as the probe does. Mutant: ADAMIC_TOOLS left out of the unit's environment. Not parallel: it sets it.
func TestPreparationSeesADAMICTOOLS(t *testing.T) {
	t.Setenv("ADAMIC_TOOLS", "/workspace/adamic-tools")
	run := &unitRun{unit: protocol.Unit{}}
	if !slices.Contains(run.prepareEnvironment(), "ADAMIC_TOOLS=/workspace/adamic-tools") {
		t.Fatalf("preparation's environment lacks ADAMIC_TOOLS: %q", run.prepareEnvironment())
	}
}
