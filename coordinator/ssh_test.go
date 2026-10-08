package coordinator

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/system-inc/loom/protocol"
)

// TestSSHMachineOnARealBox runs a unit on a real box: build the runner for its platform, install it, take a
// gate slot by the gate's own flock and taskset, and read the events back. It runs only when LOOM_SSH_BOX
// names a box, since it needs ssh and takes one of the box's slots for a moment.
func TestSSHMachineOnARealBox(t *testing.T) {
	box := os.Getenv("LOOM_SSH_BOX")
	if box == "" {
		t.Skip("set LOOM_SSH_BOX to a box (one Loom may use) to run this")
	}
	setup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	platform, cores, err := Probe(setup, box)
	if err != nil {
		t.Fatal(err)
	}
	system, architecture, _ := strings.Cut(platform, "/")
	binary := filepath.Join(t.TempDir(), "loom-runner")
	version := "test-" + time.Now().UTC().Format("20060102T150405")
	build := exec.Command("go", "build", "-ldflags", "-X github.com/system-inc/loom/runner.Version="+version, "-o", binary, "../runner/cmd/loom-runner")
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+system, "GOARCH="+architecture)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	remote := ".loom/bin/loom-runner-" + version
	if installed, err := Install(setup, box, binary, remote); err != nil || !installed {
		t.Fatalf("install: %v %v", installed, err)
	}
	defer exec.Command("ssh", box, "rm -f "+remote).Run()
	if installed, err := Install(setup, box, binary, remote); err != nil || installed {
		t.Fatalf("a second install should find it there: %v %v", installed, err)
	}
	if cores < 1 {
		t.Fatalf("%s has %d cores", box, cores)
	}
	machine := SSHMachine{Box: box, Class: "S", Runner: remote, Version: version, GoPlatform: platform, CoreCount: cores}
	unit := protocol.Unit{Run: "r-ssh-test", Unit: "where", TimeoutSeconds: 30,
		Argv: []string{"sh", "-c", "hostname; taskset -cp $$ | sed 's/.*: //'; echo slot=$LOOM_SLOT cpus=$LOOM_SLOT_CPUS; exit 4"}}
	var events bytes.Buffer
	if err := machine.Run(setup, unit, &events); err != nil {
		t.Fatal(err)
	}
	text := events.String()
	t.Log(text)
	if !strings.Contains(text, `"runnerVersion":"`+version+`"`) || !strings.Contains(text, `"code":4`) || !strings.Contains(text, `"status":"failed"`) ||
		!strings.Contains(text, "slot=1 cpus=0-") {
		t.Fatalf("events:\n%s", text)
	}
}
