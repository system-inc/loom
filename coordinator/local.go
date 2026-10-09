package coordinator

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner"
)

// LocalMachine runs units in this process with the runner package, for tests and for trying a job on the
// coordinator's own machine.
type LocalMachine struct {
	// Label names the machine in the record; empty means this computer's short hostname.
	Label string
	// WorkspaceParent is where unit workspaces go; empty means the system's temporary directory.
	WorkspaceParent string
	// Has names the toolchains this machine has, for units that require them (protocol.Toolchains).
	Has []string
	// GoPlatform names the platform this machine reports, for a test standing one machine in for another; empty is
	// this computer's own.
	GoPlatform string
}

func (machine LocalMachine) Toolchains() []string { return machine.Has }

// Name is the label, or this computer's short hostname, which is what its runner reports in started events.
func (machine LocalMachine) Name() string {
	if machine.Label != "" {
		return machine.Label
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		short, _, _ := strings.Cut(name, ".")
		return short
	}
	return "local"
}

func (machine LocalMachine) RunnerVersion() string { return runner.Version }

func (machine LocalMachine) Platform() string {
	if machine.GoPlatform != "" {
		return machine.GoPlatform
	}
	return runtime.GOOS + "/" + runtime.GOARCH
}

func (machine LocalMachine) Cores() int { return runtime.NumCPU() }

func (machine LocalMachine) Run(runContext context.Context, unit protocol.Unit, events io.Writer) error {
	runner.Run(runContext, unit, runner.Options{WorkspaceParent: machine.WorkspaceParent, Events: events})
	return nil
}
