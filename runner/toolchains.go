package runner

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/toolchains"
)

// ToolchainsTrusted is how long a toolchain's passing probe stands for this runner's later units: a probe links and runs
// a program, seconds a unit shouldn't each spend, while a toolchain that breaks under a serving runner is caught within
// it. A failure is never remembered, so a toolchain fixed is trusted at its next unit.
var ToolchainsTrusted = 10 * time.Minute

// toolchainChecks remembers when each toolchain last passed its probe in an environment, across the units a runner
// runs, at once or one after another.
type toolchainChecks struct {
	mutex  sync.Mutex
	passed map[string]time.Time // environment + " " + toolchain
	check  func(checkContext context.Context, claims []string, environment string) []toolchains.Failure
	now    func() time.Time
}

// sharedToolchainChecks is every unit's in this process, so serve's units share one probe of each toolchain.
var sharedToolchainChecks = &toolchainChecks{passed: map[string]time.Time{}, check: toolchains.Check, now: time.Now}

// failures probes each named toolchain that hasn't passed within ToolchainsTrusted, with environment sourced, and
// returns the ones that fail. It holds the lock while it probes, so units that start together wait for one probe.
func (checks *toolchainChecks) failures(checkContext context.Context, names []string, environment string) []toolchains.Failure {
	checks.mutex.Lock()
	defer checks.mutex.Unlock()
	unchecked := []string{}
	for _, name := range names {
		if passed, found := checks.passed[environment+" "+name]; !found || checks.now().Sub(passed) >= ToolchainsTrusted {
			unchecked = append(unchecked, name)
		}
	}
	if len(unchecked) == 0 {
		return nil
	}
	failures := checks.check(checkContext, unchecked, environment)
	for _, name := range unchecked {
		failed := slices.ContainsFunc(failures, func(failure toolchains.Failure) bool { return failure.Toolchain == name })
		if failed {
			delete(checks.passed, environment+" "+name)
		} else {
			checks.passed[environment+" "+name] = checks.now()
		}
	}
	return failures
}

// requiredToolchains is what the unit's machine must have working before it runs: each toolchain its key names, and the
// WASI SDK for a WASI spec, which runs on it whatever the key says.
func requiredToolchains(unit protocol.Unit) []string {
	required := append([]string{}, unit.Requires...)
	if unit.Test != nil && !slices.Contains(required, "wasiSdk") {
		for _, testPackage := range unit.Test.Packages {
			if wasiPattern.MatchString(testPackage.Run) {
				required = append(required, "wasiSdk")
				break
			}
		}
	}
	return required
}

// checkToolchains probes the toolchains the unit requires, in the box's adamic toolchain as prepare.sh sources it
// (toolchains.Environment), before anything of the unit runs. One that's missing or broken is the machine's, never the
// change's: the unit says which and why, finishes broken without running, and its finished event names them, so the
// judge voids it (#vv28ewd). Tests that ran without their toolchain would skip into passes that prove nothing.
func (run *unitRun) checkToolchains(checkContext context.Context) bool {
	required := requiredToolchains(run.unit)
	if len(required) == 0 {
		return true
	}
	home, _ := os.UserHomeDir()
	var failures []toolchains.Failure
	if run.options.Probe != nil {
		failures = run.options.Probe(checkContext, required, toolchains.Environment(home))
	} else {
		checks := run.options.toolchainChecks
		if checks == nil {
			checks = sharedToolchainChecks
		}
		failures = checks.failures(checkContext, required, toolchains.Environment(home))
	}
	if len(failures) == 0 {
		return true
	}
	whys := []string{}
	for _, failure := range failures {
		run.missingTools = append(run.missingTools, failure.Toolchain)
		whys = append(whys, failure.Toolchain+": "+failure.Why)
	}
	run.fail(protocol.PhaseStart, fmt.Errorf("refused as unfit: the unit requires %s, which this machine lacks (%s): Loom's, never the change's",
		strings.Join(run.missingTools, ", "), strings.Join(whys, "; ")))
	return false
}
