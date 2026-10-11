package daemons

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Each mutant below must make a test here fail:
//
//	a unit written only when missing (a hand-installed one never replaced): TestAHandInstalledUnitIsReplaced
//	daemon-reload on every run: TestAnInstallThatChangesNothingTellsSystemdNothing
//	the token gate dropped: TestOnlyTheDaemonsThisMachineHoldsTokensForAreInstalled
//	an enable or a start after the write: TestInstallNeverEnablesStartsOrTouchesThePushersTimer

// machine is a home with tokens for the named daemons, and a recording systemctl.
type machine struct {
	paths Paths
	calls [][]string
}

func newMachine(t *testing.T, tokens ...string) *machine {
	t.Helper()
	home := t.TempDir()
	served := &machine{paths: HomePaths(home)}
	os.MkdirAll(served.paths.Loom, 0o755)
	for _, token := range tokens {
		os.WriteFile(filepath.Join(served.paths.Loom, token), []byte("token\n"), 0o600)
	}
	return served
}

func (served *machine) install(t *testing.T) string {
	t.Helper()
	var report strings.Builder
	err := Install(served.paths, func(arguments ...string) (string, error) {
		served.calls = append(served.calls, arguments)
		return "", nil
	}, &report)
	if err != nil {
		t.Fatal(err)
	}
	return report.String()
}

func (served *machine) unit(t *testing.T, name string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(served.paths.Units, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(content)
}

var everyToken = []string{"planner-token", "placer-token", "build-trees-token", "judge-token"}

// Workshop's state on Oct 10: a unit installed by hand with another ExecStart and KillMode (the tree builder's), one
// the release has and the machine doesn't (the judge's was only by hand): the release's units replace both, and systemd
// is told once.
func TestAHandInstalledUnitIsReplaced(t *testing.T) {
	served := newMachine(t, everyToken...)
	os.MkdirAll(served.paths.Units, 0o755)
	byHand := filepath.Join(served.paths.Units, "loom-build-trees.service")
	os.WriteFile(byHand, []byte("[Service]\nKillMode=control-group\nExecStart=%h/.loom/bin/loom build-trees --old\n"), 0o644)
	report := served.install(t)
	for _, daemon := range Daemons {
		if served.unit(t, daemon.Unit) != daemon.Text {
			t.Errorf("%s isn't the release's", daemon.Unit)
		}
	}
	if !strings.Contains(served.unit(t, "loom-build-trees.service"), "KillMode=process") {
		t.Fatal("the tree builder's unit isn't the release's KillMode=process")
	}
	if len(served.calls) != 1 || strings.Join(served.calls[0], " ") != "daemon-reload" {
		t.Fatalf("systemctl calls %v, want one daemon-reload", served.calls)
	}
	if !strings.Contains(report, "wrote loom-plan.service, loom-place.service, loom-build-trees.service, loom-judge.service") {
		t.Fatalf("report %q", report)
	}
}

// The updater runs the hook again until it passes, and every release: an install that finds every unit the release's
// writes nothing and tells systemd nothing.
func TestAnInstallThatChangesNothingTellsSystemdNothing(t *testing.T) {
	served := newMachine(t, everyToken...)
	served.install(t)
	served.calls = nil
	report := served.install(t)
	if len(served.calls) != 0 || !strings.Contains(report, "already this release's") || strings.Contains(report, "wrote") {
		t.Fatalf("a second install called %v, reporting %q", served.calls, report)
	}
}

// A machine runs the daemons whose tokens it holds, and only their units are installed: a box serving a pool, with
// none, gets none, and says why.
func TestOnlyTheDaemonsThisMachineHoldsTokensForAreInstalled(t *testing.T) {
	served := newMachine(t, "placer-token")
	report := served.install(t)
	if served.unit(t, "loom-place.service") == "" {
		t.Fatal("the placer's unit is empty")
	}
	for _, name := range []string{"loom-plan.service", "loom-build-trees.service", "loom-judge.service"} {
		if _, err := os.Stat(filepath.Join(served.paths.Units, name)); err == nil {
			t.Errorf("%s installed with no token for it", name)
		}
	}
	if !strings.Contains(report, "no token here for loom-plan.service, loom-build-trees.service, loom-judge.service") {
		t.Fatalf("report %q", report)
	}
	box := newMachine(t)
	box.install(t)
	if entries, _ := os.ReadDir(box.paths.Units); len(entries) != 0 || len(box.calls) != 0 {
		t.Fatalf("a machine with no tokens got %v, calls %v", entries, box.calls)
	}
}

// Install writes and reloads, nothing else: no unit is enabled, started, stopped or restarted, and the pusher's timer,
// switched on by hand with Kirk there (docs/cutover.md), is left exactly as found, enabled or not, with its unit.
func TestInstallNeverEnablesStartsOrTouchesThePushersTimer(t *testing.T) {
	served := newMachine(t, everyToken...)
	wants := filepath.Join(served.paths.Units, "timers.target.wants")
	os.MkdirAll(wants, 0o755)
	timer := filepath.Join(served.paths.Units, "loom-pusher.timer")
	os.WriteFile(timer, []byte("[Timer]\nOnUnitActiveSec=60\n"), 0o644)
	os.Symlink(timer, filepath.Join(wants, "loom-pusher.timer"))
	before, _ := os.ReadFile(timer)
	served.install(t)
	for _, call := range served.calls {
		if call[0] != "daemon-reload" {
			t.Errorf("systemctl %v: install only ever reloads", call)
		}
	}
	after, err := os.ReadFile(timer)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the pusher's timer changed: %q, %v", after, err)
	}
	if link, err := os.Readlink(filepath.Join(wants, "loom-pusher.timer")); err != nil || link != timer {
		t.Fatalf("the pusher's timer's enablement changed: %q, %v", link, err)
	}
	for _, daemon := range Daemons {
		if strings.Contains(daemon.Unit, "pusher") {
			t.Fatalf("install-units installs %s, the lander's", daemon.Unit)
		}
	}
}

