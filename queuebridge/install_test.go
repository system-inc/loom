package queuebridge

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

// Each mutant below must make a test here fail:
//
//	decides read as anything but yes or no: TestQueueBridgeConfReadsEachSettingOverTheMacsDefaults
//	install enabling, starting or loading anything: TestInstallWritesTheUnitsAndNeverStartsThem
//	the agent written with ~ or HOME_DIRECTORY left in: TestInstallWritesTheUnitsAndNeverStartsThem
//	install without queue-bridge.conf: TestInstallRefusesAMachineThatIsntTheBridge
//	the hook installing on a machine without queue-bridge.conf: TestTheHookInstallsOnlyOnTheBridge

func TestQueueBridgeConfReadsEachSettingOverTheMacsDefaults(t *testing.T) {
	home := "/Users/k"
	if config, err := ReadConfig("", home); err != nil || config != DefaultConfig(home) || !config.Decides ||
		config.Repository != "/Users/k/Projects/system/adamic" || config.PushMain != "/Users/k/.adamic-merge-tree/cloud/integration/push-main.sh" {
		t.Fatalf("an empty queue-bridge.conf read %+v, %v", config, err)
	}
	config, err := ReadConfig("# slice 2\ndecides = no\n  queue=http://127.0.0.1:8787  \nrepository = ~/adamic\nstate = /tmp/state\nsecret = ~/s\npush-main = ~/p.sh\nrequeue = /r.sh\n", home)
	want := Config{Queue: "http://127.0.0.1:8787", Repository: "/Users/k/adamic", State: "/tmp/state", Secret: "/Users/k/s", PushMain: "/Users/k/p.sh", Requeue: "/r.sh", Decides: false}
	if err != nil || config != want {
		t.Fatalf("read %+v, %v", config, err)
	}
	for name, content := range map[string]string{
		"decides as 0":          "decides = 0\n",
		"decides as false":      "decides = false\n",
		"decides twice":         "decides = no\ndecides = yes\n",
		"an empty value":        "decides =\n",
		"a queue that's no URL": "queue = loom.system.inc\n",
		"a relative clone":      "repository = adamic\n",
		"a setting it lacks":    "lands = yes\n",
		"a line with no equals": "decides = no\nyes\n",
	} {
		if config, err := ReadConfig(content, home); err == nil {
			t.Errorf("%s: read %+v", name, config)
		}
	}
}

// The units are the checked-in files: a oneshot pass of the release's loom, and a timer every 60 s that a reboot keeps
// once it is enabled; the agent runs the same pass every 60 s.
func TestTheUnitsRunTheReleasesLoomEvery60Seconds(t *testing.T) {
	for _, line := range []string{"Type=oneshot", "ExecStart=%h/.loom/bin/loom queue-bridge", "StandardOutput=append:%h/.loom/queue-bridge.log"} {
		if !strings.Contains(serviceText, "\n"+line+"\n") {
			t.Errorf("the service has no %q", line)
		}
	}
	for _, text := range []string{serviceText, agentText} {
		if strings.Contains(text, "python") || strings.Contains(text, "[Install]") {
			t.Errorf("a unit runs python, or installs itself:\n%s", text)
		}
	}
	for _, line := range []string{"OnUnitActiveSec=60s", "[Install]", "WantedBy=timers.target"} {
		if !strings.Contains(timerText, "\n"+line+"\n") {
			t.Errorf("the timer has no %q", line)
		}
	}
	for _, part := range []string{"<string>HOME_DIRECTORY/.loom/bin/loom</string><string>queue-bridge</string>", "<key>StartInterval</key><integer>60</integer>"} {
		if !strings.Contains(agentText, part) {
			t.Errorf("the agent has no %q", part)
		}
	}
}

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

func (made *machine) install(goos string) error {
	made.calls = nil
	return Install(made.paths, made.home, goos, func(arguments ...string) (string, error) {
		made.calls = append(made.calls, arguments)
		if made.fail {
			return "", errors.New("systemctl failed")
		}
		return "", nil
	}, io.Discard)
}

