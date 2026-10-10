package queuebridge

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// A Config is ~/.loom/queue-bridge.conf, whose being there is what makes a machine the bridge: the queue it carries
// git's facts to, the adamic clone it reads them from, its state directory (the lock) and the token secret it mints
// from.
type Config struct {
	Queue      string
	Repository string
	State      string
	Secret     string
}

// DefaultConfig is Workshop's: loom.system.inc, ~/loom-queue-bridge/adamic.git (git clone --bare of the public
// https://github.com/system-inc/adamic.git: the bridge holds no key), ~/loom-queue-bridge/state and ~/.loom/token-secret.
func DefaultConfig(home string) Config {
	return Config{
		Queue:      "https://loom.system.inc",
		Repository: filepath.Join(home, "loom-queue-bridge", "adamic.git"),
		State:      filepath.Join(home, "loom-queue-bridge", "state"),
		Secret:     filepath.Join(home, ".loom", "token-secret"),
	}
}

// ReadConfig reads queue-bridge.conf as the lander reads push.conf: key = value lines, # comments, blank lines skipped,
// every setting optional over DefaultConfig. A path may start ~/ for home. Any other key, a key twice, or a line that
// isn't key = value is refused, so a typo never sends facts from the wrong clone quietly.
func ReadConfig(content, home string) (Config, error) {
	config := DefaultConfig(home)
	seen := map[string]bool{}
	for number, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if !found || seen[key] || value == "" {
			return Config{}, fmt.Errorf("queue-bridge.conf line %d: %q isn't a key = value line of its own", number+1, line)
		}
		seen[key] = true
		if strings.HasPrefix(value, "~/") {
			value = filepath.Join(home, value[2:])
		}
		switch key {
		case "queue":
			parsed, err := url.Parse(value)
			if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				return Config{}, fmt.Errorf("queue-bridge.conf line %d: queue %q isn't an http(s) address", number+1, value)
			}
			config.Queue = value
		case "repository", "state", "secret":
			if !filepath.IsAbs(value) {
				return Config{}, fmt.Errorf("queue-bridge.conf line %d: %s %q isn't an absolute path or ~/", number+1, key, value)
			}
			*map[string]*string{"repository": &config.Repository, "state": &config.State, "secret": &config.Secret}[key] = value
		default:
			return Config{}, fmt.Errorf("queue-bridge.conf line %d: no setting %q (queue, repository, state, secret)", number+1, key)
		}
	}
	return config, nil
}
