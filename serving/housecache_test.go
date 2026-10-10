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

func TestHouseCacheConfReadsAnAddressOnTheHouseAndRefusesAnythingElse(t *testing.T) {
	config, err := ReadHouseCacheConfig("# Cloud hosts the big house's cache\nlisten = 10.10.102.20:7380\nlimit-gb = 200\nfloor-gb = 50\ndirectory = /srv/loom-house-cache\n")
	if err != nil || config != (HouseCacheConfig{Listen: "10.10.102.20:7380", Directory: "/srv/loom-house-cache", LimitGB: 200, FloorGB: 50}) {
		t.Fatalf("read %+v, %v", config, err)
	}
	if config, err := ReadHouseCacheConfig("listen = 10.0.0.2:7380\n"); err != nil || config.LimitGB != DefaultHouseCacheLimitGB || config.FloorGB != DefaultHouseCacheFloorGB {
		t.Fatalf("defaults: %+v, %v", config, err)
	}
	if config, err := ReadHouseCacheConfig("listen = 100.101.102.103:7380\ntailnet = yes\n"); err != nil || !config.Tailnet ||
		!strings.Contains(HouseCacheUnit(config), " --tailnet\n") {
		t.Fatalf("a tailnet's address with tailnet = yes: %+v, %v", config, err)
	}
	if _, err := ReadHouseCacheConfig("listen = 8.8.8.8:7380\npublic = yes\n"); err != nil {
		t.Fatalf("a public address forced: %v", err)
	}
	for _, bad := range []string{
		"",
		"limit-gb = 10\n",
		"listen = 0.0.0.0:7380\n",
		"listen = 8.8.8.8:7380\n",
		"listen = 100.101.102.103:7380\n",
		"listen = 10.10.102.20:7380\ntailnet = maybe\n",
		"listen = cloud.local:7380\n",
		"listen = 10.10.102.20:7380\nlisten = 10.10.102.21:7380\n",
		"listen = 10.10.102.20:7380\nport = 7380\n",
		"listen = 10.10.102.20:7380\nlimit-gb = 0\n",
		"listen = 10.10.102.20:7380\nfloor-gb = lots\n",
		"listen = 10.10.102.20:7380\ndirectory = loom-house-cache\n",
		"listen = 10.10.102.20:7380\ndirectory = /srv/house cache\n",
		"listen = 10.10.102.20:7380\npublic = maybe\n",
	} {
		if config, err := ReadHouseCacheConfig(bad); err == nil {
			t.Errorf("%q read as %+v", bad, config)
		}
	}
}

func TestTheHouseCacheUnitServesItsSettings(t *testing.T) {
	unit := HouseCacheUnit(HouseCacheConfig{Listen: "10.10.102.20:7380", LimitGB: 100, FloorGB: 20})
	want := "ExecStart=%h/.loom/bin/loom house-cache serve --listen 10.10.102.20:7380 --directory %h/loom-house-cache --limit-gb 100 --floor-gb 20"
	if !strings.Contains(unit, "\n"+want+"\n") || strings.Contains(unit, "SETTINGS") || !strings.Contains(unit, "\nRestart=always\n") {
		t.Fatalf("the unit:\n%s", unit)
	}
	if public := HouseCacheUnit(HouseCacheConfig{Listen: "8.8.8.8:7380", Directory: "/srv/c", LimitGB: 1, Public: true}); !strings.Contains(public, " --directory /srv/c --limit-gb 1 --floor-gb 0 --public\n") {
		t.Fatalf("a forced public unit:\n%s", public)
	}
}

// A house as the hook finds it: a box with loom installed, house-cache.conf or none, and a recorder in place of
// systemctl.
type house struct {
	paths   HouseCachePaths
	calls   [][]string
	mainPid string
}

func newHouse(t *testing.T, config string) *house {
	home := t.TempDir()
	host := &house{paths: HouseCacheHomePaths(home), mainPid: "0"}
	host.paths.Proc = filepath.Join(home, "proc")
	os.MkdirAll(filepath.Join(home, ".loom", "bin"), 0o755)
	os.WriteFile(host.paths.Binary, []byte("release one"), 0o755)
	if config != "" {
		os.WriteFile(host.paths.Config, []byte(config), 0o644)
	}
	return host
}

func (host *house) running(t *testing.T, binary string) {
	host.mainPid = "4343"
	exe := filepath.Join(host.paths.Proc, "4343", "exe")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.Remove(exe)
	if err := os.Symlink(binary, exe); err != nil {
		t.Fatal(err)
	}
}

func (host *house) install() error {
	host.calls = nil
	return InstallHouseCache(host.paths, func(arguments ...string) (string, error) {
		host.calls = append(host.calls, arguments)
		if arguments[0] == "show" {
			return host.mainPid + "\n", nil
		}
		return "", nil
	}, io.Discard)
}

var (
	houseEnable  = []string{"enable", HouseCacheUnitName}
	houseShow    = []string{"show", "--property=MainPID", "--value", HouseCacheUnitName}
	houseStart   = []string{"start", HouseCacheUnitName}
	houseRestart = []string{"restart", HouseCacheUnitName}
	houseDisable = []string{"disable", "--now", HouseCacheUnitName}
)