// Install writes the hook and the units, tells systemd only when a unit changed, and never enables, starts, loads or
// stops anything: whether the bridge runs is Kirk's call.
func TestInstallWritesTheUnitsAndNeverStartsThem(t *testing.T) {
	made := newMachine(t, "decides = no\n")
	if err := made.install("linux"); err != nil || !reflect.DeepEqual(made.calls, [][]string{{"daemon-reload"}}) {
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
	if err := made.install("linux"); err != nil || len(made.calls) != 0 {
		t.Fatalf("again with nothing changed: %q, %v", made.calls, err)
	}
	os.WriteFile(filepath.Join(made.paths.Units, ServiceName), []byte("ExecStart=/usr/bin/python3 queue_bridge.py\n"), 0o644)
	made.fail = true
	if err := made.install("linux"); err == nil {
		t.Fatal("a failed daemon-reload passed")
	}
	// On macOS: the agent with the home written in, launchd never told.
	made = newMachine(t, "decides = yes\n")
	if err := made.install("darwin"); err != nil || len(made.calls) != 0 {
		t.Fatalf("on macOS: %q, %v", made.calls, err)
	}
	held, err := os.ReadFile(filepath.Join(made.paths.Agents, AgentName))
	if err != nil || strings.Contains(string(held), "HOME_DIRECTORY") || !strings.Contains(string(held), "<string>"+made.home+"/.loom/bin/loom</string>") {
		t.Fatalf("the agent: %v\n%s", err, held)
	}
	if _, err := os.Stat(filepath.Join(made.paths.Units, ServiceName)); !os.IsNotExist(err) {
		t.Fatalf("a systemd unit on macOS: %v", err)
	}
	if err := made.install("windows"); err == nil {
		t.Fatal("installed under neither systemd nor launchd")
	}
}

// Without queue-bridge.conf, or with one it can't read, this machine isn't the bridge: nothing is written.
func TestInstallRefusesAMachineThatIsntTheBridge(t *testing.T) {
	for name, made := range map[string]*machine{"no queue-bridge.conf": newMachine(t, ""), "a bad queue-bridge.conf": newMachine(t, "decides = 0\n")} {
		for _, goos := range []string{"linux", "darwin"} {
			if err := made.install(goos); err == nil || len(made.calls) != 0 {
				t.Errorf("%s on %s: installed (%v), calls %q", name, goos, err, made.calls)
			}
		}
		for _, path := range []string{filepath.Join(made.paths.Units, ServiceName), filepath.Join(made.paths.Units, TimerName), filepath.Join(made.paths.Agents, AgentName), made.paths.Hook} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("%s: %s was written", name, path)
			}
		}
	}
}

// The hook runs `loom queue-bridge install` only on the bridge (queue-bridge.conf there), and passes with a note on any
// other machine or on a release from before `loom queue-bridge` (a rollback past it).
func TestTheHookInstallsOnlyOnTheBridge(t *testing.T) {
	made := newMachine(t, "decides = no\n")
	if err := made.install("linux"); err != nil {
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
	current := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom queue-bridge [--config <queue-bridge.conf>]\\n  loom queue-bridge install\\n' >&2; exit 3; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(current); code != 7 || !strings.Contains(output, "ran queue-bridge install") {
		t.Fatalf("on the bridge: exit %d, %s", code, output)
	}
	rollback := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom run\\n' >&2; exit 3; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(rollback); code != 0 || strings.Contains(output, "ran ") || !strings.Contains(output, "has no queue-bridge install") {
		t.Fatalf("on a rollback: exit %d, %s", code, output)
	}
	os.Remove(made.paths.Config)
	if code, output := run(current); code != 0 || strings.Contains(output, "ran ") || !strings.Contains(output, "isn't the bridge") {
		t.Fatalf("without queue-bridge.conf: exit %d, %s", code, output)
	}
}
