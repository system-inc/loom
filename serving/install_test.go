package serving

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

func TestServeConfReadsAPoolAndRefusesAnythingElse(t *testing.T) {
	config, err := ReadConfig("# Cloud\npool = box-strict\n\n  phase-jobs=yes  \nhas = go, clang,node,wasiSdk\n")
	if err != nil || !reflect.DeepEqual(config, Config{Pool: "box-strict", PhaseJobs: true, Has: []string{"go", "clang", "node", "wasiSdk"}}) {
		t.Fatalf("read %+v, %v", config, err)
	}
	if config, err := ReadConfig("pool=box-phase\nphase-jobs = no\n"); err != nil || config.PhaseJobs {
		t.Fatalf("phase-jobs = no read %+v, %v", config, err)
	}
	for name, content := range map[string]string{
		"no pool":                     "phase-jobs = yes\n",
		"an empty pool":               "pool =\n",
		"a pool with a space":         "pool = box strict\n",
		"a specifier":                 "pool = box%h\n",
		"a path":                      "pool = ../board\n",
		"a pool named twice":          "pool = box-strict\npool = box-phase\n",
		"phase-jobs as true":          "pool = box-phase\nphase-jobs = true\n",
		"a setting it lacks":          "pool = box-strict\nworker = cloud\n",
		"a line with no equals":       "pool = box-strict\nbox-phase\n",
		"a toolchain no probe checks": "pool = box-strict\nhas = go,rust\n",
		"a toolchain claimed twice":   "pool = box-strict\nhas = go,go\n",
	} {
		if config, err := ReadConfig(content); err == nil {
			t.Errorf("%s: read %+v", name, config)
		}
	}
}

// Two boxes may share a host name, so a worker is its host's short name and the first six digits of its machine id.
func TestAWorkersNameIsItsHostAndItsMachine(t *testing.T) {
	machineId := "4f1d2c3b4a5968778695a4b3c2d1e0f9\n"
	if name, err := WorkerName("cloud.lan", machineId); err != nil || name != "cloud-4f1d2c" {
		t.Fatalf("named %q, %v", name, err)
	}
	for name, test := range map[string][2]string{
		"no host":         {"", machineId},
		"a host with %":   {"cl%oud", machineId},
		"a short id":      {"cloud", "4f1d2c"},
		"an uppercase id": {"cloud", strings.ToUpper(machineId)},
		"no machine id":   {"cloud", ""},
		"a host with a /": {"a/b", machineId},
		"a host too long": {strings.Repeat("a", 64), machineId},
	} {
		if worker, err := WorkerName(test[0], test[1]); err == nil {
			t.Errorf("%s: named %q", name, worker)
		}
	}
}

// The unit is the template with the pool, the flags and the worker filled into ExecStart alone: its comments keep their
// words.
func TestTheUnitServesTheConfiguredPoolStrictAndDrainsOnReload(t *testing.T) {
	unit := Unit(Config{Pool: "box-strict"}, "cloud-4f1d2c", "")
	var execStart []string
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			execStart = append(execStart, line)
		}
	}
	want := "ExecStart=%h/.loom/bin/loom-runner serve --strict --pool https://runs.loom.system.inc/pools/box-strict --token-file %t/loom-serve/pool-token --worker cloud-4f1d2c --until 1h --root %h/loom-serve/root --workspace %h/loom-serve/units"
	if len(execStart) != 1 || execStart[0] != want {
		t.Fatalf("ExecStart lines %q", execStart)
	}
	if phase := Unit(Config{Pool: "box-phase", PhaseJobs: true}, "cloud-4f1d2c", ""); !strings.Contains(phase, "serve --strict --phase-jobs --pool https://runs.loom.system.inc/pools/box-phase ") {
		t.Fatalf("a phase box's unit:\n%s", phase)
	}
	if claims := Unit(Config{Pool: "box-strict", Has: []string{"go", "wasiSdk"}}, "cloud-4f1d2c", ""); !strings.Contains(claims, "serve --strict --has go,wasiSdk --pool https://runs.loom.system.inc/pools/box-strict ") {
		t.Fatalf("a box claiming toolchains:\n%s", claims)
	}
	for _, line := range []string{
		"ExecStartPre=/usr/bin/install -m 600 %h/.loom/serve-token %t/loom-serve/pool-token",
		"RuntimeDirectoryMode=0700",
		"ExecReload=/bin/kill -HUP $MAINPID",
		"KillMode=mixed",
		"Restart=always",
		"WantedBy=default.target",
	} {
		if !strings.Contains(unit, "\n"+line+"\n") {
			t.Errorf("the unit has no %q", line)
		}
	}
	// A user unit can't order itself after the system's network-online.target; it would only wait on nothing.
	if strings.Contains(unit, "network-online") || strings.Contains(unit, "--exclusive") {
		t.Errorf("the unit waits on the system's network, or serves --exclusive:\n%s", unit)
	}
}

