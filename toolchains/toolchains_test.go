package toolchains

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each mutant below must make a test here fail:
//
//	a probe that always passes: TestABrokenToolchainFailsItsProbe
//	a probe that always fails: TestAWorkingBoxPassesEveryProbe
//	the environment not sourced (probes on the caller's PATH): TestAWorkingBoxPassesEveryProbe
//	the caller's environment inherited (a box's own WASI_SYSROOT masking a broken claim): TestABrokenToolchainFailsItsProbe
//	a claim with no probe passed over: TestAClaimWithNoProbeOrNoToolchainFails

// stubs are a working box's tools: go builds its standard library, clang compiles a sanitized overflow into a program
// whose report names it and links for wasm32-wasi, node is v24 and runs whatever it is given.
var stubs = map[string]string{
	"go": `case $1 in version) echo "go version go1.27.1 linux/amd64" ;; list) exit 0 ;; *) exit 1 ;; esac`,
	"clang": `out=; while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; case $1 in -fsanitize=*) sanitize=1 ;; --target=wasm32-wasi) wasm=1 ;; esac; shift; done
if [ -n "${wasm:-}" ]; then printf 'wasm' > "$out"; exit 0; fi
printf '#!/bin/sh\necho "ERROR: AddressSanitizer: heap-buffer-overflow" >&2\nexit 1\n' > "$out"; chmod +x "$out"`,
	"node": `[ "$1" = --version ] && { echo v24.19.0; exit 0; }; exit 0`,
}

// box writes a box's adamic toolchain: env.sh, whose PATH leads to the stubs with replaced's in their place, and
// WASI_SYSROOT a directory unless sysroot is false. It returns env.sh's path.
func box(t *testing.T, replaced map[string]string, sysroot bool) string {
	t.Helper()
	home := t.TempDir()
	bin := filepath.Join(home, "adamic-tools", "bin")
	os.MkdirAll(bin, 0o755)
	for name, script := range stubs {
		if replacement, found := replaced[name]; found {
			script = replacement
		}
		os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+script+"\n"), 0o755)
	}
	environment := "export PATH='" + bin + ":/usr/bin:/bin'\n"
	if sysroot {
		os.MkdirAll(filepath.Join(home, "wasi-sysroot"), 0o755)
		environment += "export WASI_SYSROOT='" + filepath.Join(home, "wasi-sysroot") + "'\n"
	}
	os.WriteFile(filepath.Join(home, "adamic-tools", "env.sh"), []byte(environment), 0o644)
	if found := Environment(home); found != filepath.Join(home, "adamic-tools", "env.sh") {
		t.Fatalf("Environment(%s) is %q", home, found)
	}
	return Environment(home)
}

var everyToolchain = []string{"go", "clang", "node", "wasiSdk"}

// A box whose tools all work passes every probe, and the probes ran with its environment: none of the stubs is on the
// test's own PATH.
func TestAWorkingBoxPassesEveryProbe(t *testing.T) {
	if failures := Check(context.Background(), everyToolchain, box(t, nil, true)); len(failures) != 0 {
		t.Fatalf("a working box failed: %v", failures)
	}
}

// Each way a claim was false on the house, and the ways it could be: the probe names the toolchain and says why in
// the tool's own words, and the others still pass.
func TestABrokenToolchainFailsItsProbe(t *testing.T) {
	for _, test := range []struct {
		name      string
		replaced  map[string]string
		sysroot   bool
		toolchain string
		why       string
	}{
		{"wasm32 builtins missing (Oct 10, every box-strict box)", map[string]string{"clang": stubs["clang"] + "\n" + `exit 0`}, true, "wasiSdk", ""},
		{"no WASI_SYSROOT", nil, false, "wasiSdk", "WASI_SYSROOT isn't a directory"},
		{"node 22", map[string]string{"node": `[ "$1" = --version ] && { echo v22.11.0; exit 0; }; exit 0`}, true, "node", "node is v22.11.0, not 24"},
		{"node without node:wasi (the wasm probe runs under it, so wasiSdk fails too)", map[string]string{"node": `[ "$1" = --version ] && { echo v24.1.0; exit 0; }; exit 1`}, true, "node", "lacks node:wasi or registerHooks"},
		{"sanitizers that let an overflow run", map[string]string{"clang": `while [ $# -gt 0 ]; do [ "$1" = -o ] && out=$2; shift; done; printf '#!/bin/sh\nexit 0\n' > "$out"; chmod +x "$out"`}, true, "clang", "the sanitizers let an overflow run"},
		{"go without its standard library", map[string]string{"go": `case $1 in version) echo go1.27.1 ;; *) echo "package fmt is not in std" >&2; exit 1 ;; esac`}, true, "go", "package fmt is not in std"},
	} {
		t.Run(test.name, func(t *testing.T) {
			// The caller holds a WASI_SYSROOT of its own (a box's shell, Oct 10 on Cloud): only env.sh may reach a probe.
			t.Setenv("WASI_SYSROOT", t.TempDir())
			replaced := test.replaced
			if test.toolchain == "wasiSdk" && test.why == "" {
				// The link of Oct 10: clang compiles, and wasm-ld can't open the builtins.
				replaced = map[string]string{"clang": strings.Replace(stubs["clang"], `if [ -n "${wasm:-}" ]; then printf 'wasm' > "$out"; exit 0; fi`,
					`if [ -n "${wasm:-}" ]; then echo "wasm-ld: error: cannot open /tools/llvm/lib/clang/20/lib/wasm32-unknown-wasi/libclang_rt.builtins.a: No such file or directory" >&2; exit 1; fi`, 1)}
				test.why = "cannot open /tools/llvm/lib/clang/20/lib/wasm32-unknown-wasi/libclang_rt.builtins.a"
			}
			failures := Check(context.Background(), everyToolchain, box(t, replaced, test.sysroot))
			want := 1
			if strings.Contains(test.name, "wasiSdk fails too") {
				want = 2
			}
			if len(failures) != want || failures[0].Toolchain != test.toolchain || !strings.Contains(failures[0].Why, test.why) {
				t.Fatalf("failures %v, want only %s failing with %q", failures, test.toolchain, test.why)
			}
			if !strings.HasPrefix(failures[0].String(), "claims "+test.toolchain+", but ") {
				t.Errorf("a failure reads %q", failures[0].String())
			}
		})
	}
}

