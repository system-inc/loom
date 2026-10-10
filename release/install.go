package release

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/serving"
	"github.com/system-inc/loom/updater"
)

//go:embed systemd/loom-release.service
var unitText string

//go:embed updated.d/60-release
var hookText string

// UnitName is the watcher's unit under ~/.config/systemd/user.
const UnitName = "loom-release.service"

// HookName is the updater's hook, and the health probe, that `loom release install` writes.
const HookName = "60-release"

// InstallPaths are what Install reads and writes: release.conf, the systemd user unit directory, the updater's hook
// and health probe, the loom binary the updater installed, and /proc.
type InstallPaths struct {
	Home   string
	Config string
	Units  string
	Hook   string
	Probe  string
	Binary string
	Proc   string
}

// HomeInstallPaths are Workshop's: ~/.loom/release.conf, ~/.config/systemd/user, ~/.loom/updated.d/60-release,
// ~/.loom/health.d/60-release, ~/.loom/bin/loom and /proc.
func HomeInstallPaths(home string) InstallPaths {
	return InstallPaths{Home: home, Config: filepath.Join(home, ".loom", "release.conf"), Units: filepath.Join(home, ".config", "systemd", "user"),
		Hook: filepath.Join(home, ".loom", "updated.d", HookName), Probe: filepath.Join(home, ".loom", "health.d", HookName),
		Binary: filepath.Join(home, ".loom", "bin", "loom"), Proc: "/proc"}
}

// ReadHomeConfig is release.conf over Workshop's defaults; found is false when there is none.
func ReadHomeConfig(home, path string) (Config, bool, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	config, err := ReadConfig(string(content), DefaultConfig(home))
	if err != nil {
		return Config{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return config, true, nil
}

// Install readies the watcher where release.conf says this machine releases, and does nothing anywhere else: the
// updater's hook runs it after every release, on every machine it was ever written to. It reads release.conf first,
// then writes the hook, the health probe (this machine's units, release.conf's units) and the unit, each only when its
// text changed; last it touches the watcher: started when it isn't running, restarted when its unit changed or it runs
// another binary than the updater installed, and otherwise left alone.
func Install(paths InstallPaths, systemctl serving.Systemctl, report io.Writer) error {
	config, found, err := ReadHomeConfig(paths.Home, paths.Config)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(report, "loom release install: no %s, so this machine doesn't release; nothing installed\n", paths.Config)
		return nil
	}
	if _, err := serving.WriteChanged(paths.Hook, hookText, 0o755); err != nil {
		return fmt.Errorf("the updater's hook: %w", err)
	}
	probe, err := updater.Probe(config.Units...)
	if err != nil {
		return err
	}
	if _, err := serving.WriteChanged(paths.Probe, probe, 0o755); err != nil {
		return fmt.Errorf("the updater's health probe: %w", err)
	}
	changed, err := serving.WriteChanged(filepath.Join(paths.Units, UnitName), unitText, 0o644)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(report, "loom release install: wrote %s\n", filepath.Join(paths.Units, UnitName))
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
	}
	if _, err := systemctl("enable", UnitName); err != nil {
		return err
	}
	answer, err := systemctl("show", "--property=MainPID", "--value", UnitName)
	if err != nil {
		return err
	}
	switch pid := strings.TrimSpace(answer); {
	case pid == "" || pid == "0":
		if _, err := systemctl("start", UnitName); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom release install: %s started\n", UnitName)
	case changed || !serving.SameFile(filepath.Join(paths.Proc, pid, "exe"), paths.Binary):
		if _, err := systemctl("restart", UnitName); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom release install: %s restarted on this release; the release in hand carries on from release.json\n", UnitName)
	default:
		fmt.Fprintf(report, "loom release install: %s already runs this unit and this release\n", UnitName)
	}
	return nil
}
