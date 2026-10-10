// Package updater holds what Go installs beside the shell updater (docs/updater.md): the health probe a machine's
// services are reported through.
package updater

import (
	_ "embed"
	"fmt"
	"regexp"
	"strings"
)

//go:embed health.d/units
var probeTemplate string

var unitPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]*\.(service|timer)$`)

// Probe is the health probe for these systemd user units: health.d/units with its units filled in. Each prints a line
// in every report the updater posts, which is how the release watcher and `loom release status` see the units.
func Probe(units ...string) (string, error) {
	if len(units) == 0 {
		return "", fmt.Errorf("a probe watches at least one unit")
	}
	for _, unit := range units {
		if !unitPattern.MatchString(unit) {
			return "", fmt.Errorf("%q isn't a systemd unit's name (a .service or a .timer)", unit)
		}
	}
	return strings.Replace(probeTemplate, `units="UNITS"`, `units="`+strings.Join(units, " ")+`"`, 1), nil
}