// A box as the hook finds it: serve.conf, a token, a machine id, an installed runner, and a recorder in place of
// systemctl that answers `show` with serve's main pid, whose /proc/<pid>/exe is a link the test points.
type box struct {
	paths   Paths
	calls   [][]string
	fail    string
	mainPid string
}

func newBox(t *testing.T, config string, tokenMode os.FileMode) *box {
	home := t.TempDir()
	served := &box{paths: HomePaths(home), mainPid: "0"}
	served.paths.Host, served.paths.MachineId, served.paths.Proc = "cloud", filepath.Join(home, "machine-id"), filepath.Join(home, "proc")
	os.MkdirAll(filepath.Join(home, ".loom", "bin"), 0o755)
	os.WriteFile(served.paths.Config, []byte(config), 0o644)
	os.WriteFile(served.paths.Token, []byte("pool-token\n"), tokenMode)
	os.Chmod(served.paths.Token, tokenMode)
	os.WriteFile(served.paths.MachineId, []byte("4f1d2c3b4a5968778695a4b3c2d1e0f9\n"), 0o444)
	os.WriteFile(served.paths.Binary, []byte("release one"), 0o755)
	return served
}

// running says serve runs as pid 4242 from binary.
func (served *box) running(t *testing.T, binary string) {
	served.mainPid = "4242"
	exe := filepath.Join(served.paths.Proc, "4242", "exe")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.Remove(exe)
	if err := os.Symlink(binary, exe); err != nil {
		t.Fatal(err)
	}
}

func (served *box) install() error {
	served.calls = nil
	return Install(served.paths, func(arguments ...string) (string, error) {
		served.calls = append(served.calls, arguments)
		if arguments[0] == served.fail {
			return "", errors.New("systemctl failed")
		}
		if arguments[0] == "show" {
			return served.mainPid + "\n", nil
		}
		return "", nil
	}, io.Discard)
}

