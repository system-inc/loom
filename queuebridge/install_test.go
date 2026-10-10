package queuebridge

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// Each mutant below must make a test here fail:
//
//	a setting it doesn't know taken quietly: TestQueueBridgeConfReadsEachSettingOverWorkshopsDefaults
//	install enabling or starting the timer: TestInstallWritesTheUnitsAndNeverStartsThem
//	install without queue-bridge.conf: TestInstallRefusesAMachineThatIsntTheBridge
//	the hook installing on a machine without queue-bridge.conf: TestTheHookInstallsOnlyOnTheBridge
//	install passing with any gap in what a pass reads, or before writing the units: TestInstallPreflightsWhatAPassReads

func TestQueueBridgeConfReadsEachSettingOverWorkshopsDefaults(t *testing.T) {
	home := "/home/ahra"
	if config, err := ReadConfig("", home); err != nil || config != DefaultConfig(home) ||
		config.Repository != "/home/ahra/loom-queue-bridge/adamic.git" || config.Token != "/home/ahra/.loom/queue-bridge.token" {
		t.Fatalf("an empty queue-bridge.conf read %+v, %v", config, err)
	}
	config, err := ReadConfig("# rehearsal\n  queue=http://127.0.0.1:8787  \nrepository = ~/adamic\nstate = /tmp/state\ntoken = ~/t\n", home)
	want := Config{Queue: "http://127.0.0.1:8787", Repository: "/home/ahra/adamic", State: "/tmp/state", Token: "/home/ahra/t"}
	if err != nil || config != want {
		t.Fatalf("read %+v, %v", config, err)
	}
	for name, content := range map[string]string{
		"a queue twice":         "queue = https://a.example\nqueue = https://b.example\n",
		"an empty value":        "state =\n",
		"a queue that's no URL": "queue = loom.system.inc\n",
		"a relative clone":      "repository = adamic\n",
		"a setting it lacks":    "decides = no\n",
		"a line with no equals": "state = /tmp/state\nyes\n",
	} {
		if config, err := ReadConfig(content, home); err == nil {
			t.Errorf("%s: read %+v", name, config)
		}
	}
}

// The units are the checked-in files: a oneshot pass of the release's loom, and a timer every 60 s that a reboot keeps
// once it is enabled.
func TestTheUnitsRunTheReleasesLoomEvery60Seconds(t *testing.T) {
	for _, line := range []string{"Type=oneshot", "ExecStart=%h/.loom/bin/loom queue-bridge", "StandardOutput=append:%h/.loom/queue-bridge.log"} {
		if !strings.Contains(serviceText, "\n"+line+"\n") {
			t.Errorf("the service has no %q", line)
		}
	}
	if strings.Contains(serviceText, "python") || strings.Contains(serviceText, "[Install]") {
		t.Errorf("the service runs python, or installs itself:\n%s", serviceText)
	}
	for _, line := range []string{"OnUnitActiveSec=60s", "[Install]", "WantedBy=timers.target"} {
		if !strings.Contains(timerText, "\n"+line+"\n") {
			t.Errorf("the timer has no %q", line)
		}
	}
}

type machine struct {
	home  string
	paths Paths
	calls [][]string
	fail  bool
}

// installedAt is the time every install here runs at, so a token's expiry reads the same every run.
var installedAt = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

// newMachine is a home with config as its queue-bridge.conf and, when it has one, what a pass reads at the defaults:
// a coordinator token for 30 days, and a bare clone whose origin is a local path, as the tests' clones are.
func newMachine(t *testing.T, config string) *machine {
	home := t.TempDir()
	made := &machine{home: home, paths: HomePaths(home)}
	if config != "" {
		os.MkdirAll(filepath.Dir(made.paths.Config), 0o755)
		os.WriteFile(made.paths.Config, []byte(config), 0o644)
		defaults := DefaultConfig(home)
		made.token(t, protocol.ScopeCoordinator, installedAt.Add(30*24*time.Hour))
		origin := filepath.Join(home, "origin.git")
		gitIn(t, home, "init", "-q", "--bare", origin)
		gitIn(t, home, "init", "-q", "--bare", defaults.Repository)
		gitIn(t, defaults.Repository, "remote", "add", "origin", origin)
	}
	return made
}

