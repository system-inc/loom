package lander

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPushConfReadsEachSettingOverWorkshopsDefaults(t *testing.T) {
	home := "/home/ahra"
	if config, err := ReadConfig("", home); err != nil || config != DefaultConfig(home) || config.Branch != "main" ||
		config.Repository != "/home/ahra/loom-lander/adamic.git" || config.Secret != "/home/ahra/.loom/token-secret" {
		t.Fatalf("an empty push.conf read %+v, %v", config, err)
	}
	config, err := ReadConfig("# rehearsal\nbranch = loom-rehearsal\n  queue=http://127.0.0.1:8787  \nrepository = ~/lander.git\nstate = /tmp/state\nsecret = ~/dummy-secret\n", home)
	want := Config{Branch: "loom-rehearsal", Queue: "http://127.0.0.1:8787", Repository: "/home/ahra/lander.git", State: "/tmp/state", Secret: "/home/ahra/dummy-secret"}
	if err != nil || config != want {
		t.Fatalf("read %+v, %v", config, err)
	}
	for name, content := range map[string]string{
		"an option":             "branch = --force\n",
		"a forced refspec":      "branch = +main\n",
		"a revision":            "branch = main~1\n",
		"a full ref":            "branch = refs/heads/main:refs/heads/x\n",
		"dots":                  "branch = a..b\n",
		"a lock":                "branch = main.lock\n",
		"a trailing slash":      "branch = loom/\n",
		"a space":               "branch = loom rehearsal\n",
		"an empty branch":       "branch =\n",
		"a branch twice":        "branch = main\nbranch = loom-rehearsal\n",
		"a queue that's no URL": "queue = loom.system.inc\n",
		"a relative clone":      "repository = loom-lander/adamic.git\n",
		"a setting it lacks":    "ref = main\n",
		"a line with no equals": "branch = main\nloom-rehearsal\n",
	} {
		if config, err := ReadConfig(content, home); err == nil {
			t.Errorf("%s: read %+v", name, config)
		}
	}
}

// The units are the checked-in files: a oneshot pass of the release's loom, and a timer every 30 s that a reboot keeps
// once it is enabled.
func TestTheUnitsRunTheReleasesLoomEvery30Seconds(t *testing.T) {
	for _, line := range []string{"Type=oneshot", "ExecStart=%h/.loom/bin/loom push", "StandardOutput=append:%h/loom-lander/pusher.log", "TimeoutStartSec=600"} {
		if !strings.Contains(serviceText, "\n"+line+"\n") {
			t.Errorf("the service has no %q", line)
		}
	}
	if strings.Contains(serviceText, "python") || strings.Contains(serviceText, "[Install]") {
		t.Errorf("the service runs python, or installs itself:\n%s", serviceText)
	}
	for _, line := range []string{"OnUnitActiveSec=30s", "[Install]", "WantedBy=timers.target"} {
		if !strings.Contains(timerText, "\n"+line+"\n") {
			t.Errorf("the timer has no %q", line)
		}
	}
}

// A machine as the hook finds it, with a recorder in place of systemctl.
type machine struct {
	home  string
	paths Paths
	calls [][]string
	fail  bool
}

func newMachine(t *testing.T, config string) *machine {
	home := t.TempDir()
	made := &machine{home: home, paths: HomePaths(home)}
	if config != "" {
		os.MkdirAll(filepath.Dir(made.paths.Config), 0o755)
		os.WriteFile(made.paths.Config, []byte(config), 0o644)
	}
	return made
}

func (made *machine) install() error {
	made.calls = nil
	return Install(made.paths, made.home, func(arguments ...string) (string, error) {
		made.calls = append(made.calls, arguments)
		if made.fail {
			return "", errors.New("systemctl failed")
		}
		return "", nil
	}, io.Discard)
}

// Install writes the hook and the units, tells systemd only when a unit changed, and never enables, starts or stops
// anything: whether the lander runs is Kirk's call.
func TestInstallWritesTheUnitsAndNeverStartsThem(t *testing.T) {
	made := newMachine(t, "branch = main\n")
	if err := made.install(); err != nil || !reflect.DeepEqual(made.calls, [][]string{{"daemon-reload"}}) {
		t.Fatalf("first install: %q, %v", made.calls, err)
	}
	for name, text := range map[string]string{ServiceName: serviceText, TimerName: timerText} {
		if held, err := os.ReadFile(filepath.Join(made.paths.Units, name)); err != nil || string(held) != text {
			t.Fatalf("%s: %v\n%s", name, err, held)
		}
	}
	if hook, err := os.Stat(made.paths.Hook); err != nil || hook.Mode().Perm() != 0o755 {
		t.Fatalf("the hook: %v, %v", hook, err)
	}
	for range 2 {
		if err := made.install(); err != nil || len(made.calls) != 0 {
			t.Fatalf("again with nothing changed: %q, %v", made.calls, err)
		}
	}
	// A stale unit (the Python pusher's, as Workshop holds today) is replaced and systemd told; an enabled timer keeps running the release.
	os.WriteFile(filepath.Join(made.paths.Units, ServiceName), []byte("ExecStart=/usr/bin/python3 -B %h/loom-lander/bin/pusher.py\n"), 0o644)
	if err := made.install(); err != nil || !reflect.DeepEqual(made.calls, [][]string{{"daemon-reload"}}) {
		t.Fatalf("over a stale unit: %q, %v", made.calls, err)
	}
	os.Remove(filepath.Join(made.paths.Units, TimerName))
	made.fail = true
	if err := made.install(); err == nil {
		t.Fatal("a failed daemon-reload passed")
	}
}

// Without push.conf, or with one it can't read, this machine isn't the lander: nothing is written.
func TestInstallRefusesAMachineThatIsntTheLander(t *testing.T) {
	for name, made := range map[string]*machine{"no push.conf": newMachine(t, ""), "a bad push.conf": newMachine(t, "branch = +main\n")} {
		if err := made.install(); err == nil || len(made.calls) != 0 {
			t.Errorf("%s: installed (%v), calls %q", name, err, made.calls)
		}
		for _, path := range []string{filepath.Join(made.paths.Units, ServiceName), filepath.Join(made.paths.Units, TimerName), made.paths.Hook} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s: %s was written", name, path)
			}
		}
	}
}

// The hook runs `loom push install` only on the lander (push.conf there), and passes with a note on any other machine
// or on a release from before `loom push` (a rollback past it).
func TestTheHookInstallsOnlyOnTheLander(t *testing.T) {
	made := newMachine(t, "branch = main\n")
	if err := made.install(); err != nil {
		t.Fatal(err)
	}
	run := func(loom string) (int, string) {
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "loom"), []byte(loom), 0o755)
		command := exec.Command(made.paths.Hook)
		command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + made.home, "LOOM_UPDATE_BIN=" + bin}
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	current := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom push [--config <push.conf>]\\n  loom push install\\n' >&2; exit 3; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(current); code != 7 || !strings.Contains(output, "ran push install") {
		t.Fatalf("on the lander: exit %d, %s", code, output)
	}
	rollback := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom run\\n' >&2; exit 3; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(rollback); code != 0 || strings.Contains(output, "ran ") || !strings.Contains(output, "has no push install") {
		t.Fatalf("on a rollback: exit %d, %s", code, output)
	}
	os.Remove(made.paths.Config)
	if code, output := run(current); code != 0 || strings.Contains(output, "ran ") || !strings.Contains(output, "doesn't land") {
		t.Fatalf("without push.conf: exit %d, %s", code, output)
	}
}
