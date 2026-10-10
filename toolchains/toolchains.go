// Package toolchains checks the toolchains a box claims for its pool (protocol.Toolchains), so a claim is a fact on
// the box, never only a line in the placer's --pool-has. Oct 10 (#qm8bchp): box-strict claimed wasiSdk, but no box's
// clang could link for wasm32-wasi, so every wasi test skipped and the census held internal/native red. A serve that
// claims a toolchain probes it before it asks its pool for anything (runner.ServeOptions.Has).
//
// Each probe is the check the toolchain's users make themselves, run in bash with the box's adamic toolchain sourced
// (Environment), as prepare.sh sources it for every unit: go builds its standard library, clang's sanitizers catch an
// overflow (cloud/setup.sh's saneClang), node is Node 24 with node:wasi and registerHooks (the wasi tests' own check),
// and wasiSdk links the wasi tests' own probe for wasm32-wasi with the clang on PATH and runs it under node:wasi.
package toolchains

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Timeout bounds one probe: a clang link and a node start are seconds, so a probe still running at this is a failure.
const Timeout = 2 * time.Minute

// probes are the scripts, by toolchain, each run in a fresh directory ($1) after the environment is sourced. Exit 0
// is a working toolchain; anything else fails it, with the script's last lines as the reason.
var probes = map[string]string{
	"go": `go version && (cd / && go list fmt testing > /dev/null)`,
	"clang": `printf '#include <stdlib.h>\nint main(void) { int *values = malloc(4 * sizeof(int)); int read = values[4]; free(values); return read; }\n' > "$1/overflow.c"
clang -fsanitize=address,undefined -g "$1/overflow.c" -o "$1/overflow" || exit 1
if "$1/overflow" > /dev/null 2> "$1/report"; then echo "the sanitizers let an overflow run"; exit 1; fi
grep -q heap-buffer-overflow "$1/report" || { echo "the sanitizers didn't name the overflow"; tail -3 "$1/report"; exit 1; }`,
	"node": `version=$(node --version) || exit 1
case $version in v24.*) ;; *) echo "node is $version, not 24"; exit 1 ;; esac
node --disable-warning=ExperimentalWarning --input-type=module -e "import {WASI} from 'node:wasi'; import {registerHooks} from 'node:module'; new WASI({version:'preview1'}); if (!registerHooks) process.exit(1)" ||
	{ echo "node $version lacks node:wasi or registerHooks"; exit 1; }`,
	"wasiSdk": `[ -n "${WASI_SYSROOT:-}" ] && [ -d "$WASI_SYSROOT" ] || { echo "WASI_SYSROOT isn't a directory: '${WASI_SYSROOT:-}'"; exit 1; }
printf '#include <stdlib.h>\nint main(void) { void *p = malloc(16); free(p); return 0; }\n' > "$1/probe.c"
clang --target=wasm32-wasi --sysroot="$WASI_SYSROOT" -DADAMIC_TARGET_WASI=1 -std=c11 -Wall -Wextra -Werror -pedantic -O2 -ffp-contract=off -fno-optimize-sibling-calls "$1/probe.c" -o "$1/probe.wasm" || exit 1
node --disable-warning=ExperimentalWarning --input-type=module -e "import {WASI} from 'node:wasi'; import {readFileSync} from 'node:fs'; const wasi = new WASI({version:'preview1', returnOnExit:true}); const module = await WebAssembly.instantiate(readFileSync(process.argv[1]), wasi.getImportObject()); process.exit(wasi.start(module.instance))" "$1/probe.wasm" ||
	{ echo "the linked probe didn't run under node:wasi"; exit 1; }`,
}

// A Failure is one claimed toolchain that doesn't work here, and why, in the probe's own words.
type Failure struct {
	Toolchain string
	Why       string
}

func (failure Failure) String() string {
	return fmt.Sprintf("claims %s, but %s", failure.Toolchain, failure.Why)
}

// Environment is the adamic toolchain a box's units run with: home's adamic-tools/env.sh, else .adamic-tools/env.sh,
// as prepare.sh looks; empty when there is neither.
func Environment(home string) string {
	for _, name := range []string{"adamic-tools", ".adamic-tools"} {
		path := filepath.Join(home, name, "env.sh")
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

// Check probes every claimed toolchain with environment sourced and returns the ones that fail, in the order claimed:
// none means every claim holds. A claim with no probe, or no environment to probe it in, fails as such.
func Check(checkContext context.Context, claims []string, environment string) []Failure {
	failures := []Failure{}
	for _, toolchain := range claims {
		if why := probe(checkContext, toolchain, environment); why != "" {
			failures = append(failures, Failure{Toolchain: toolchain, Why: why})
		}
	}
	return failures
}

// Parse reads a claim list, go,clang,node,wasiSdk: each a toolchain a probe exists for, none twice. Empty claims
// nothing.
func Parse(list string) ([]string, error) {
	claims := []string{}
	if strings.TrimSpace(list) == "" {
		return claims, nil
	}
	seen := map[string]bool{}
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		if _, known := probes[name]; !known {
			return nil, fmt.Errorf("%q isn't a toolchain a probe exists for (%s)", name, strings.Join(Known(), ", "))
		}
		if seen[name] {
			return nil, fmt.Errorf("%q is claimed twice", name)
		}
		seen[name] = true
		claims = append(claims, name)
	}
	return claims, nil
}

// Known is every toolchain a probe exists for, sorted.
func Known() []string {
	names := make([]string, 0, len(probes))
	for name := range probes {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// probe runs one toolchain's script and says why it failed, or is empty when the toolchain works.
func probe(checkContext context.Context, toolchain, environment string) string {
	script, known := probes[toolchain]
	if !known {
		return fmt.Sprintf("no probe exists for %q (known: %s)", toolchain, strings.Join(Known(), ", "))
	}
	if environment == "" {
		return "the box has no adamic toolchain (adamic-tools/env.sh)"
	}
	directory, err := os.MkdirTemp("", "loom-toolchain-"+toolchain+"-")
	if err != nil {
		return fmt.Sprintf("making a directory to probe in: %v", err)
	}
	defer os.RemoveAll(directory)
	probeContext, cancel := context.WithTimeout(checkContext, Timeout)
	defer cancel()
	command := exec.CommandContext(probeContext, "bash", "-c", `source "$2" > /dev/null 2>&1 || { echo "sourcing $2 failed"; exit 1; }
`+script, "probe", directory, environment)
	command.Dir = directory
	// Nothing of the caller's environment reaches the probe but its home and a base PATH: what the toolchain needs must
	// come from env.sh, as it does for a unit, so a variable the caller happens to hold (a box's own WASI_SYSROOT) can never
	// make a broken claim pass.
	home, _ := os.UserHomeDir()
	command.Env = []string{"HOME=" + home, "PATH=/usr/local/bin:/usr/bin:/bin"}
	output, err := command.CombinedOutput()
	if err == nil {
		return ""
	}
	if probeContext.Err() != nil {
		return fmt.Sprintf("its probe ran past %v", Timeout)
	}
	return lastLines(string(output), 3)
}

// lastLines is text's last count non-empty lines, joined with "; ", or "its probe failed" when it printed nothing.
func lastLines(text string, count int) string {
	lines := []string{}
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return "its probe failed"
	}
	return strings.Join(lines[max(0, len(lines)-count):], "; ")
}