func (served *box) unit(t *testing.T) string {
	content, err := os.ReadFile(filepath.Join(served.paths.Units, UnitName))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

var (
	reloadCall = []string{"daemon-reload"}
	enableCall = []string{"enable", UnitName}
	showCall   = []string{"show", "--property=MainPID", "--value", UnitName}
	startCall  = []string{"start", UnitName}
	reloadUnit = []string{"reload", UnitName}
)

// A release's hook runs install every time: the unit is written, and systemd told, only when its text changed; a
// stopped serve is started; a running one is reloaded (a drain, then the new runner) only when its unit changed or it
// runs another binary than the one installed, and otherwise left alone.
func TestInstallReloadsServeOnlyWhenItsUnitOrItsRunnerChanged(t *testing.T) {
	served := newBox(t, "pool = box-strict\n", 0o600)
	if err := served.install(); err != nil || !reflect.DeepEqual(served.calls, [][]string{reloadCall, enableCall, showCall, startCall}) {
		t.Fatalf("first install: %q, %v", served.calls, err)
	}
	if served.unit(t) != Unit(Config{Pool: "box-strict"}, "cloud-4f1d2c", "") {
		t.Fatalf("the unit written:\n%s", served.unit(t))
	}
	if _, err := os.Stat(filepath.Join(served.paths.Units, UnitName+".partial")); !os.IsNotExist(err) {
		t.Fatalf("a partial unit stayed: %v", err)
	}
	// Running the installed binary, nothing changed: left alone, however often the hook runs.
	served.running(t, served.paths.Binary)
	for range 2 {
		if err := served.install(); err != nil || !reflect.DeepEqual(served.calls, [][]string{enableCall, showCall}) {
			t.Fatalf("again with nothing changed: %q, %v", served.calls, err)
		}
	}
	// A release linked a new binary: serve still runs the old one, so it is reloaded.
	old := filepath.Join(filepath.Dir(served.paths.Binary), "old-loom-runner")
	os.WriteFile(old, []byte("release zero"), 0o755)
	served.running(t, old)
	if err := served.install(); err != nil || !reflect.DeepEqual(served.calls, [][]string{enableCall, showCall, reloadUnit}) {
		t.Fatalf("after a release: %q, %v", served.calls, err)
	}
	// A box moved to another pool gets its unit rewritten, systemd told, and serve reloaded.
	served.running(t, served.paths.Binary)
	os.WriteFile(served.paths.Config, []byte("pool = box-phase\nphase-jobs = yes\n"), 0o644)
	if err := served.install(); err != nil || !reflect.DeepEqual(served.calls, [][]string{reloadCall, enableCall, showCall, reloadUnit}) ||
		served.unit(t) != Unit(Config{Pool: "box-phase", PhaseJobs: true}, "cloud-4f1d2c", "") {
		t.Fatalf("after a pool change: %q, %v", served.calls, err)
	}
	// A failing systemctl fails the hook, which the updater runs again next minute.
	served.running(t, old)
	served.fail = "reload"
	if err := served.install(); err == nil {
		t.Fatal("a failed reload passed")
	}
}

// The house cache is update.conf's house-cache line: serve is given it, and when the line changes (a new host, or none)
// the unit is rewritten and serve reloaded onto it. A line that isn't a plain address refuses the install before serve
// is touched.
func TestServeAsksTheHouseCacheUpdateConfNames(t *testing.T) {
	served := newBox(t, "pool = box-strict\n", 0o600)
	os.WriteFile(served.paths.UpdateConfig, []byte("base = https://artifacts.loom.system.inc/releases\nhouse-cache = http://10.10.102.20:7380\n"), 0o644)
	if err := served.install(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(served.unit(t), "serve --strict --house-cache http://10.10.102.20:7380 --pool ") {
		t.Fatalf("the unit asks no house cache:\n%s", served.unit(t))
	}
	// The house cache moves to Server: one line on this box, and serve is reloaded onto it.
	served.running(t, served.paths.Binary)
	os.WriteFile(served.paths.UpdateConfig, []byte("base = https://artifacts.loom.system.inc/releases\nhouse-cache = http://10.10.102.21:7380\n"), 0o644)
	if err := served.install(); err != nil || !reflect.DeepEqual(served.calls, [][]string{reloadCall, enableCall, showCall, reloadUnit}) ||
		!strings.Contains(served.unit(t), " --house-cache http://10.10.102.21:7380 ") {
		t.Fatalf("after the house cache moved: %q, %v", served.calls, err)
	}
	// A line that isn't an address refuses the install, and serve keeps running as it is.
	for _, bad := range []string{"house-cache = 10.10.102.21:7380\n", "house-cache = http://10.10.102.21:7380/blobs\n", "house-cache = http://x:1 --exclusive\n"} {
		os.WriteFile(served.paths.UpdateConfig, []byte(bad), 0o644)
		if err := served.install(); err == nil || len(served.calls) != 0 {
			t.Errorf("%q: installed (%v), calls %q", bad, err, served.calls)
		}
	}
	// No line, or no update.conf at all, is no house cache.
	os.WriteFile(served.paths.UpdateConfig, []byte("base = https://artifacts.loom.system.inc/releases\n# house-cache = http://10.10.102.21:7380\n"), 0o644)
	if err := served.install(); err != nil || strings.Contains(served.unit(t), "house-cache") {
		t.Fatalf("with the line gone: %v\n%s", err, served.unit(t))
	}
	os.Remove(served.paths.UpdateConfig)
	if err := served.install(); err != nil || strings.Contains(served.unit(t), "house-cache") {
		t.Fatalf("with no update.conf: %v", err)
	}
}

// Everything that can refuse an install comes before serve is touched, so a refused install, which the updater runs
// again every minute, never reloads serve.
func TestARefusedInstallNeverTouchesServe(t *testing.T) {
	cases := map[string]*box{
		"group-readable": newBox(t, "pool = box-strict\n", 0o640),
		"world-readable": newBox(t, "pool = box-strict\n", 0o604),
		"no settings":    newBox(t, "", 0o600),
	}
	missing := newBox(t, "pool = box-strict\n", 0o600)
	os.Remove(missing.paths.Token)
	cases["a missing token"] = missing
	empty := newBox(t, "pool = box-strict\n", 0o600)
	os.WriteFile(empty.paths.Token, nil, 0o600)
	cases["an empty token"] = empty
	others := newBox(t, "pool = box-strict\n", 0o600)
	others.paths.User++
	cases["another user's token"] = others
	noMachine := newBox(t, "pool = box-strict\n", 0o600)
	os.Remove(noMachine.paths.MachineId)
	cases["no machine id"] = noMachine
	for name, served := range cases {
		old := filepath.Join(filepath.Dir(served.paths.Binary), "old-loom-runner")
		os.WriteFile(old, []byte("release zero"), 0o755)
		served.running(t, old)
		if err := served.install(); err == nil || len(served.calls) != 0 {
			t.Errorf("%s: installed (%v), calls %q", name, err, served.calls)
		}
		if _, err := os.Stat(filepath.Join(served.paths.Units, UnitName)); !os.IsNotExist(err) {
			t.Errorf("%s: the unit was written", name)
		}
	}
}

// install writes the updater's hook too, so it is the binary's to keep. The hook runs install-serve, and a release
// from before install-serve (a rollback past it) passes with a note, loom-serve left as it runs.
func TestTheHookRunsInstallServeOrPassesOnARollback(t *testing.T) {
	served := newBox(t, "pool = box-strict\n", 0o600)
	if err := served.install(); err != nil {
		t.Fatal(err)
	}
	hook, err := os.Stat(served.paths.Hook)
	if err != nil || hook.Mode().Perm() != 0o755 {
		t.Fatalf("the hook: %v, %v", hook, err)
	}
	// The health probe puts serve's state in the updater's every report.
	if probe, err := os.ReadFile(served.paths.Probe); err != nil || !strings.Contains(string(probe), `units="loom-serve.service"`) {
		t.Fatalf("the probe: %v\n%s", err, probe)
	}
	run := func(runner string) (int, string) {
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "loom-runner"), []byte(runner), 0o755)
		command := exec.Command(served.paths.Hook)
		command.Env = []string{"PATH=/usr/bin:/bin", "LOOM_UPDATE_BIN=" + bin}
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	current := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom-runner install-serve\\n  loom-runner version\\n' >&2; exit 2; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(current); code != 7 || !strings.Contains(output, "ran install-serve") {
		t.Fatalf("with install-serve: exit %d, %s", code, output)
	}
	rollback := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom-runner version\\n' >&2; exit 2; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(rollback); code != 0 || strings.Contains(output, "ran ") || !strings.Contains(output, "has no install-serve") {
		t.Fatalf("on a rollback: exit %d, %s", code, output)
	}
}

