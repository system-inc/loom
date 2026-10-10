package lander

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/serving"
)

//go:embed systemd/loom-pusher.service
var serviceText string

//go:embed systemd/loom-pusher.timer
var timerText string

//go:embed updated.d/60-push
var hookText string

// ServiceName and TimerName are the units' names under ~/.config/systemd/user; HookName is the updater's hook that runs
// `loom push install` after every release (docs/updater.md).
const (
	ServiceName = "loom-pusher.service"
	TimerName   = "loom-pusher.timer"
	HookName    = "60-push"
)

// Paths are what Install reads and writes: the machine's push.conf, the systemd user unit directory and the hook.
type Paths struct {
	Config string
	Units  string
	Hook   string
}

// HomePaths are a machine's: ~/.loom/push.conf, ~/.config/systemd/user and ~/.loom/updated.d/60-push.
func HomePaths(home string) Paths {
	return Paths{Config: filepath.Join(home, ".loom", "push.conf"), Units: filepath.Join(home, ".config", "systemd", "user"),
		Hook: filepath.Join(home, ".loom", "updated.d", HookName)}
}

// A Systemctl runs `systemctl --user <arguments>` and gives back what it printed.
type Systemctl func(arguments ...string) (string, error)

// Install readies the pusher on the machine push.conf makes the lander, so its units travel with the loom binary that
// runs them. push.conf must be there and read; then the hook, the service and the timer are each written only when
// their text changed, and systemd told when a unit did. Install never enables, starts or stops anything: whether the
// timer runs is Kirk's call (`systemctl --user enable --now loom-pusher.timer`, enable so a reboot keeps it), and a
// timer already enabled runs the release's `loom push` on its next tick, since each pass starts the binary afresh.
//
// Last, it preflights what a pass reads (Preflight) and fails naming every gap, so the updater's hook fails, the box's
// report says its hooks didn't pass, and the release says so (#18kj26x), rather than a landing failing in its log.
func Install(paths Paths, home string, systemctl Systemctl, report io.Writer) error {
	content, err := os.ReadFile(paths.Config)
	if err != nil {
		return fmt.Errorf("this machine's push settings, whose being there makes it the lander: %w", err)
	}
	config, err := ReadConfig(string(content), home)
	if err != nil {
		return fmt.Errorf("%s: %w", paths.Config, err)
	}
	if _, err := serving.WriteChanged(paths.Hook, hookText, 0o755); err != nil {
		return fmt.Errorf("the updater's hook: %w", err)
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
		fmt.Fprintf(report, "loom push install: wrote %s and %s in %s, landing on %s; the timer is left as it was\n", ServiceName, TimerName, paths.Units, config.Branch)
	} else {
		fmt.Fprintf(report, "loom push install: %s and %s are this release's already\n", ServiceName, TimerName)
	}
	if gaps := Preflight(config); len(gaps) > 0 {
		return fmt.Errorf("a pass can't run on this machine: %s", strings.Join(gaps, "; "))
	}
	return nil
}

// Preflight is every file a pass reads that isn't fit: the token secret each call mints from, and the lander's clone
// with its origin (the key behind it is GitHub's to judge, on the first push).
func Preflight(config Config) []string {
	gaps := []string{}
	if _, err := protocol.ReadTokenSecret(config.Secret); err != nil {
		gaps = append(gaps, fmt.Sprintf("its token secret %s: %v", config.Secret, err))
	}
	if origin, stderr, err := Git(config.Repository, nil, "remote", "get-url", "origin"); err != nil || strings.TrimSpace(origin) == "" {
		gaps = append(gaps, fmt.Sprintf("its clone %s has no origin: %v %s (git clone --bare git@github-lander:system-inc/adamic.git %s)", config.Repository, err, strings.TrimSpace(stderr), config.Repository))
	}
	return gaps
}
