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
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"github.com/system-inc/loom/housecache"
	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/toolchains"
	"github.com/system-inc/loom/updater"
)

//go:embed systemd/loom-serve.service
var unitTemplate string

// UnitName is the unit's name under ~/.config/systemd/user.
const UnitName = "loom-serve.service"

// A Config is ~/.loom/serve.conf: the pool this box serves, whether it also takes phase jobs (box-phase's workers do,
// through run.py at the gate tools' commit), and how many units it runs at once (#ef2rgaq: a 64-thread box holds
// several). Every box worker serves --strict: the placer sends a pool nothing but test jobs.
type Config struct {
	Pool      string
	PhaseJobs bool
	// Has is the toolchains this box claims for its pool, which serve probes before it asks for anything (--has).
	Has []string
	// Units is serve's --units, the most units at once; 1 when absent.
	Units int
}

// ReadConfig reads serve.conf as the updater reads update.conf: key = value lines, # comments, blank lines skipped.
// pool is required and is a name the wire takes; phase-jobs is yes or no, no when absent. has is the toolchains this box
// claims, go,clang,node,wasiSdk, each one a probe exists for (package toolchains), none when absent; units is a whole
// number from 1 to 64, 1 when absent. Any other key or line is refused, so a typo never serves the wrong pool quietly.
func ReadConfig(content string) (Config, error) {
	config := Config{Units: 1}
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
		case "has":
			claims, err := toolchains.Parse(value)
			if err != nil {
				return Config{}, fmt.Errorf("serve.conf line %d: has: %w", number+1, err)
			}
			config.Has = claims
		case "units":
			units, err := strconv.Atoi(value)
			if err != nil || units < 1 || units > 64 || strconv.Itoa(units) != value {
				return Config{}, fmt.Errorf("serve.conf line %d: units is a whole number from 1 to 64, not %q", number+1, value)
			}
			config.Units = units
		default:
			return Config{}, fmt.Errorf("serve.conf line %d: no setting %q (pool, phase-jobs, has, units)", number+1, key)
		}
	}
	if config.Pool == "" {
		return Config{}, errors.New("serve.conf names no pool")
	}
	return config, nil
}

// Unit is loom-serve.service for this config, worker name and house cache (empty for none): the template with its
// ExecStart line's POOL, FLAGS and WORKER filled in, and nothing else changed.
func Unit(config Config, worker, houseCache string) string {
	flags := "--strict"
	if config.PhaseJobs {
		flags += " --phase-jobs"
	}
	if houseCache != "" {
		flags += " --house-cache " + houseCache
	}
	if len(config.Has) > 0 {
		flags += " --has " + strings.Join(config.Has, ",")
	}
	if config.Units > 1 {
		flags += " --units " + strconv.Itoa(config.Units)
	}
	lines := strings.Split(unitTemplate, "\n")
	for index, line := range lines {
		if strings.HasPrefix(line, "ExecStart=") {
			line = strings.Replace(line, " FLAGS ", " "+flags+" ", 1)
			line = strings.Replace(line, " --worker WORKER ", " --worker "+worker+" ", 1)
			lines[index] = strings.Replace(line, "/pools/POOL ", "/pools/"+config.Pool+" ", 1)
		}
	}
	return strings.Join(lines, "\n")
}

// extraNamePattern is what a box's further pool may be called in its file names (serve-<name>.conf): a short word, so
// its unit, token, directories and worker name read as the box's own serve's with the name added.
var extraNamePattern = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// An Extra is one more pool a box serves beside serve.conf's (Oct 10: Chonchon, box-phase's only worker, went down,
// and Home and Cloud took box-phase beside box-strict). ~/.loom/serve-<name>.conf names it in serve.conf's own
// settings, ~/.loom/serve-token-<name> is its token, and it runs as loom-serve-<name>.service, with its own runtime
// directory, root and workspace under ~/loom-serve-<name>, as worker <host>-<id>-<name>.
type Extra struct {
	Name   string
	Config Config
}