// The clock hook (#nmx30ay), run against stubs that record every call: a WSL box with Hyper-V's clock, timesyncd
// enabled and its tick turned down, gets timesyncd stopped and masked and the tick put back; one already fixed (noise
// in the frequency) is left alone; anywhere but WSL with Hyper-V's clock nothing is touched, since there timesyncd may
// be the only time sync; a refused sudo fails the hook, named. Each mutant below fails a case here: the WSL guard
// dropped (the plain Linux box), the Hyper-V guard dropped, the mask dropped, the tick reset dropped, the 1 ppm
// tolerance dropped (the fixed box).
func TestTheClockHookKeepsHyperVsTimeSyncAlone(t *testing.T) {
	served := newBox(t, "pool = box-strict\n", 0o600)
	if err := served.install(); err != nil {
		t.Fatal(err)
	}
	if hook, err := os.Stat(served.paths.ClockHook); err != nil || hook.Mode().Perm() != 0o755 {
		t.Fatalf("the clock hook: %v, %v", hook, err)
	}
	for _, test := range []struct {
		name, release, timesyncd, adjtimex, sudo string
		hyperv                                   bool
		code                                     int
		calls                                    string
		says                                     string
	}{
		{"a plain Linux box", "6.8.0-45-generic", "enabled", "9000 0", "ok", true, 0, "", "not WSL"},
		{"WSL without Hyper-V's clock", "6.18.40.1-microsoft-standard-WSL2", "enabled", "9000 0", "ok", false, 0, "", "no Hyper-V clock"},
		{"the boxes on Oct 10", "6.18.40.1-microsoft-standard-WSL2", "enabled", "9162 -2832001", "ok", true, 0,
			"sudo systemctl stop systemd-timesyncd|sudo systemctl mask systemd-timesyncd|sudo python3 set 10000|", "put back to 10000 and 0"},
		{"a box already fixed", "6.18.40.1-microsoft-standard-WSL2", "masked", "10000 41", "ok", true, 0, "", "alone keeps the clock"},
		{"sudo refused", "6.18.40.1-microsoft-standard-WSL2", "enabled", "9000 0", "refused", true, 1, "", "couldn't stop and mask"},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			bin := filepath.Join(directory, "bin")
			os.MkdirAll(bin, 0o755)
			calls := filepath.Join(directory, "calls")
			release := filepath.Join(directory, "osrelease")
			os.WriteFile(release, []byte(test.release+"\n"), 0o644)
			hyperv := filepath.Join(directory, "ptp_hyperv")
			if test.hyperv {
				os.WriteFile(hyperv, nil, 0o644)
			}
			stubs := map[string]string{
				"systemctl": `[ "$1" = is-enabled ] && { echo ` + test.timesyncd + `; exit 0; }; echo "systemctl $*" >> ` + calls + `; exit 0`,
				"sudo":      `[ "` + test.sudo + `" = ok ] || exit 1; [ "$1" = -n ] && shift; printf 'sudo ' >> ` + calls + `; exec "$@"`,
				"python3":   `if [ $# -gt 2 ]; then echo "python3 set $3" >> ` + calls + `; echo 10000 0; else echo ` + test.adjtimex + `; fi`,
			}
			for name, script := range stubs {
				os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755)
			}
			command := exec.Command(served.paths.ClockHook)
			command.Env = []string{"PATH=" + bin + ":/usr/bin:/bin", "LOOM_CLOCK_RELEASE=" + release, "LOOM_CLOCK_HYPERV=" + hyperv}
			output, _ := command.CombinedOutput()
			recorded, _ := os.ReadFile(calls)
			got := strings.ReplaceAll(strings.TrimSpace(string(recorded)), "\n", "|")
			if got != "" {
				got += "|"
			}
			if command.ProcessState.ExitCode() != test.code || got != test.calls || !strings.Contains(string(output), test.says) {
				t.Fatalf("exit %d, calls %q, said %q; want exit %d, calls %q, saying %q", command.ProcessState.ExitCode(), got, output, test.code, test.calls, test.says)
			}
		})
	}
}
