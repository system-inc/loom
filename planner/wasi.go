package planner

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A unit whose tests run WASI needs the WASI SDK the tree pins, and its key says so: its tools part holds that SDK's
// release, and the job built from the key requires wasiSdk, so a runner without it never runs the unit and its 36 skips
// never read as a red (#vv28ewd, Judge's sort of the verify on #r0xntgv). Both come from the tree, never the planner's
// host, which set no WASI_SDK_VERSION and so keyed internal/native with no SDK at all.

// wasiPin is adamic's pin of the WASI SDK release in cloud/setup.sh, which installs it: `wasiVersion=27`.
var wasiPin = regexp.MustCompile(`(?m)^\s*wasiVersion=["']?([0-9][0-9.]*)["']?\s*$`)

// wasiSwitches are the gate switches that turn a package's WASI tests on: a package whose tests read one runs WASI.
var wasiSwitches = []string{"ADAMIC_TEST_WASI", "ADAMIC_ORACLE_WASI"}

// TreeWasiSdk is the WASI SDK release the tree pins, empty when it pins none.
func TreeWasiSdk(tree string) (string, error) {
	content, err := os.ReadFile(filepath.Join(tree, "cloud", "setup.sh"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if match := wasiPin.FindSubmatch(content); match != nil {
		return string(match[1]), nil
	}
	return "", nil
}

// runsWasi says whether a package's tests read a WASI gate switch, from its test files for this platform.
func runsWasi(listed listedPackage) (bool, error) {
	for _, file := range append(append([]string{}, listed.TestGoFiles...), listed.XTestGoFiles...) {
		content, err := os.ReadFile(filepath.Join(listed.Dir, file))
		if err != nil {
			return false, err
		}
		for _, name := range wasiSwitches {
			if strings.Contains(string(content), `"`+name+`"`) {
				return true, nil
			}
		}
	}
	return false, nil
}

// unitTools is a test unit's tools part: the planner's, with the tree's WASI SDK for a package that runs WASI and
// none for any other, so only a WASI unit requires it.
func unitTools(tools Tools, listed listedPackage, wasiSdk string) (Tools, error) {
	tools.WasiSdk = ""
	wasi, err := runsWasi(listed)
	if err != nil || !wasi {
		return tools, err
	}
	tools.WasiSdk = wasiSdk
	return tools, nil
}
