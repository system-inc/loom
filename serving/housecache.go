package serving

import (
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/system-inc/loom/housecache"
)

// The house cache (docs/house-cache.md) runs on whichever box holds ~/.loom/house-cache.conf: `loom house-cache
// install`, which the updater's hook ~/.loom/updated.d/40-house-cache runs after every release, renders
// loom-house-cache.service from it, and starts or restarts the cache. On a box without the file it stops and removes
// the unit and the hook, so moving the house cache is the file moving, an install on each box, and the clients'
// house-cache line.

//go:embed systemd/loom-house-cache.service
var houseCacheUnitTemplate string

//go:embed updated.d/40-house-cache
var houseCacheHookText string

// HouseCacheUnitName is the house cache's unit under ~/.config/systemd/user.
const HouseCacheUnitName = "loom-house-cache.service"

// HouseCacheHookName is the updater's hook that runs `loom house-cache install` after every release.
const HouseCacheHookName = "40-house-cache"

// A HouseCacheConfig is ~/.loom/house-cache.conf: the address the cache listens on, where it keeps its blobs, how many
// gigabytes it holds at most, how many it keeps free on its disk, and whether a tailnet's or a public address is
// allowed.
type HouseCacheConfig struct {
	Listen    string
	Directory string
	LimitGB   uint64
	FloorGB   uint64
	Public    bool
	Tailnet   bool
}

// Defaults: 100 GB of blobs, about 27 cold trees at 3.7 GB each, and 20 GB always free beside them.
const (
	DefaultHouseCacheLimitGB = 100
	DefaultHouseCacheFloorGB = 20
)

// ReadHouseCacheConfig reads house-cache.conf as ReadConfig reads serve.conf: listen (required, <ip>:<port> on a local
// network unless public = yes), directory (an absolute path, default ~/loom-house-cache), limit-gb and floor-gb. Any
// other key or line is refused.
func ReadHouseCacheConfig(content string) (HouseCacheConfig, error) {
	config := HouseCacheConfig{LimitGB: DefaultHouseCacheLimitGB, FloorGB: DefaultHouseCacheFloorGB}
	seen := map[string]bool{}
	for number, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found || seen[key] {
			return HouseCacheConfig{}, fmt.Errorf("house-cache.conf line %d: %q isn't a key = value line of its own", number+1, line)
		}
		seen[key] = true
		switch key {
		case "listen":
			config.Listen = value
		case "directory":
			if !filepath.IsAbs(value) || filepath.Clean(value) != value || strings.ContainsAny(value, " \t\"'\\%$") {
				return HouseCacheConfig{}, fmt.Errorf("house-cache.conf line %d: directory %q isn't a plain absolute path", number+1, value)
			}
			config.Directory = value
		case "limit-gb", "floor-gb":
			gigabytes, err := strconv.ParseUint(value, 10, 32)
			if err != nil || (key == "limit-gb" && gigabytes == 0) {
				return HouseCacheConfig{}, fmt.Errorf("house-cache.conf line %d: %s is a whole number of gigabytes, not %q", number+1, key, value)
			}
			if key == "limit-gb" {
				config.LimitGB = gigabytes
			} else {
				config.FloorGB = gigabytes
			}
		case "public", "tailnet":
			switch value {
			case "yes":
				if key == "public" {
					config.Public = true
				} else {
					config.Tailnet = true
				}
			case "no":
			default:
				return HouseCacheConfig{}, fmt.Errorf("house-cache.conf line %d: %s is yes or no, not %q", number+1, key, value)
			}
		default:
			return HouseCacheConfig{}, fmt.Errorf("house-cache.conf line %d: no setting %q (listen, directory, limit-gb, floor-gb, tailnet, public)", number+1, key)
		}
	}
	if config.Listen == "" {
		return HouseCacheConfig{}, errors.New("house-cache.conf names no listen address")
	}
	if err := housecache.CheckListen(config.Listen, config.Public, config.Tailnet); err != nil {
		return HouseCacheConfig{}, fmt.Errorf("house-cache.conf: %w", err)
	}
	return config, nil
}

