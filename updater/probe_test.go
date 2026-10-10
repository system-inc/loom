package updater

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The probe prints a line per unit from systemctl's own answer, in the shape the updater's report keeps: state,
// substate and restarts, or not-found for a unit systemd doesn't know.
func TestTheProbePrintsALinePerUnit(t *testing.T) {
	probe, err := Probe("loom-serve.service", "loom-pusher.timer", "loom-gone.service")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "probe")
	os.WriteFile(path, []byte(probe), 0o755)
	// systemctl show answers in its own order, whatever order the properties were asked in.
	stub := `#!/bin/sh
case "$3" in
loom-serve.service) printf 'NRestarts=2\nActiveState=active\nSubState=running\nLoadState=loaded\n' ;;
loom-pusher.timer) printf 'LoadState=loaded\nActiveState=active\nSubState=waiting\nNRestarts=\n' ;;
*) printf 'LoadState=not-found\nActiveState=inactive\nSubState=dead\nNRestarts=0\n' ;;
esac
`
	os.WriteFile(filepath.Join(directory, "systemctl"), []byte(stub), 0o755)
	command := exec.Command(path)
	command.Env = []string{"PATH=" + directory + ":/usr/bin:/bin"}
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	want := "loom-serve.service active running restarts=2\nloom-pusher.timer active waiting\nloom-gone.service not-found\n"
	if string(output) != want {
		t.Fatalf("printed:\n%s\nwant:\n%s", output, want)
	}
	for _, bad := range [][]string{nil, {"loom-serve"}, {"loom serve.service"}, {"$(reboot).service"}} {
		if probe, err := Probe(bad...); err == nil {
			t.Errorf("%q made a probe:\n%s", bad, probe)
		}
	}
	if !strings.Contains(probe, "systemctl --user show") {
		t.Fatal("the probe asks no user unit")
	}
}
