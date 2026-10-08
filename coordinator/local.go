package coordinator

import (
	"context"
	"io"
	"runtime"

	"github.com/system-inc/loom/protocol"
	"github.com/system-inc/loom/runner"
)

// LocalMachine runs units in this process with the runner package, for tests and for trying a job on the
// coordinator's own machine.
type LocalMachine struct {
	// Label names the machine in the record; empty means "local".
	Label string
	// WorkspaceParent is where unit workspaces go; empty means the system's temporary directory.
	WorkspaceParent string
}

func (machine LocalMachine) Name() string {
	if machine.Label == "" {
		return "local"
	}
	return machine.Label
}

func (machine LocalMachine) RunnerVersion() string { return runner.Version }

func (machine LocalMachine) Platform() string { return runtime.GOOS + "/" + runtime.GOARCH }

func (machine LocalMachine) Run(runContext context.Context, unit protocol.Unit, events io.Writer) error {
	runner.Run(runContext, unit, runner.Options{WorkspaceParent: machine.WorkspaceParent, Events: events})
	return nil
}