// HouseCacheUnit is loom-house-cache.service for this config: the template with its ExecStart line's settings filled
// in, and nothing else changed.
func HouseCacheUnit(config HouseCacheConfig) string {
	directory := config.Directory
	if directory == "" {
		directory = "%h/loom-house-cache"
	}
	flags := fmt.Sprintf("--listen %s --directory %s --limit-gb %d --floor-gb %d", config.Listen, directory, config.LimitGB, config.FloorGB)
	if config.Tailnet {
		flags += " --tailnet"
	}
	if config.Public {
		flags += " --public"
	}
	lines := strings.Split(houseCacheUnitTemplate, "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "ExecStart=") {
			lines[index] = strings.Replace(line, " SETTINGS", " "+flags, 1)
		}
	}
	return strings.Join(lines, "\n")
}

// HouseCachePaths are what InstallHouseCache reads and writes: the box's house-cache.conf, the systemd user unit
// directory, the updater's hook, the loom binary the updater installed, and /proc.
type HouseCachePaths struct {
	Config string
	Units  string
	Hook   string
	Binary string
	Proc   string
}

// HouseCacheHomePaths are a box's: ~/.loom/house-cache.conf, ~/.config/systemd/user, ~/.loom/updated.d/40-house-cache,
// ~/.loom/bin/loom and /proc.
func HouseCacheHomePaths(home string) HouseCachePaths {
	return HouseCachePaths{Config: filepath.Join(home, ".loom", "house-cache.conf"), Units: filepath.Join(home, ".config", "systemd", "user"),
		Hook: filepath.Join(home, ".loom", "updated.d", HouseCacheHookName), Binary: filepath.Join(home, ".loom", "bin", "loom"), Proc: "/proc"}
}

// InstallHouseCache readies the house cache on a box that hosts it, as Install readies serve: the config read and
// checked first, then the hook and the unit, each written only when its text changed, and only then the cache touched:
// started when it isn't running, restarted when its unit changed or it runs another binary than the one the updater
// installed, and otherwise left alone. A restart costs its clients nothing but a read of the store for what they ask
// meanwhile. On a box without house-cache.conf the cache is stopped and its unit and hook removed, so a box that no
// longer hosts serves nothing.
func InstallHouseCache(paths HouseCachePaths, systemctl Systemctl, report io.Writer) error {
	content, err := os.ReadFile(paths.Config)
	if errors.Is(err, os.ErrNotExist) {
		return removeHouseCache(paths, systemctl, report)
	}
	if err != nil {
		return fmt.Errorf("this box's house cache settings: %w", err)
	}
	config, err := ReadHouseCacheConfig(string(content))
	if err != nil {
		return fmt.Errorf("%s: %w", paths.Config, err)
	}
	if _, err := WriteChanged(paths.Hook, houseCacheHookText, 0o755); err != nil {
		return fmt.Errorf("the updater's hook: %w", err)
	}
	unit := filepath.Join(paths.Units, HouseCacheUnitName)
	changed, err := WriteChanged(unit, HouseCacheUnit(config), 0o644)
	if err != nil {
		return err
	}
	if changed {
		fmt.Fprintf(report, "loom house-cache install: wrote %s, listening on %s\n", unit, config.Listen)
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
	}
	if _, err := systemctl("enable", HouseCacheUnitName); err != nil {
		return err
	}
	answer, err := systemctl("show", "--property=MainPID", "--value", HouseCacheUnitName)
	if err != nil {
		return err
	}
	pid := strings.TrimSpace(answer)
	switch {
	case pid == "" || pid == "0":
		if _, err := systemctl("start", HouseCacheUnitName); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom house-cache install: %s started\n", HouseCacheUnitName)
	case changed || !SameFile(filepath.Join(paths.Proc, pid, "exe"), paths.Binary):
		if _, err := systemctl("restart", HouseCacheUnitName); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom house-cache install: %s restarted on this release\n", HouseCacheUnitName)
	default:
		fmt.Fprintf(report, "loom house-cache install: %s already runs this unit and this release\n", HouseCacheUnitName)
	}
	return nil
}

// removeHouseCache stops the house cache on a box that no longer hosts it and removes its unit, then its hook.
func removeHouseCache(paths HouseCachePaths, systemctl Systemctl, report io.Writer) error {
	unit := filepath.Join(paths.Units, HouseCacheUnitName)
	if _, err := os.Stat(unit); err == nil {
		if _, err := systemctl("disable", "--now", HouseCacheUnitName); err != nil {
			return err
		}
		if err := os.Remove(unit); err != nil {
			return err
		}
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom house-cache install: no %s, so this box hosts no house cache: %s stopped and removed\n", paths.Config, HouseCacheUnitName)
	}
	if err := os.Remove(paths.Hook); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
