package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/system-inc/loom/protocol"
)

// A house box shares its machine with other work (Workshop's tree builder, Kirk's own), so a strict unit there clears
// only its own root: another job's adamic runtime build in flight in the shared HOME stays where it is. Only an
// exclusive runner, a Codex instance whose machine is its alone, clears HOME's caches. The unit is a prebuilt one, whose
// opening runs the real trim.
func TestASharedUnitLeavesAnotherJobsBuildInPlace(t *testing.T) {
	real := prepareScript
	fixture := newPrebuiltFixture(t)
	script := filepath.Join(fixture.directory, "real-prepare.sh")
	os.WriteFile(script, real, 0o700)
	prepareScript = []byte(strings.Replace(string(prepareScript), "trim-only) exit 0 ;;",
		`trim-only) PATH=/usr/bin:/bin exec bash "`+script+`" "$@" ;;`, 1))
	inflight := filepath.Join(fixture.directory, "home", ".cache", "adamic", "runtime", ".build-otherjob", "inflight.o")
	leaving := filepath.Join(fixture.directory, "root", "adamic-stage3-lane-1", "x")
	for _, exclusive := range []bool{false, true} {
		for _, path := range []string{inflight, leaving} {
			os.MkdirAll(filepath.Dir(path), 0o755)
			os.WriteFile(path, []byte("x"), 0o644)
		}
		options := fixture.options(t)
		options.Exclusive = exclusive
		result, events, _ := runUnit(t, fixture.unit("^TestA$"), options)
		if result.Status != protocol.StatusPassed {
			t.Fatalf("exclusive %v: %s, errors %q", exclusive, result.Status, errorPhases(events))
		}
		if _, err := os.Stat(leaving); err == nil {
			t.Errorf("exclusive %v: an earlier unit's leaving on the root stayed", exclusive)
		}
		if _, err := os.Stat(inflight); (err == nil) == exclusive {
			t.Errorf("exclusive %v: another job's build in HOME: %v", exclusive, err)
		}
	}
}

// adamic's cloud/setup.sh installs into HOME, so only an exclusive runner runs it. On a shared machine the machine's own
// toolchain serves, and a machine without one is unfit for a checkout unit, named, with nothing run in its HOME. git and
// go are stand-ins: the checkout steps all succeed, and the clone's setup.sh marks HOME when it runs.
func TestOnlyAnExclusiveRunnerRunsSetupInHome(t *testing.T) {
	sha := strings.Repeat("e", 40)
	prepare := func(owner string, toolchain bool) (int, string, bool) {
		directory := t.TempDir()
		bin, home, root := filepath.Join(directory, "bin"), filepath.Join(directory, "home"), filepath.Join(directory, "root")
		for _, path := range []string{bin, home, root} {
			os.MkdirAll(path, 0o755)
		}
		if toolchain {
			os.MkdirAll(filepath.Join(home, "adamic-tools"), 0o755)
			os.WriteFile(filepath.Join(home, "adamic-tools", "env.sh"), []byte(":\n"), 0o644)
		}
		os.WriteFile(filepath.Join(bin, "git"), []byte(`#!/bin/bash
for last; do :; done
case " $* " in
*" clone "*)
	mkdir -p "${last}/.git" "${last}/cloud"
	printf '#!/bin/bash\ntouch "${HOME}/setup-ran"\nmkdir -p "${HOME}/adamic-tools" && echo : > "${HOME}/adamic-tools/env.sh"\n' > "${last}/cloud/setup.sh" ;;
*" rev-parse HEAD "*) echo `+sha+` ;;
*" config "*) exit 1 ;;
esac
exit 0
`), 0o755)
		os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/bash\nexit 0\n"), 0o755)
		script := filepath.Join(directory, "prepare.sh")
		os.WriteFile(script, prepareScript, 0o700)
		command := exec.Command("bash", script, filepath.Join(root, "adamic"), sha, "", "", filepath.Join(directory, "environment"), "trim", root, owner)
		command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "HOME=" + home, "TMPDIR=" + filepath.Join(directory, "tmp"), "LOOM_PREPARE_ATTEMPTS=1"}
		output, _ := command.CombinedOutput()
		_, err := os.Stat(filepath.Join(home, "setup-ran"))
		return command.ProcessState.ExitCode(), string(output), err == nil
	}
	if code, output, ran := prepare("exclusive", false); code != 0 || !ran {
		t.Fatalf("exclusive: exit %d, setup ran %v: %s", code, ran, output)
	}
	if code, output, ran := prepare("shared", false); code != 2 || ran || !strings.Contains(output, "unfit: this machine isn't the runner's alone") {
		t.Fatalf("shared, no toolchain: exit %d, setup ran %v: %s", code, ran, output)
	}
	if code, output, ran := prepare("shared", true); code != 0 || ran {
		t.Fatalf("shared, its own toolchain: exit %d, setup ran %v: %s", code, ran, output)
	}
}
