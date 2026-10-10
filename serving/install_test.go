package serving

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestServeConfReadsAPoolAndRefusesAnythingElse(t *testing.T) {
	config, err := ReadConfig("# Cloud\npool = box-strict\n\n  phase-jobs=yes  \n")
	if err != nil || config != (Config{Pool: "box-strict", PhaseJobs: true}) {
		t.Fatalf("read %+v, %v", config, err)
	}
	if config, err := ReadConfig("pool=box-phase\nphase-jobs = no\n"); err != nil || config.PhaseJobs {
		t.Fatalf("phase-jobs = no read %+v, %v", config, err)
	}
	for name, content := range map[string]string{
		"no pool":               "phase-jobs = yes\n",
		"an empty pool":         "pool =\n",
		"a pool with a space":   "pool = box strict\n",
		"a specifier":           "pool = box%h\n",
		"a path":                "pool = ../board\n",
		"a pool named twice":    "pool = box-strict\npool = box-phase\n",
		"phase-jobs as true":    "pool = box-phase\nphase-jobs = true\n",
		"a setting it lacks":    "pool = box-strict\nworker = cloud\n",
		"a line with no equals": "pool = box-strict\nbox-phase\n",
	} {
		if config, err := ReadConfig(content); err == nil {
			t.Errorf("%s: read %+v", name, config)
		}
	}
}

// The unit is the template with the pool and the flags filled into ExecStart alone: its comments keep their words.
func TestTheUnitServesTheConfiguredPoolStrictAndDrainsOnReload(t *testing.T) {
	unit := Unit(Config{Pool: "box-strict"})
	var execStart []string
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "ExecStart=") {
			execStart = append(execStart, line)
		}
	}
	want := "ExecStart=%h/.loom/bin/loom-runner serve --strict --pool https://runs.loom.system.inc/pools/box-strict --token-file %t/loom-serve/pool-token --worker %H --until 1h --root %h/loom-serve/root --workspace %h/loom-serve/units"
	if len(execStart) != 1 || execStart[0] != want {
		t.Fatalf("ExecStart lines %q", execStart)
	}
	if phase := Unit(Config{Pool: "box-phase", PhaseJobs: true}); !strings.Contains(phase, "serve --strict --phase-jobs --pool https://runs.loom.system.inc/pools/box-phase ") {
		t.Fatalf("a phase box's unit:\n%s", phase)
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
	if Unit(Config{Pool: "box-strict"}) != strings.Replace(strings.Replace(unitTemplate, " FLAGS --pool", " --strict --pool", 1), "/pools/POOL ", "/pools/box-strict ", 1) {
		t.Fatal("rendering changed more than ExecStart's pool and flags")
	}
}

// A box as the hook finds it: serve.conf and a token, and a recorder in place of systemctl.
type box struct {
	paths Paths
	calls [][]string
	fail  string
}

func newBox(t *testing.T, config string, tokenMode os.FileMode) *box {
	home := t.TempDir()
	served := &box{paths: HomePaths(home)}
	os.MkdirAll(filepath.Join(home, ".loom"), 0o755)
	os.WriteFile(served.paths.Config, []byte(config), 0o644)
	os.WriteFile(served.paths.Token, []byte("pool-token\n"), tokenMode)
	os.Chmod(served.paths.Token, tokenMode)
	return served
}

func (served *box) install() error {
	return Install(served.paths, func(arguments ...string) error {
		served.calls = append(served.calls, arguments)
		if arguments[0] == served.fail {
			return errors.New("systemctl failed")
		}
		return nil
	}, io.Discard)
}

func (served *box) unit(t *testing.T) string {
	content, err := os.ReadFile(filepath.Join(served.paths.Units, UnitName))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

// Every release's hook runs install: the unit is written and systemd reloaded only when its text changed, and serve is
// enabled and reloaded (a drain, then the new runner) every time.
func TestInstallWritesTheUnitWhenItChangedAndReloadsServeEveryTime(t *testing.T) {
	served := newBox(t, "pool = box-strict\n", 0o600)
	if err := served.install(); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"daemon-reload"}, {"enable", UnitName}, {"reload-or-restart", UnitName}}
	if !reflect.DeepEqual(served.calls, want) || served.unit(t) != Unit(Config{Pool: "box-strict"}) {
		t.Fatalf("first install: %q", served.calls)
	}
	served.calls = nil
	if err := served.install(); err != nil || !reflect.DeepEqual(served.calls, want[1:]) {
		t.Fatalf("again with nothing changed: %q, %v", served.calls, err)
	}
	if _, err := os.Stat(filepath.Join(served.paths.Units, UnitName+".partial")); !os.IsNotExist(err) {
		t.Fatalf("a partial unit stayed: %v", err)
	}
	// A box moved to another pool gets its unit rewritten and systemd told.
	os.WriteFile(served.paths.Config, []byte("pool = box-phase\nphase-jobs = yes\n"), 0o644)
	served.calls = nil
	if err := served.install(); err != nil || !reflect.DeepEqual(served.calls, want) || served.unit(t) != Unit(Config{Pool: "box-phase", PhaseJobs: true}) {
		t.Fatalf("after a pool change: %q, %v", served.calls, err)
	}
	// A failing systemctl fails the hook, which the updater runs again next minute.
	served.calls, served.fail = nil, "reload-or-restart"
	if err := served.install(); err == nil {
		t.Fatal("a failed reload passed")
	}
}

// The token is the box's only credential: a token anyone else can read, or none, installs nothing.
func TestInstallRefusesATokenOthersCanReadOrNone(t *testing.T) {
	for name, served := range map[string]*box{
		"group-readable": newBox(t, "pool = box-strict\n", 0o640),
		"world-readable": newBox(t, "pool = box-strict\n", 0o604),
		"no settings":    newBox(t, "", 0o600),
	} {
		if err := served.install(); err == nil || len(served.calls) != 0 {
			t.Errorf("%s: installed (%v), calls %q", name, err, served.calls)
		}
	}
	missing := newBox(t, "pool = box-strict\n", 0o600)
	os.Remove(missing.paths.Token)
	empty := newBox(t, "pool = box-strict\n", 0o600)
	os.WriteFile(empty.paths.Token, nil, 0o600)
	for name, served := range map[string]*box{"missing": missing, "empty": empty} {
		if err := served.install(); err == nil || len(served.calls) != 0 {
			t.Errorf("a %s token: installed (%v), calls %q", name, err, served.calls)
		}
		if _, err := os.Stat(filepath.Join(served.paths.Units, UnitName)); !os.IsNotExist(err) {
			t.Errorf("a %s token: the unit was written", name)
		}
	}
}
