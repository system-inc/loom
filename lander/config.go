package lander

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
)

// A Config is ~/.loom/push.conf, whose being there is what makes a machine the lander: the branch it lands on, the
// queue it reads, the lander's bare clone, its state directory (the lock) and the token secret it mints from.
type Config struct {
	Branch     string
	Queue      string
	Repository string
	State      string
	Secret     string
}

// DefaultConfig is Workshop's: main, loom.system.inc, ~/loom-lander/adamic.git,
// ~/loom-lander/state and ~/.loom/token-secret.
func DefaultConfig(home string) Config {
	return Config{
		Branch:     "main",
		Queue:      "https://loom.system.inc",
		Repository: filepath.Join(home, "loom-lander", "adamic.git"),
		State:      filepath.Join(home, "loom-lander", "state"),
		Secret:     filepath.Join(home, ".loom", "token-secret"),
	}
}

// A branch the lander lands on: a plain name git takes after refs/heads/, never an option, a revision or a pattern.
var branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)

// ReadConfig reads push.conf as the updater reads update.conf: key = value lines, # comments, blank lines skipped, every
// setting optional over DefaultConfig. A path may start ~/ for home. Any other key, a key twice, or a line that isn't
// key = value is refused, so a typo never lands on the wrong branch quietly.
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
			return Config{}, fmt.Errorf("push.conf line %d: %q isn't a key = value line of its own", number+1, line)
		}
		seen[key] = true
		if strings.HasPrefix(value, "~/") {
			value = filepath.Join(home, value[2:])
		}
		switch key {
		case "branch":
			if !branchPattern.MatchString(value) || strings.Contains(value, "..") || strings.Contains(value, "//") || strings.HasSuffix(value, "/") ||
				strings.HasSuffix(value, ".") || strings.HasSuffix(value, ".lock") || strings.Contains(value, "/.") {
				return Config{}, fmt.Errorf("push.conf line %d: branch %q isn't a branch name (letters, digits, dot, dash, underscore, slash)", number+1, value)
			}
			config.Branch = value
		case "queue":
			parsed, err := url.Parse(value)
			if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				return Config{}, fmt.Errorf("push.conf line %d: queue %q isn't an http(s) address", number+1, value)
			}
			config.Queue = value
		case "repository", "state", "secret":
			if !filepath.IsAbs(value) {
				return Config{}, fmt.Errorf("push.conf line %d: %s %q isn't an absolute path or ~/", number+1, key, value)
			}
			switch key {
			case "repository":
				config.Repository = value
			case "state":
				config.State = value
			default:
				config.Secret = value
			}
		default:
			return Config{}, fmt.Errorf("push.conf line %d: no setting %q (branch, queue, repository, state, secret)", number+1, key)
		}
	}
	return config, nil
}