// A claim no probe checks fails rather than passing unchecked, and so does every claim on a box with no adamic
// toolchain to probe in.
func TestAClaimWithNoProbeOrNoToolchainFails(t *testing.T) {
	failures := Check(context.Background(), []string{"go", "rust"}, box(t, nil, true))
	if len(failures) != 1 || failures[0].Toolchain != "rust" || !strings.Contains(failures[0].Why, "no probe exists") {
		t.Fatalf("an unknown claim: %v", failures)
	}
	if Environment(t.TempDir()) != "" {
		t.Fatal("an empty home has an environment")
	}
	failures = Check(context.Background(), everyToolchain, "")
	if len(failures) != len(everyToolchain) || !strings.Contains(failures[0].Why, "no adamic toolchain") {
		t.Fatalf("no environment: %v", failures)
	}
}

// On a box (this home has an adamic toolchain), every toolchain probes for real; elsewhere it is skipped.
func TestTheRealToolchainsOnThisBox(t *testing.T) {
	home, _ := os.UserHomeDir()
	environment := Environment(home)
	if environment == "" {
		t.Skip("no adamic toolchain in this home")
	}
	for _, failure := range Check(context.Background(), everyToolchain, environment) {
		t.Errorf("%s", failure)
	}
}

// The toolchain is found where adamic's setup puts it: $ADAMIC_TOOLS/env.sh first when the environment names one (a
// Codex instance's, Oct 11), else home's, as before. Mutant: ADAMIC_TOOLS left unread. Not parallel: it sets the
// environment.
func TestTheToolchainIsWhereADAMICTOOLSSays(t *testing.T) {
	home, tools := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(home, "adamic-tools"), 0o755)
	os.WriteFile(filepath.Join(home, "adamic-tools", "env.sh"), []byte("\n"), 0o644)
	os.WriteFile(filepath.Join(tools, "env.sh"), []byte("\n"), 0o644)
	t.Setenv("ADAMIC_TOOLS", "")
	if found := Environment(home); found != filepath.Join(home, "adamic-tools", "env.sh") {
		t.Fatalf("with no ADAMIC_TOOLS: %q, want home's", found)
	}
	t.Setenv("ADAMIC_TOOLS", tools)
	if found := Environment(home); found != filepath.Join(tools, "env.sh") {
		t.Fatalf("with ADAMIC_TOOLS: %q, want its env.sh", found)
	}
	if found := Environment(t.TempDir()); found != filepath.Join(tools, "env.sh") {
		t.Fatalf("an empty home with ADAMIC_TOOLS: %q", found)
	}
}

// adamic's env.sh names a TMPDIR a fresh instance hasn't made yet (Oct 11, Codex: "clang: error: unable to make temporary
// file"); the probe makes it as prepare.sh does for a unit, so a working clang passes. Mutant: TMPDIR left unmade.
func TestAProbeMakesTheTMPDIREnvShNames(t *testing.T) {
	needsTemporary := "[ -d \"$TMPDIR\" ] || { echo 'clang: error: unable to make temporary file: No such file or directory' >&2; exit 1; }\n" + stubs["clang"]
	environment := box(t, map[string]string{"clang": needsTemporary}, true)
	gate := filepath.Join(filepath.Dir(environment), "gate-not-made-yet")
	content, _ := os.ReadFile(environment)
	os.WriteFile(environment, append(content, []byte("export TMPDIR='"+gate+"'\n")...), 0o644)
	if failures := Check(context.Background(), []string{"clang"}, environment); len(failures) != 0 {
		t.Fatalf("a working clang with a TMPDIR not made yet: %v", failures)
	}
	if info, err := os.Stat(gate); err != nil || !info.IsDir() {
		t.Fatalf("the probe didn't make TMPDIR: %v", err)
	}
}