// The hook runs install-units, and a release from before it (a rollback past it) passes, units left as they are.
func TestTheHookRunsInstallUnitsOrPassesOnARollback(t *testing.T) {
	served := newMachine(t)
	served.install(t)
	hook, err := os.Stat(served.paths.Hook)
	if err != nil || hook.Mode().Perm() != 0o755 {
		t.Fatalf("the hook: %v, %v", hook, err)
	}
	run := func(loom string) (int, string) {
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "loom"), []byte(loom), 0o755)
		command := exec.Command(served.paths.Hook)
		command.Env = []string{"PATH=/usr/bin:/bin", "LOOM_UPDATE_BIN=" + bin}
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	current := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom install-units\\n  loom version\\n' >&2; exit 2; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(current); code != 7 || !strings.Contains(output, "ran install-units") {
		t.Fatalf("the hook: exit %d, %q", code, output)
	}
	old := "#!/bin/sh\nprintf 'usage:\\n  loom version\\n' >&2; exit 2\n"
	if code, output := run(old); code != 0 || !strings.Contains(output, "has no install-units") {
		t.Fatalf("a rollback: exit %d, %q", code, output)
	}
}

// The judge's unit is the one Workshop ran by its last drop-in (zz-live.conf): live batches with --tree-git and
// --require-runner, none of the dry flags, and its only command in the unit itself. Mutants: the dry flags back, or
// --tree-git dropped.
func TestTheJudgesUnitIsTheOneWorkshopRan(t *testing.T) {
	execStart := ""
	for _, line := range strings.Split(Daemons[3].Text, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			if execStart != "" {
				t.Fatal("the judge's unit has two ExecStart lines")
			}
			execStart = line
		}
	}
	for _, flag := range []string{" --tree-git %h/loom-trees/adamic", " --require-runner", " --interval 10s"} {
		if !strings.Contains(execStart, flag) {
			t.Errorf("the judge's command lacks%s", flag)
		}
	}
	for _, flag := range []string{"--dry-run", "--post-parity"} {
		if strings.Contains(execStart, flag) {
			t.Errorf("the judge's command has %s", flag)
		}
	}
}
