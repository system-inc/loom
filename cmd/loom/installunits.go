package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/system-inc/loom/daemons"
)

// installUnits is `loom install-units`, which the updater's hook 30-units runs after every release: the release's units
// for the daemons this machine holds tokens for (planner, placer, tree builder, judge), written where systemd reads
// them and never enabled, started or restarted (daemons.Install).
func installUnits(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) != 0 {
		fmt.Fprintln(stderr, "usage: loom install-units")
		return 2
	}
	if runtime.GOOS != "linux" {
		fmt.Fprintf(stderr, "loom install-units: the daemons are systemd user units, and this is %s\n", runtime.GOOS)
		return 3
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "loom install-units: %v\n", err)
		return 3
	}
	systemctl := func(arguments ...string) (string, error) {
		output, err := exec.Command("systemctl", append([]string{"--user"}, arguments...)...).CombinedOutput()
		if err != nil {
			return string(output), fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
		}
		return string(output), nil
	}
	if err := daemons.Install(daemons.HomePaths(home), systemctl, stdout); err != nil {
		fmt.Fprintf(stderr, "loom install-units: %v\n", err)
		return 1
	}
	return 0
}