// The box house-cache.conf is on hosts the cache: a first install starts it, a rerun with nothing changed leaves it,
// a new release or new settings restart it. The box it leaves stops it and loses the unit and the hook, so it serves
// nothing; a box that never hosted is left alone.
func TestTheHouseCacheRunsWhereItsConfIsAndNowhereElse(t *testing.T) {
	host := newHouse(t, "listen = 10.10.102.20:7380\n")
	if err := host.install(); err != nil || !reflect.DeepEqual(host.calls, [][]string{reloadCall, houseEnable, houseShow, houseStart}) {
		t.Fatalf("first install: %q, %v", host.calls, err)
	}
	unit := filepath.Join(host.paths.Units, HouseCacheUnitName)
	if content, _ := os.ReadFile(unit); string(content) != HouseCacheUnit(HouseCacheConfig{Listen: "10.10.102.20:7380", LimitGB: 100, FloorGB: 20}) {
		t.Fatalf("the unit written:\n%s", content)
	}
	if hook, err := os.Stat(host.paths.Hook); err != nil || hook.Mode().Perm() != 0o755 {
		t.Fatalf("the hook: %v, %v", hook, err)
	}
	host.running(t, host.paths.Binary)
	for range 2 {
		if err := host.install(); err != nil || !reflect.DeepEqual(host.calls, [][]string{houseEnable, houseShow}) {
			t.Fatalf("again with nothing changed: %q, %v", host.calls, err)
		}
	}
	old := filepath.Join(filepath.Dir(host.paths.Binary), "old-loom")
	os.WriteFile(old, []byte("release zero"), 0o755)
	host.running(t, old)
	if err := host.install(); err != nil || !reflect.DeepEqual(host.calls, [][]string{houseEnable, houseShow, houseRestart}) {
		t.Fatalf("after a release: %q, %v", host.calls, err)
	}
	host.running(t, host.paths.Binary)
	os.WriteFile(host.paths.Config, []byte("listen = 10.10.102.20:7380\nlimit-gb = 300\n"), 0o644)
	if err := host.install(); err != nil || !reflect.DeepEqual(host.calls, [][]string{reloadCall, houseEnable, houseShow, houseRestart}) {
		t.Fatalf("after new settings: %q, %v", host.calls, err)
	}
	// Settings that would serve the internet refuse the install before the cache is touched.
	os.WriteFile(host.paths.Config, []byte("listen = 0.0.0.0:7380\n"), 0o644)
	if err := host.install(); err == nil || len(host.calls) != 0 {
		t.Fatalf("a bind to every address: %q, %v", host.calls, err)
	}
	if content, _ := os.ReadFile(unit); !strings.Contains(string(content), "--listen 10.10.102.20:7380 ") {
		t.Fatalf("a refused install rewrote the unit:\n%s", content)
	}
	// The house cache moves away: this box stops it, and its unit and hook go.
	os.Remove(host.paths.Config)
	if err := host.install(); err != nil || !reflect.DeepEqual(host.calls, [][]string{houseDisable, reloadCall}) {
		t.Fatalf("after the house cache moved away: %q, %v", host.calls, err)
	}
	for _, gone := range []string{unit, host.paths.Hook} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stayed: %v", gone, err)
		}
	}
	never := newHouse(t, "")
	if err := never.install(); err != nil || len(never.calls) != 0 {
		t.Fatalf("a box that never hosted: %q, %v", never.calls, err)
	}
}

// The hook runs `loom house-cache install` from the release just installed, and passes on a release from before it.
func TestTheHouseCacheHookRunsInstallOrPassesOnARollback(t *testing.T) {
	host := newHouse(t, "listen = 10.10.102.20:7380\n")
	if err := host.install(); err != nil {
		t.Fatal(err)
	}
	run := func(loom string) (int, string) {
		bin := t.TempDir()
		os.WriteFile(filepath.Join(bin, "loom"), []byte(loom), 0o755)
		command := exec.Command(host.paths.Hook)
		command.Env = []string{"PATH=/usr/bin:/bin", "LOOM_UPDATE_BIN=" + bin}
		output, _ := command.CombinedOutput()
		return command.ProcessState.ExitCode(), string(output)
	}
	current := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom house-cache serve --listen <ip>:<port>\\n  loom house-cache install\\n' >&2; exit 3; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(current); code != 7 || !strings.Contains(output, "ran house-cache install") {
		t.Fatalf("with house-cache install: exit %d, %s", code, output)
	}
	rollback := "#!/bin/sh\nif [ $# = 0 ]; then printf 'usage:\\n  loom run <job.json>\\n' >&2; exit 3; fi\necho \"ran $*\"; exit 7\n"
	if code, output := run(rollback); code != 0 || strings.Contains(output, "ran ") || !strings.Contains(output, "has no house-cache install") {
		t.Fatalf("on a rollback: exit %d, %s", code, output)
	}
}
