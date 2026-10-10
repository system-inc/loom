package release

import (
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// A Config is ~/.loom/release.conf on Workshop: key = value lines, # comments, as update.conf and serve.conf are. Its
// presence is what makes a machine release: `loom release install` does nothing where there is none.
type Config struct {
	Repository     string              // the Loom clone the watcher fetches main into and runs publish.sh and upload.sh from
	Remote         string              // its remote
	Branch         string              // the branch whose head is released
	Out            string              // publish.sh's out directory
	Destination    string              // upload.sh's destination
	Base           string              // where the boxes read current.txt, for status
	State          string              // the watcher's state: release.json, reports/, marks/
	Listen         string              // Workshop's LAN address and port the reports are received on, never every interface
	Addresses      map[string][]string // each box's addresses, by lowercase host; a box not named is resolved by its name
	Canary         string              // the host that takes each release first
	Boxes          []string            // every box that runs the updater, the canary among them
	CanaryServices []string            // the services the canary must report active
	Pools          []string            // the pools the boxes serve, read for when each box's serve last asked
	Units          []string            // this machine's own services, which its health probe reports
	CanaryWithin   time.Duration       // how long the canary has to report the release installed and healthy
	Soak           time.Duration       // how long it then has to stay healthy before the release is promoted
	FleetWithin    time.Duration       // how long every other box has to report the release once promoted
	Restarts       int                 // how many times a canary service may restart during the soak
	Interval       time.Duration       // the watcher's pass
}

// DefaultConfig is Workshop's, under home.
func DefaultConfig(home string) Config {
	return Config{
		Repository:     filepath.Join(home, "Projects", "system", "loom"),
		Remote:         "origin",
		Branch:         "main",
		Out:            filepath.Join(home, "loom-releases", "out"),
		Destination:    "r2:loom-artifacts/releases",
		Base:           "https://artifacts.loom.system.inc/releases",
		State:          filepath.Join(home, ".loom", "releases"),
		Addresses:      map[string][]string{},
		Canary:         "Cloud",
		Boxes:          []string{"Workshop", "Cloud", "Server", "Home", "Chonchon"},
		CanaryServices: []string{"loom-serve.service"},
		Pools:          []string{"box-strict", "box-phase"},
		Units:          []string{"loom-plan.service", "loom-place.service", "loom-build-trees.service", "loom-pusher.timer", "loom-release.service"},
		CanaryWithin:   15 * time.Minute,
		Soak:           10 * time.Minute,
		FleetWithin:    15 * time.Minute,
		// A canary's serve restarts once when the release drains it, and once an hour when --until ends it; a crash
		// loop restarts it every 30 s.
		Restarts: 2,
		Interval: 30 * time.Second,
	}
}

// CheckListen refuses a listen address that isn't one IP address and a port: never a name, and never every
// interface (":7381", "0.0.0.0:7381", "[::]:7381"), since the receiver is for the house's LAN alone.
func CheckListen(listen string) error {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %q isn't <address>:<port>", listen)
	}
	if number, err := strconv.Atoi(port); err != nil || number < 1 || number > 65535 {
		return fmt.Errorf("listen %q names no port", listen)
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		return fmt.Errorf("listen %q isn't one IP address: the receiver binds Workshop's LAN address alone, never every interface", listen)
	}
	return nil
}

var unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*\.(service|timer)$`)

// ReadConfig reads release.conf over the defaults: any key it doesn't know, a key twice, or a value that doesn't
// read is refused, so a typo never releases somewhere unmeant. Lists are space or comma separated.
func ReadConfig(content string, defaults Config) (Config, error) {
	config := defaults
	seen := map[string]bool{}
	for number, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found || seen[key] || value == "" {
			return Config{}, fmt.Errorf("release.conf line %d: %q isn't a key = value line of its own", number+1, line)
		}
		seen[key] = true
		list := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		duration := func(target *time.Duration) error {
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return fmt.Errorf("release.conf line %d: %s %q isn't a duration such as 15m", number+1, key, value)
			}
			*target = parsed
			return nil
		}
		hosts := func(target *[]string) error {
			for _, host := range list {
				if !hostPattern.MatchString(host) {
					return fmt.Errorf("release.conf line %d: %q isn't a host name", number+1, host)
				}
			}
			*target = list
			return nil
		}
		var err error
		switch key {
		case "repository":
			config.Repository = value
		case "remote":
			config.Remote = value
		case "branch":
			config.Branch = value
		case "out":
			config.Out = value
		case "destination":
			config.Destination = value
		case "base":
			config.Base = strings.TrimSuffix(value, "/")
		case "state":
			config.State = value
		case "listen":
			if err = CheckListen(value); err != nil {
				err = fmt.Errorf("release.conf line %d: %w", number+1, err)
			}
			config.Listen = value
		case "addresses":
			config.Addresses = map[string][]string{}
			for _, entry := range strings.Fields(value) {
				host, list, found := strings.Cut(entry, "=")
				if !found || !hostPattern.MatchString(host) {
					err = fmt.Errorf("release.conf line %d: %q isn't <host>=<address>[,<address>]", number+1, entry)
					break
				}
				for _, address := range strings.Split(list, ",") {
					if net.ParseIP(address) == nil {
						err = fmt.Errorf("release.conf line %d: %q isn't an IP address", number+1, address)
						break
					}
					config.Addresses[strings.ToLower(host)] = append(config.Addresses[strings.ToLower(host)], address)
				}
				if err != nil {
					break
				}
			}
		case "canary":
			if !hostPattern.MatchString(value) {
				err = fmt.Errorf("release.conf line %d: canary %q isn't a host name", number+1, value)
			}
			config.Canary = value
		case "boxes":
			err = hosts(&config.Boxes)
		case "pools":
			err = hosts(&config.Pools)
		case "canary-services", "units":
			for _, unit := range list {
				if !unitPattern.MatchString(unit) {
					err = fmt.Errorf("release.conf line %d: %q isn't a systemd unit's name (a .service or a .timer)", number+1, unit)
				}
			}
			if key == "units" {
				config.Units = list
			} else {
				config.CanaryServices = list
			}
		case "canary-within":
			err = duration(&config.CanaryWithin)
		case "soak":
			err = duration(&config.Soak)
		case "fleet-within":
			err = duration(&config.FleetWithin)
		case "interval":
			err = duration(&config.Interval)
		case "restarts":
			config.Restarts, err = strconv.Atoi(value)
			if err != nil || config.Restarts < 0 {
				err = fmt.Errorf("release.conf line %d: restarts %q isn't a whole number", number+1, value)
			}
		default:
			err = fmt.Errorf("release.conf line %d: no setting %q", number+1, key)
		}
		if err != nil {
			return Config{}, err
		}
	}
	canaryBoxed := false
	for _, box := range config.Boxes {
		canaryBoxed = canaryBoxed || strings.EqualFold(box, config.Canary)
	}
	if !canaryBoxed {
		return Config{}, fmt.Errorf("release.conf: the canary %s isn't one of the boxes (%s)", config.Canary, strings.Join(config.Boxes, ", "))
	}
	// The canary reports every 5 minutes when nothing changes, so a soak shorter than that may hear nothing from it.
	if config.Soak < 6*time.Minute {
		return Config{}, fmt.Errorf("release.conf: soak %s is under 6m, and a healthy canary reports only every 5", config.Soak)
	}
	return config, nil
}

// An Order is what a release declares in its commit's updater/release-order: the steps that must be done before the
// fleet takes it (it waits, unpublished, until each is marked done), and those to do once the fleet has it (it is
// finished only once each is marked, and the next release waits on it). The watcher does none of the steps itself;
// it honors them and says which it waits on.
type Order struct {
	Before []string `json:"before,omitempty"`
	After  []string `json:"after,omitempty"`
}

// OrderFile is where a commit declares its order.
const OrderFile = "updater/release-order"

var stepPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ParseOrder reads a release-order file: # comments, and lines "<step> after fleet" (do it once the fleet has the
// release) or "fleet after <step>" (do it before). Anything else is refused, and the release with it, since a rule
// the watcher can't honor must stop it rather than be skipped.
func ParseOrder(content string) (Order, error) {
	order := Order{}
	seen := map[string]bool{}
	for number, line := range strings.Split(content, "\n") {
		if cut := strings.IndexByte(line, '#'); cut >= 0 {
			line = line[:cut]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 3 || fields[1] != "after" || (fields[0] == "fleet") == (fields[2] == "fleet") {
			return Order{}, fmt.Errorf("%s line %d: %q isn't \"<step> after fleet\" or \"fleet after <step>\"", OrderFile, number+1, strings.Join(fields, " "))
		}
		step := fields[0]
		if step == "fleet" {
			step = fields[2]
		}
		if !stepPattern.MatchString(step) {
			return Order{}, fmt.Errorf("%s line %d: step %q isn't a lowercase name", OrderFile, number+1, step)
		}
		if seen[step] {
			return Order{}, fmt.Errorf("%s line %d: step %s is ordered twice", OrderFile, number+1, step)
		}
		seen[step] = true
		if fields[0] == "fleet" {
			order.Before = append(order.Before, step)
		} else {
			order.After = append(order.After, step)
		}
	}
	return order, nil
}
