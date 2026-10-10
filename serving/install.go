// Package serving keeps a Linux box serving its pool (docs/serving.md): loom-serve.service, the systemd user unit
// that runs `loom-runner serve` with the box's pool and token, and Install, which `loom-runner install-serve` runs from
// the updater's hook after every release, so the unit travels with the runner it runs.
package serving

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/system-inc/loom/protocol"
)

//go:embed systemd/loom-serve.service
var unitTemplate string

// UnitName is the unit's name under ~/.config/systemd/user.
const UnitName = "loom-serve.service"

// A Config is ~/.loom/serve.conf: the pool this box serves, and whether it also takes phase jobs (box-phase's
// workers do, through run.py at the gate tools' commit). Every box worker serves --strict: the placer sends a pool
// nothing but test jobs.
type Config struct {
	Pool      string
	PhaseJobs bool
}

// ReadConfig reads serve.conf as the updater reads update.conf: key = value lines, # comments, blank lines skipped.
// pool is required and is a name the wire takes; phase-jobs is yes or no, no when absent. Any other key or line is
// refused, so a typo never serves the wrong pool quietly.
func ReadConfig(content string) (Config, error) {
	config := Config{}
	seen := map[string]bool{}
	for number, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found || seen[key] {
			return Config{}, fmt.Errorf("serve.conf line %d: %q isn't a key = value line of its own", number+1, line)
		}
		seen[key] = true
		switch key {
		case "pool":
			if !protocol.RunIdPattern.MatchString(value) {
				return Config{}, fmt.Errorf("serve.conf line %d: pool %q isn't a pool name (letters, digits, dot, dash, underscore)", number+1, value)
			}
			config.Pool = value
		case "phase-jobs":
			switch value {
			case "yes":
				config.PhaseJobs = true
			case "no":
			default:
				return Config{}, fmt.Errorf("serve.conf line %d: phase-jobs is yes or no, not %q", number+1, value)
			}
		default:
			return Config{}, fmt.Errorf("serve.conf line %d: no setting %q (pool, phase-jobs)", number+1, key)
		}
	}
	if config.Pool == "" {
		return Config{}, errors.New("serve.conf names no pool")
	}
	return config, nil
}

// Unit is loom-serve.service for this config: the template with its ExecStart line's POOL and FLAGS filled in, and
// nothing else changed.
func Unit(config Config) string {
	flags := "--strict"
	if config.PhaseJobs {
		flags += " --phase-jobs"
	}
	lines := strings.Split(unitTemplate, "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "ExecStart=") {
			line = strings.Replace(line, " FLAGS ", " "+flags+" ", 1)
			lines[index] = strings.Replace(line, "/pools/POOL ", "/pools/"+config.Pool+" ", 1)
		}
	}
	return strings.Join(lines, "\n")
}

// Paths are where Install reads and writes: the box's serve.conf and pool token, and the systemd user unit directory.
type Paths struct {
	Config string
	Token  string
	Units  string
}

// HomePaths are a box's: ~/.loom/serve.conf, ~/.loom/serve-token and ~/.config/systemd/user.
func HomePaths(home string) Paths {
	return Paths{Config: filepath.Join(home, ".loom", "serve.conf"), Token: filepath.Join(home, ".loom", "serve-token"),
		Units: filepath.Join(home, ".config", "systemd", "user")}
}

// Install readies loom-serve.service and restarts it on the runner now installed: it reads serve.conf, refuses a
// token file that is missing, empty, or readable by anyone but its owner, writes the unit only when its text changed
// (then reloads systemd's view of it), enables it, and asks systemd to reload it, which drains a running serve so the
// unit in hand finishes before Restart=always starts the new runner, or starts a stopped one. systemctl runs
// `systemctl --user <arguments>`. Run again with nothing changed, it only drains and restarts serve, so a hook the
// updater runs twice for one release costs nothing but a restart.
func Install(paths Paths, systemctl func(arguments ...string) error, report io.Writer) error {
	content, err := os.ReadFile(paths.Config)
	if err != nil {
		return fmt.Errorf("this box's serve settings: %w", err)
	}
	config, err := ReadConfig(string(content))
	if err != nil {
		return fmt.Errorf("%s: %w", paths.Config, err)
	}
	token, err := os.Stat(paths.Token)
	switch {
	case err != nil:
		return fmt.Errorf("the pool token: %w", err)
	case !token.Mode().IsRegular() || token.Size() == 0:
		return fmt.Errorf("the pool token %s isn't a file holding a token", paths.Token)
	case token.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("the pool token %s is mode %o: anyone but its owner can read it (chmod 600)", paths.Token, token.Mode().Perm())
	}
	unit := Unit(config)
	path := filepath.Join(paths.Units, UnitName)
	if held, err := os.ReadFile(path); err != nil || string(held) != unit {
		if err := os.MkdirAll(paths.Units, 0o755); err != nil {
			return err
		}
		// Written beside its name and renamed over it, so systemd never reads half a unit.
		partial := path + ".partial"
		if err := os.WriteFile(partial, []byte(unit), 0o644); err != nil {
			return err
		}
		if err := os.Rename(partial, path); err != nil {
			os.Remove(partial)
			return err
		}
		fmt.Fprintf(report, "loom-runner install-serve: wrote %s, serving pool %s\n", path, config.Pool)
		if err := systemctl("daemon-reload"); err != nil {
			return err
		}
	}
	if err := systemctl("enable", UnitName); err != nil {
		return err
	}
	if err := systemctl("reload-or-restart", UnitName); err != nil {
		return err
	}
	fmt.Fprintf(report, "loom-runner install-serve: %s reloaded: serve drains the unit in hand and starts again on this release\n", UnitName)
	return nil
}
