// Package release carries a commit on loom main to every house box (docs/releases.md): Workshop's watcher notices it,
// publishes it as a canary for Cloud, waits for Cloud's updater to report it installed and healthy, and promotes it to
// every box, stopping loudly on any failure. It also receives the reports every box's updater posts, and reads them
// back as `loom release status`: each box's version, when it last updated, whether it is held, and its services.
package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A Manifest is current.txt as publish.sh writes it and the updater reads it (docs/updater.md): the hosts a canary
// names, and one section, or two when there is a canary.
type Manifest struct {
	Canary   []string
	Sections []Section
}

// A Section is one version and its files, "<name> <os>/<arch> <sha256>" each.
type Section struct {
	Version string
	Files   []string
}

var plainName = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._-]{0,127}$`)
var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ParseManifest reads a manifest as the updater's awk does, refusing what it refuses: a canary line not first, a
// section that doesn't close with an end line counting its files, a file outside a section, a name twice for one
// platform, any other line, and a manifest cut short (a missing end line, or a canary's missing second section).
func ParseManifest(text string) (Manifest, error) {
	manifest := Manifest{}
	ended := false
	seen := map[string]bool{}
	for number, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		at := func(format string, arguments ...any) error {
			return fmt.Errorf("line %d: %s", number+1, fmt.Sprintf(format, arguments...))
		}
		switch {
		case len(fields) == 0:
		case fields[0] == "canary" && len(fields) >= 2:
			if len(manifest.Sections) > 0 || manifest.Canary != nil {
				return Manifest{}, at("a canary line not first")
			}
			manifest.Canary = fields[1:]
		case fields[0] == "version" && len(fields) == 2:
			if len(manifest.Sections) > 0 && !ended {
				return Manifest{}, at("a version line before the last section ended")
			}
			if len(manifest.Sections) == 1+min(len(manifest.Canary), 1) {
				return Manifest{}, at("a section more than the manifest names")
			}
			if !plainName.MatchString(fields[1]) {
				return Manifest{}, at("version %s is not a plain name", fields[1])
			}
			manifest.Sections = append(manifest.Sections, Section{Version: fields[1]})
			ended = false
		case fields[0] == "end" && len(fields) == 2:
			if len(manifest.Sections) == 0 || ended {
				return Manifest{}, at("an end line outside a section")
			}
			count, err := strconv.Atoi(fields[1])
			if section := manifest.Sections[len(manifest.Sections)-1]; err != nil || count != len(section.Files) {
				return Manifest{}, at("the end line counts %s files, the section holds %d", fields[1], len(section.Files))
			}
			ended = true
		case len(fields) == 3:
			if len(manifest.Sections) == 0 || ended {
				return Manifest{}, at("a file outside a section")
			}
			if !plainName.MatchString(fields[0]) || fields[0] == "SHA256SUMS" {
				return Manifest{}, at("file name %s is not a plain name", fields[0])
			}
			if !sha256Pattern.MatchString(fields[2]) {
				return Manifest{}, at("%s is not a sha256", fields[2])
			}
			key := strconv.Itoa(len(manifest.Sections)) + " " + fields[0] + " " + fields[1]
			if seen[key] {
				return Manifest{}, at("%s for %s twice", fields[0], fields[1])
			}
			seen[key] = true
			section := &manifest.Sections[len(manifest.Sections)-1]
			section.Files = append(section.Files, strings.Join(fields, " "))
		default:
			return Manifest{}, at("not a manifest line: %s", line)
		}
	}
	if len(manifest.Sections) < 1+min(len(manifest.Canary), 1) || !ended {
		return Manifest{}, fmt.Errorf("it ends before its last end line: cut short")
	}
	return manifest, nil
}

// Top is the version every box but the canary's follows.
func (manifest Manifest) Top() string {
	return manifest.Sections[0].Version
}

// For is the version a host follows: the canary's section when the canary line names it (ignoring case), else the top.
func (manifest Manifest) For(host string) string {
	for _, canary := range manifest.Canary {
		if strings.EqualFold(canary, host) {
			return manifest.Sections[1].Version
		}
	}
	return manifest.Top()
}

// CanaryVersion is the version the canary hosts are offered, or nothing when there is no canary.
func (manifest Manifest) CanaryVersion() string {
	if len(manifest.Canary) == 0 {
		return ""
	}
	return manifest.Sections[1].Version
}
