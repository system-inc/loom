package queuebridge

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/serving"
)

//go:embed systemd/loom-queue-bridge.service
var serviceText string

//go:embed systemd/loom-queue-bridge.timer
var timerText string

//go:embed updated.d/61-queue-bridge
var hookText string

// ServiceName and TimerName are the units' names under ~/.config/systemd/user; HookName is the updater's hook that runs
// `loom queue-bridge install` after every release (docs/updater.md).
const (
	ServiceName = "loom-queue-bridge.service"
	TimerName   = "loom-queue-bridge.timer"
	HookName    = "61-queue-bridge"
)

// Paths are what Install reads and writes: the machine's queue-bridge.conf, the systemd user unit directory and the hook.
type Paths struct {
	Config string
	Units  string
	Hook   string
}

// HomePaths are a machine's: ~/.loom/queue-bridge.conf, ~/.config/systemd/user and ~/.loom/updated.d/61-queue-bridge.
func HomePaths(home string) Paths {
	return Paths{Config: filepath.Join(home, ".loom", "queue-bridge.conf"), Units: filepath.Join(home, ".config", "systemd", "user"),
		Hook: filepath.Join(home, ".loom", "updated.d", HookName)}
}

// A Systemctl runs `systemctl --user <arguments>` and gives back what it printed.
type Systemctl func(arguments ...string) (string, error)

// Install readies the bridge on the machine queue-bridge.conf makes the bridge, so its units travel with the loom binary
// that runs them. queue-bridge.conf must be there and read; then the hook, the service and the timer are each written
// only when their text changed, and systemd told when a unit did. Install never enables, starts or stops anything:
// whether the bridge runs is Kirk's call (`systemctl --user enable --now loom-queue-bridge.timer`), and a timer already
// enabled runs the release's `loom queue-bridge` on its next tick, since each pass starts the binary afresh.
//
// Last, it preflights what a pass reads (Preflight) and fails naming every gap, so the updater's hook fails, the box's
// report says its hooks didn't pass, and the release says so (#18kj26x), rather than the pass failing quietly in its log.
func Install(paths Paths, home string, now time.Time, systemctl Systemctl, report io.Writer) error {
	content, err := os.ReadFile(paths.Config)
	if err != nil {
		return fmt.Errorf("this machine's queue-bridge settings, whose being there makes it the bridge: %w", err)
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
		fmt.Fprintf(report, "loom queue-bridge install: wrote %s and %s in %s, reading %s; the timer is left as it was\n", ServiceName, TimerName, paths.Units, config.Repository)
	} else {
		fmt.Fprintf(report, "loom queue-bridge install: %s and %s are this release's already\n", ServiceName, TimerName)
	}
	gaps, notes := Preflight(config, now)
	for _, note := range notes {
		fmt.Fprintf(report, "loom queue-bridge install: %s\n", note)
	}
	if len(gaps) > 0 {
		return fmt.Errorf("a pass can't run on this machine: %s", strings.Join(gaps, "; "))
	}
	return nil
}

// TokenWarning is how long before the token's expiry the preflight names it: under a week, it's renewed in time.
const TokenWarning = 7 * 24 * time.Hour

// Preflight is every file a pass reads that isn't fit (gaps) and what should be renewed soon (notes): the bridge's token
// (there, a coordinator token, not expired; named under TokenWarning), and its clone (there, an origin and own settings
// a keyless pass takes, Facts's own checks).
func Preflight(config Config, now time.Time) ([]string, []string) {
	gaps, notes := []string{}, []string{}
	held, err := os.ReadFile(config.Token)
	switch {
	case err != nil:
		gaps = append(gaps, fmt.Sprintf("its token %s: %v (loom coordinator-token queue-bridge --days N > %s)", config.Token, err, config.Token))
	default:
		claims, err := protocol.ClaimsOf(string(held))
		switch {
		case err != nil:
			gaps = append(gaps, fmt.Sprintf("its token %s: %v", config.Token, err))
		case claims.Scope != protocol.ScopeCoordinator:
			gaps = append(gaps, fmt.Sprintf("its token %s is a %s token, not a coordinator one", config.Token, claims.Scope))
		case now.Unix() >= claims.Expires:
			gaps = append(gaps, fmt.Sprintf("its token %s expired at %s", config.Token, time.Unix(claims.Expires, 0).UTC().Format(time.RFC3339)))
		case time.Unix(claims.Expires, 0).Sub(now) < TokenWarning:
			notes = append(notes, fmt.Sprintf("its token %s expires at %s: mint another before then", config.Token, time.Unix(claims.Expires, 0).UTC().Format(time.RFC3339)))
		}
	}
	clone := Clone{Repository: config.Repository}
	if _, err := clone.git([]string{"rev-parse", "--git-dir"}); err != nil {
		gaps = append(gaps, fmt.Sprintf("its clone %s: %v (git clone --bare https://github.com/system-inc/adamic.git %s)", config.Repository, err, config.Repository))
	} else if err := clone.keylessClone(); err != nil {
		gaps = append(gaps, fmt.Sprintf("its clone %s: %v", config.Repository, err))
	}
	return gaps, notes
}