// ExtraUnitName is the unit an extra pool's serve runs as.
func ExtraUnitName(name string) string {
	return "loom-serve-" + name + ".service"
}

// ExtraUnit is Unit for an extra pool: the same serve with the name added to its worker, its token, its runtime
// directory, and its root and workspace, so two serves on one box share nothing a unit writes.
func ExtraUnit(extra Extra, worker, houseCache string) string {
	unit := Unit(extra.Config, worker+"-"+extra.Name, houseCache)
	for _, swap := range [][2]string{
		{"RuntimeDirectory=loom-serve\n", "RuntimeDirectory=loom-serve-" + extra.Name + "\n"},
		{"%h/.loom/serve-token ", "%h/.loom/serve-token-" + extra.Name + " "},
		{"%t/loom-serve/", "%t/loom-serve-" + extra.Name + "/"},
		{"%h/loom-serve/", "%h/loom-serve-" + extra.Name + "/"},
	} {
		unit = strings.ReplaceAll(unit, swap[0], swap[1])
	}
	return strings.Replace(unit, "Description=Loom serve:", "Description=Loom serve for "+extra.Config.Pool+":", 1)
}

// readExtras reads every serve-<name>.conf beside serve.conf, each with its token checked as serve.conf's is, in name
// order. A file whose name isn't a name an extra may have is refused, so a stray file never serves a pool quietly.
func readExtras(paths Paths) ([]Extra, error) {
	found, err := filepath.Glob(filepath.Join(filepath.Dir(paths.Config), "serve-*.conf"))
	if err != nil {
		return nil, err
	}
	sort.Strings(found)
	extras := []Extra{}
	for _, path := range found {
		name := strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "serve-"), ".conf")
		if !extraNamePattern.MatchString(name) {
			return nil, fmt.Errorf("%s: an extra pool's name is 1 to 16 lowercase letters and digits, not %q", path, name)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		config, err := ReadConfig(string(content))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if err := checkToken(filepath.Join(filepath.Dir(paths.Token), "serve-token-"+name), paths.User); err != nil {
			return nil, err
		}
		extras = append(extras, Extra{Name: name, Config: config})
	}
	return extras, nil
}

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,62}$`)
var machineIdPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// WorkerName is the name serve gives its pool: the host's name, to its first dot, and the first six digits of its
// machine id, so two boxes that happen to share a host name never read as one worker.
func WorkerName(host, machineId string) (string, error) {
	host, _, _ = strings.Cut(host, ".")
	machineId = strings.TrimSpace(machineId)
	if !hostPattern.MatchString(host) {
		return "", fmt.Errorf("the host name %q isn't a worker's name (letters, digits, dash, underscore)", host)
	}
	if !machineIdPattern.MatchString(machineId) {
		return "", fmt.Errorf("the machine id %q isn't 32 lowercase hex digits", machineId)
	}
	return host + "-" + machineId[:6], nil
}

// Paths are what Install reads and writes: the box's serve.conf and pool token, the updater's update.conf (for its
// house-cache line), the systemd user unit directory, the updater's hook and health probe, the serving binary the
// updater installed, and the host's name, machine id, user and /proc.
type Paths struct {
	Config       string
	UpdateConfig string
	Token        string
	Units        string
	Hook         string
	ClockHook    string
	Probe        string
	Binary       string
	Host         string
	MachineId    string
	User         int
	Proc         string
}

// HomePaths are a box's: ~/.loom/serve.conf, ~/.loom/serve-token, ~/.config/systemd/user, ~/.loom/updated.d/50-serve,
// ~/.loom/health.d/50-serve and ~/.loom/bin/loom-runner, this host, /etc/machine-id, this user and /proc.
func HomePaths(home string) Paths {
	host, _ := os.Hostname()
	return Paths{Config: filepath.Join(home, ".loom", "serve.conf"), UpdateConfig: filepath.Join(home, ".loom", "update.conf"), Token: filepath.Join(home, ".loom", "serve-token"),
		Units: filepath.Join(home, ".config", "systemd", "user"), Hook: filepath.Join(home, ".loom", "updated.d", HookName),
		ClockHook: filepath.Join(home, ".loom", "updated.d", ClockHookName),
		Probe:     filepath.Join(home, ".loom", "health.d", HookName),
		Binary:    filepath.Join(home, ".loom", "bin", "loom-runner"), Host: host, MachineId: "/etc/machine-id", User: os.Getuid(), Proc: "/proc"}
}

//go:embed updated.d/50-serve
var hookText string

// HookName is the updater's hook that runs install-serve after every release (docs/updater.md).
const HookName = "50-serve"

//go:embed updated.d/10-clock
var clockHookText string

// ClockHookName is the updater's hook that keeps a WSL box's clock to Hyper-V's time sync alone (updated.d/10-clock, #nmx30ay).
const ClockHookName = "10-clock"

// A Systemctl runs `systemctl --user <arguments>` and gives back what it printed.
type Systemctl func(arguments ...string) (string, error)

// UserSystemctl is the user's own systemd: systemctl --user, its complaints to stderr.
func UserSystemctl(stderr io.Writer) Systemctl {
	return func(arguments ...string) (string, error) {
		command := exec.Command("systemctl", append([]string{"--user"}, arguments...)...)
		command.Stderr = stderr
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("systemctl --user %s: %w", strings.Join(arguments, " "), err)
		}
		return string(output), nil
	}
}

// Install readies loom-serve on this box and leaves it running the release now installed. First everything that can
// be refused: serve.conf, the token (a file holding one, owned by this user and readable by no one else), the worker's
// name, the hook, the health probe and the unit, each written only when its text changed (the unit beside its name and renamed over it,
// then systemd's view of it reloaded). Only then, last, is serve touched: started when it isn't running, reloaded when
// its unit changed or it runs another binary than the one the updater installed (a reload drains: the unit in hand
// finishes, and Restart=always starts the new runner), and otherwise left alone. So a refused install never reloads
// serve, and the updater running the hook again for one release costs nothing.
func Install(paths Paths, systemctl Systemctl, report io.Writer) error {
	content, err := os.ReadFile(paths.Config)
	if err != nil {
		return fmt.Errorf("this box's serve settings: %w", err)
	}
	config, err := ReadConfig(string(content))
	if err != nil {
		return fmt.Errorf("%s: %w", paths.Config, err)
	}
	if err := checkToken(paths.Token, paths.User); err != nil {
		return err
	}
	extras, err := readExtras(paths)
	if err != nil {
		return err
	}
	machineId, err := os.ReadFile(paths.MachineId)
	if err != nil {
		return fmt.Errorf("the machine id: %w", err)
	}
	worker, err := WorkerName(paths.Host, string(machineId))
	if err != nil {
		return err
	}
	houseCache, err := houseCacheSetting(paths.UpdateConfig)
	if err != nil {
		return err
	}
	if _, err := WriteChanged(paths.Hook, hookText, 0o755); err != nil {
		return fmt.Errorf("the updater's hook: %w", err)
	}
	if _, err := WriteChanged(paths.ClockHook, clockHookText, 0o755); err != nil {
		return fmt.Errorf("the updater's clock hook: %w", err)
	}
	// The probe puts serve's state in every report the updater posts: the canary's health, and `loom release status`.
	// Every serve this box runs, serve.conf's first: each is written, then each is readied, as one.
	type serving struct {
		name, text, pool, worker string
		changed                  bool
	}
	servings := []serving{{name: UnitName, text: Unit(config, worker, houseCache), pool: config.Pool, worker: worker}}
	for _, extra := range extras {
		servings = append(servings, serving{name: ExtraUnitName(extra.Name), text: ExtraUnit(extra, worker, houseCache), pool: extra.Config.Pool,
			worker: worker + "-" + extra.Name})
	}
	names := []string{}
	for _, each := range servings {
		names = append(names, each.name)
	}
	probe, err := updater.Probe(names...)
	if err != nil {
		return err
	}
	if _, err := WriteChanged(paths.Probe, probe, 0o755); err != nil {
		return fmt.Errorf("the updater's health probe: %w", err)
	}
	anyChanged := false
	for index := range servings {
		each := &servings[index]
		if each.changed, err = WriteChanged(filepath.Join(paths.Units, each.name), each.text, 0o644); err != nil {
			return err
		}
		if each.changed {
			anyChanged = true
			fmt.Fprintf(report, "loom-runner install-serve: wrote %s, serving pool %s as %s\n", filepath.Join(paths.Units, each.name), each.pool, each.worker)
		}
	}
	if anyChanged {
		if _, err := systemctl("daemon-reload"); err != nil {
			return err
		}
	}
	for _, each := range servings {
		if err := ready(paths, systemctl, report, each.name, each.changed); err != nil {
			return err
		}
	}
	return nil
}

// ready leaves one serve unit enabled and running this release: started when it isn't running, reloaded when its unit
// changed or it runs another binary than the one the updater installed, and otherwise left alone.
func ready(paths Paths, systemctl Systemctl, report io.Writer, unitName string, changed bool) error {
	if _, err := systemctl("enable", unitName); err != nil {
		return err
	}
	answer, err := systemctl("show", "--property=MainPID", "--value", unitName)
	if err != nil {
		return err
	}
	pid := strings.TrimSpace(answer)
	switch {
	case pid == "" || pid == "0":
		if _, err := systemctl("start", unitName); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom-runner install-serve: %s started\n", unitName)
	case changed || !SameFile(filepath.Join(paths.Proc, pid, "exe"), paths.Binary):
		if _, err := systemctl("reload", unitName); err != nil {
			return err
		}
		fmt.Fprintf(report, "loom-runner install-serve: %s reloaded: serve drains the unit in hand and starts again on this release\n", unitName)
	default:
		fmt.Fprintf(report, "loom-runner install-serve: %s already runs this unit and this release\n", unitName)
	}
	return nil
}

// checkToken refuses a pool token that isn't a file holding one, owned by this user and readable by no one else.
func checkToken(path string, user int) error {
	token, err := os.Stat(path)
	switch {
	case err != nil:
		return fmt.Errorf("the pool token: %w", err)
	case !token.Mode().IsRegular() || token.Size() == 0:
		return fmt.Errorf("the pool token %s isn't a file holding a token", path)
	case token.Mode().Perm()&0o077 != 0:
		return fmt.Errorf("the pool token %s is mode %o: anyone but its owner can read it (chmod 600)", path, token.Mode().Perm())
	}
	if owner, ok := token.Sys().(*syscall.Stat_t); !ok || int(owner.Uid) != user {
		return fmt.Errorf("the pool token %s isn't this user's", path)
	}
	return nil
}

// houseCacheSetting is the house cache update.conf names (housecache.Setting), empty when it names none or there is no
// update.conf.
func houseCacheSetting(path string) (string, error) {
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("this box's update settings: %w", err)
	}
	houseCache, err := housecache.Setting(string(content))
	if err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return houseCache, nil
}

// WriteChanged writes content to path, beside it first and renamed over it so nothing reads half of it, unless path
// already holds exactly that. It says whether it wrote.
func WriteChanged(path, content string, mode os.FileMode) (bool, error) {
	if held, err := os.ReadFile(path); err == nil && string(held) == content {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false, err
	}
	partial := path + ".partial"
	if err := os.WriteFile(partial, []byte(content), mode); err != nil {
		return false, err
	}
	if err := os.Chmod(partial, mode); err != nil {
		os.Remove(partial)
		return false, err
	}
	if err := os.Rename(partial, path); err != nil {
		os.Remove(partial)
		return false, err
	}
	return true, nil
}

// SameFile is whether two paths are one file, a running process's /proc/<pid>/exe and an installed binary's link
// alike; unreadable reads as not the same, so serve is reloaded rather than left on an unknown binary.
func SameFile(left, right string) bool {
	leftInfo, leftErr := os.Stat(left)
	rightInfo, rightErr := os.Stat(right)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}
