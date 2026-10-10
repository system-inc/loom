package queuebridge

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// A Config is ~/.loom/queue-bridge.conf, whose being there is what makes a machine the bridge: the queue it carries
// git's facts to, the adamic clone it reads them from, its state directory (the lock and memory.json), the token secret
// it mints from, today's gate's push-main.sh and requeue.sh, and whether today's gate still decides futures here.
type Config struct {
	Queue      string
	Repository string
	State      string
	Secret     string
	PushMain   string
	Requeue    string
	Decides    bool
}

// DefaultConfig is Kirk's Mac's, where the bridge has run: loom.system.inc, ~/Projects/system/adamic, ~/.loom/queue-bridge,
// ~/.loom/token-secret, the push-main checkout ~/.adamic-merge-tree, ~/.loom/bin/requeue.sh, and deciding.
func DefaultConfig(home string) Config {
	return Config{
		Queue:      "https://loom.system.inc",
		Repository: filepath.Join(home, "Projects", "system", "adamic"),
		State:      filepath.Join(home, ".loom", "queue-bridge"),
		Secret:     filepath.Join(home, ".loom", "token-secret"),
		PushMain:   filepath.Join(home, ".adamic-merge-tree", "cloud", "integration", "push-main.sh"),
		Requeue:    filepath.Join(home, ".loom", "bin", "requeue.sh"),
		Decides:    true,
	}
}

// ReadConfig reads queue-bridge.conf as the lander reads push.conf: key = value lines, # comments, blank lines skipped,
// every setting optional over DefaultConfig. A path may start ~/ for home, and decides is yes or no (no once Judge
// decides every future, #xvvf6cn). Any other key, a key twice, or a line that isn't key = value is refused, so a typo
// never leaves today's gate deciding quietly.
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
		case "decides":
			if value != "yes" && value != "no" {
				return Config{}, fmt.Errorf("queue-bridge.conf line %d: decides is yes or no, not %q", number+1, value)
			}
			config.Decides = value == "yes"
		case "repository", "state", "secret", "push-main", "requeue":
			if !filepath.IsAbs(value) {
				return Config{}, fmt.Errorf("queue-bridge.conf line %d: %s %q isn't an absolute path or ~/", number+1, key, value)
			}
			*map[string]*string{"repository": &config.Repository, "state": &config.State, "secret": &config.Secret,
				"push-main": &config.PushMain, "requeue": &config.Requeue}[key] = value
		default:
			return Config{}, fmt.Errorf("queue-bridge.conf line %d: no setting %q (queue, repository, state, secret, push-main, requeue, decides)", number+1, key)
		}
	}
	return config, nil
}
