package queuebridge

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/serving"
)

//go:embed systemd/loom-queue-bridge.service
var serviceText string

//go:embed systemd/loom-queue-bridge.timer
var timerText string

//go:embed launchd/com.loom.queue-bridge.plist
var agentText string

//go:embed updated.d/61-queue-bridge
var hookText string

// ServiceName and TimerName are the systemd units under ~/.config/systemd/user, AgentName the launchd agent under
// ~/Library/LaunchAgents, and HookName the updater's hook that runs `loom queue-bridge install` after every release
// (docs/updater.md).
const (
	ServiceName = "loom-queue-bridge.service"
	TimerName   = "loom-queue-bridge.timer"
	AgentName   = "com.loom.queue-bridge.plist"
	HookName    = "61-queue-bridge"
)

// Paths are what Install reads and writes: the machine's queue-bridge.conf, the systemd user unit directory, the
// launchd agent directory and the hook.
type Paths struct {
	Config string
	Units  string
	Agents string
	Hook   string
}

// HomePaths are a machine's: ~/.loom/queue-bridge.conf, ~/.config/systemd/user, ~/Library/LaunchAgents and
// ~/.loom/updated.d/61-queue-bridge.
func HomePaths(home string) Paths {
	return Paths{Config: filepath.Join(home, ".loom", "queue-bridge.conf"), Units: filepath.Join(home, ".config", "systemd", "user"),
		Agents: filepath.Join(home, "Library", "LaunchAgents"), Hook: filepath.Join(home, ".loom", "updated.d", HookName)}
}

// A Systemctl runs `systemctl --user <arguments>` and gives back what it printed.
type Systemctl func(arguments ...string) (string, error)

// Install readies the bridge on the machine queue-bridge.conf makes the bridge, so its units travel with the loom
// binary that runs them: on Linux (goos "linux") the systemd service and timer, systemd told when one changed, and on
// macOS ("darwin") the launchd agent with the home written in. queue-bridge.conf must be there and read; each file is
// written only when its text changed. Install never enables, starts, loads or stops anything: whether the bridge runs is
// Kirk's call, and one already running runs the release's `loom queue-bridge` on its next tick, since each pass starts
// the binary afresh.
func Install(paths Paths, home string, goos string, systemctl Systemctl, report io.Writer) error {
	content, err := os.ReadFile(paths.Config)
	if err != nil {
		return fmt.Errorf("this machine's queue-bridge settings, whose being there makes it the bridge: %w", err)
	}
	config, err := ReadConfig(string(content), home)
	if err != nil {
		return fmt.Errorf("%s: %w", paths.Config, err)
	}
	if goos != "linux" && goos != "darwin" {
		return fmt.Errorf("the bridge runs under systemd or launchd, and this is %s", goos)
	}
	if _, err := serving.WriteChanged(paths.Hook, hookText, 0o755); err != nil {
		return fmt.Errorf("the updater's hook: %w", err)
	}
	deciding := map[bool]string{true: "deciding", false: "carrying git's facts only"}[config.Decides]
	if goos == "darwin" {
		wrote, err := serving.WriteChanged(filepath.Join(paths.Agents, AgentName), strings.ReplaceAll(agentText, "HOME_DIRECTORY", home), 0o644)
		if err != nil {
			return err
		}
		if wrote {
			fmt.Fprintf(report, "loom queue-bridge install: wrote %s in %s, %s; launchd is left as it was\n", AgentName, paths.Agents, deciding)
		} else {
			fmt.Fprintf(report, "loom queue-bridge install: %s is this release's already\n", AgentName)
		}
		return nil
	}
	changed := false
	for name, text := range map[string]string{ServiceName: serviceText, TimerName: timerText} {
		wrote, err := serving.WriteChanged(filepath.Join(paths.Units, name), text, 0o644)
		if err != nil {
			return err
		}
		changed = changed || wrote
	}
	if changed {
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom queue-bridge install: wrote %s and %s in %s, %s; the timer is left as it was\n", ServiceName, TimerName, paths.Units, deciding)
	} else {
		fmt.Fprintf(report, "loom queue-bridge install: %s and %s are this release's already\n", ServiceName, TimerName)
	}
	return nil
}
