// Package daemons installs Workshop's daemons' systemd user units from the release a machine runs (`loom
// install-units`, run by the updater's hook updated.d/30-units after every release), so a unit never drifts from the
// binary it starts. Oct 10 (#pzrz9r8): the units were installed by hand, and loom-build-trees ran an old one with
// KillMode=control-group, so every release killed the tree build in flight, though the repo's unit said
// KillMode=process; drop-ins replaced ExecStart on the placer and the judge, and the judge's unit wasn't in the repo.
//
// It installs the planner's, the placer's, the tree builder's and the judge's units, each only on a machine holding that
// daemon's token (~/.loom/<name>-token), which is what makes a machine run it. The pusher's and the Queue bridge's
// units come from their own installs (`loom push install`, `loom queue-bridge install`), the house cache's from `loom
// house-cache install`. Install only writes units and tells systemd: it never enables, starts, stops or restarts
// anything, so each daemon's own hook or its next start runs the new unit, and the pusher's timer, which Kirk switches
// on by hand, is never touched (docs/cutover.md).
package daemons

import (
	_ "embed"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/judge"
	"github.com/system-inc/loom/placer"
	"github.com/system-inc/loom/planner"
	"github.com/system-inc/loom/serving"
	"github.com/system-inc/loom/treebuilder"
)

//go:embed updated.d/30-units
var hookText string

// HookName is the updater's hook that runs `loom install-units` after every release (docs/updater.md).
const HookName = "30-units"

// A Daemon is one of Workshop's daemons: its unit's name and text as this release has it, and the token whose being on a
// machine makes that machine run it.
type Daemon struct {
	Unit  string
	Text  string
	Token string
}

// Daemons are the units Install writes, in this order.
var Daemons = []Daemon{
	{Unit: planner.ServiceName, Text: planner.ServiceText, Token: "planner-token"},
	{Unit: placer.ServiceName, Text: placer.ServiceText, Token: "placer-token"},
	{Unit: treebuilder.ServiceName, Text: treebuilder.ServiceText, Token: "build-trees-token"},
	{Unit: judge.ServiceName, Text: judge.ServiceText, Token: "judge-token"},
}

// Paths are what Install reads and writes: the machine's ~/.loom (its tokens), the systemd user unit directory and the
// hook.
type Paths struct {
	Loom  string
	Units string
	Hook  string
}

// HomePaths are a machine's: ~/.loom, ~/.config/systemd/user and ~/.loom/updated.d/30-units.
func HomePaths(home string) Paths {
	return Paths{Loom: filepath.Join(home, ".loom"), Units: filepath.Join(home, ".config", "systemd", "user"),
		Hook: filepath.Join(home, ".loom", "updated.d", HookName)}
}

// A Systemctl runs `systemctl --user <arguments>` and gives back what it printed.
type Systemctl func(arguments ...string) (string, error)

// Install writes the hook, then each daemon's unit on a machine holding its token, each only when its text changed (a
// unit by hand, a stale one or a missing one is written; one already this release's is left), and tells systemd once
// when any did. It reports what it wrote and what it passed over. It calls systemctl for daemon-reload alone.
func Install(paths Paths, systemctl Systemctl, report io.Writer) error {
	if _, err := serving.WriteChanged(paths.Hook, hookText, 0o755); err != nil {
		return fmt.Errorf("the updater's hook: %w", err)
	}
	wrote, kept, absent := []string{}, []string{}, []string{}
	for _, daemon := range Daemons {
		if _, err := os.Stat(filepath.Join(paths.Loom, daemon.Token)); err != nil {
			absent = append(absent, daemon.Unit)
			continue
		}
		changed, err := serving.WriteChanged(filepath.Join(paths.Units, daemon.Unit), daemon.Text, 0o644)
		if err != nil {
			return fmt.Errorf("%s: %w", daemon.Unit, err)
		}
		if changed {
			wrote = append(wrote, daemon.Unit)
		} else {
			kept = append(kept, daemon.Unit)
		}
	}
	if len(wrote) > 0 {
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom install-units: wrote %s; nothing enabled, started or restarted\n", strings.Join(wrote, ", "))
	}
	if len(kept) > 0 {
		fmt.Fprintf(report, "loom install-units: %s already this release's\n", strings.Join(kept, ", "))
	}
	if len(absent) > 0 {
		fmt.Fprintf(report, "loom install-units: no token here for %s, so not installed\n", strings.Join(absent, ", "))
	}
	return nil
}