// token writes the bridge's token at its default path: scope, expiring at expires.
func (made *machine) token(t *testing.T, scope string, expires time.Time) {
	t.Helper()
	token, err := protocol.MintToken([]byte(strings.Repeat("s", 64)), protocol.TokenClaims{Run: "queue-bridge", Scope: scope, Expires: expires.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(DefaultConfig(made.home).Token, []byte(token+"\n"), 0o600)
}

func (made *machine) install() error {
	return made.installReporting(io.Discard)
}

func (made *machine) installReporting(report io.Writer) error {
	made.calls = nil
	return Install(made.paths, made.home, installedAt, func(arguments ...string) (string, error) {
		made.calls = append(made.calls, arguments)
		if made.fail {
			return "", errors.New("systemctl failed")
		}
		return "", nil
	}, report)
}

// Install writes the hook and the units, tells systemd only when a unit changed, and never enables, starts or stops
// anything: whether the bridge runs is Kirk's call.
func TestInstallWritesTheUnitsAndNeverStartsThem(t *testing.T) {
	made := newMachine(t, "queue = https://loom.system.inc\n")
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
	os.WriteFile(filepath.Join(made.paths.Units, ServiceName), []byte("ExecStart=/usr/bin/python3 queue_bridge.py\n"), 0o644)
	if err := made.install(); err != nil || !reflect.DeepEqual(made.calls, [][]string{{"daemon-reload"}}) {
		t.Fatalf("over a stale unit: %q, %v", made.calls, err)
	}
	os.Remove(filepath.Join(made.paths.Units, TimerName))
	made.fail = true
	if err := made.install(); err == nil {
		t.Fatal("a failed daemon-reload passed")
	}
}

// Without queue-bridge.conf, or with one it can't read, this machine isn't the bridge: nothing is written.
func TestInstallRefusesAMachineThatIsntTheBridge(t *testing.T) {
	for name, made := range map[string]*machine{"no queue-bridge.conf": newMachine(t, ""), "a bad queue-bridge.conf": newMachine(t, "decides = no\n")} {
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

// The hook runs `loom queue-bridge install` only on the bridge (queue-bridge.conf there), and passes with a note on any
// other machine or on a release from before `loom queue-bridge` (a rollback past it).
func TestTheHookInstallsOnlyOnTheBridge(t *testing.T) {
	made := newMachine(t, "queue = https://loom.system.inc\n")
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

// Install writes the release's units first, then preflights what a pass reads and fails naming every gap, so the
// updater's hook fails and the release says so before a pass fails quietly in its log (#18kj26x). A token near its
// expiry is named and passes.
func TestInstallPreflightsWhatAPassReads(t *testing.T) {
	for name, each := range map[string]struct {
		breakIt func(t *testing.T, made *machine)
		want    string
	}{
		"no token": {func(t *testing.T, made *machine) { os.Remove(DefaultConfig(made.home).Token) }, "loom coordinator-token queue-bridge --days N >"},
		"an expired token": {func(t *testing.T, made *machine) {
			made.token(t, protocol.ScopeCoordinator, installedAt.Add(-time.Minute))
		}, "expired at 2026-10-10T11:59:00Z"},
		"a board token": {func(t *testing.T, made *machine) { made.token(t, protocol.ScopeBoard, installedAt.Add(time.Hour)) }, "is a board token, not a coordinator one"},
		"a token that isn't one": {func(t *testing.T, made *machine) {
			os.WriteFile(DefaultConfig(made.home).Token, []byte("not-a-token\n"), 0o600)
		}, "malformed token"},
		"no clone": {func(t *testing.T, made *machine) {
			os.Rename(DefaultConfig(made.home).Repository, DefaultConfig(made.home).Repository+".gone")
		}, "git clone --bare https://github.com/system-inc/adamic.git"},
		"a clone that needs a key": {func(t *testing.T, made *machine) {
			gitIn(t, DefaultConfig(made.home).Repository, "remote", "set-url", "origin", "git@github-lander:system-inc/adamic.git")
		}, "isn't a github.com repository over https with no key in it"},
		"a clone with a rewrite": {func(t *testing.T, made *machine) {
			gitIn(t, DefaultConfig(made.home).Repository, "config", "url.https://kirk:key@x/.insteadOf", "https://github.com/")
		}, "holds url.https://kirk:key@x/.insteadof"},
	} {
		made := newMachine(t, "queue = https://loom.system.inc\n")
		each.breakIt(t, made)
		err := made.install()
		if err == nil || !strings.Contains(err.Error(), "a pass can't run on this machine") || !strings.Contains(err.Error(), each.want) {
			t.Errorf("%s: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(made.paths.Units, ServiceName)); err != nil {
			t.Errorf("%s: the units weren't written first: %v", name, err)
		}
	}
	// Every gap at once is named at once.
	made := newMachine(t, "queue = https://loom.system.inc\n")
	os.Remove(DefaultConfig(made.home).Token)
	os.Rename(DefaultConfig(made.home).Repository, DefaultConfig(made.home).Repository+".gone")
	if err := made.install(); err == nil || !strings.Contains(err.Error(), "its token") || !strings.Contains(err.Error(), "its clone") {
		t.Fatalf("two gaps: %v", err)
	}
	// Three days from expiry: named, and the install passes.
	made = newMachine(t, "queue = https://loom.system.inc\n")
	made.token(t, protocol.ScopeCoordinator, installedAt.Add(3*24*time.Hour))
	var report bytes.Buffer
	if err := made.installReporting(&report); err != nil || !strings.Contains(report.String(), "expires at 2026-10-13T12:00:00Z: mint another before then") {
		t.Fatalf("a token near its expiry: %v\n%s", err, report.String())
	}
}
