package planner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// ProbeTools reads the box's tools part: the pinned runner's sha256 and the toolchains' versions. A toolchain the box
// lacks reads as empty, which is part of the key: a box without clang keys apart from one with it. The runner is the
// pinned runner of the pool the unit is placed on, read from one file (Loom, Oct 10 00:18Z), so a key never holds a
// runner no pool runs; runnerShaFile empty leaves it empty, as a product's key wants (a product is the same whichever
// runner asked).
func ProbeTools(runnerShaFile string) (Tools, error) {
	tools := Tools{WasiSdk: os.Getenv("WASI_SDK_VERSION")}
	if runnerShaFile != "" {
		var err error
		if tools.Runner, err = ReadRunnerPin(runnerShaFile); err != nil {
			return Tools{}, err
		}
	}
	tools.Go = firstLine("go", "env", "GOVERSION")
	tools.Clang = firstLine("clang", "--version")
	tools.Node = firstLine("node", "--version")
	return tools, nil
}

// TreeGoVersionBound bounds TreeGoVersion. A change that raises go.mod's go line makes go fetch that release, and
// against a proxy it can't reach go env hung (third review, killed at 20 s), stalling all planning. A few minutes
// covers a real toolchain download; past it the tree is refused, never waited on.
const TreeGoVersionBound = 3 * time.Minute

// TreeGoVersion is the Go release go resolves inside the tree: go env GOVERSION run there under GOTOOLCHAIN=auto, as
// adamic's setup leaves it, so the tree's go.mod go and toolchain lines decide, not the planner's own Go alone. The
// gofmt phase is keyed on it, and the runner resolves exactly that release (GOTOOLCHAIN=<it>) for the gofmt it runs.
func TreeGoVersion(tree string) (string, error) {
	return treeGoVersion(tree, TreeGoVersionBound)
}

func treeGoVersion(tree string, bound time.Duration) (string, error) {
	versionContext, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	command := exec.CommandContext(versionContext, "go", "env", "GOVERSION")
	command.Dir = tree
	// The last GOTOOLCHAIN in an environment is the one go gets, so a planner's own GOTOOLCHAIN=local can't win.
	command.Env = append(os.Environ(), "GOTOOLCHAIN=auto")
	// go runs in a process group of its own, which the bound kills whole, so nothing go started outlives it, as the
	// runner's goCommand does.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = 5 * time.Second
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if versionContext.Err() != nil {
		return "", fmt.Errorf("go env GOVERSION in %s didn't answer within %v (a Go release its go.mod names that can't be fetched?), so the tree isn't planned", tree, bound)
	}
	version := strings.TrimSpace(string(output))
	if err != nil || version == "" {
		return "", fmt.Errorf("go env GOVERSION in %s: %v: %s", tree, err, strings.TrimSpace(stderr.String()))
	}
	return version, nil
}

func firstLine(name string, arguments ...string) string {
	output, err := exec.Command(name, arguments...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(output), "\n", 2)[0])
}

// ReadRunnerPin reads the pinned runner's sha256, which every test key holds. The steady planner reads it before each
// pull, so a runner switch moves every key from the next pull on, with no restart (Loom, Oct 10 02:5xZ).
func ReadRunnerPin(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("the pinned runner: %w", err)
	}
	runner := strings.TrimSpace(string(content))
	if !Sha256Hex(runner) {
		return "", fmt.Errorf("the pinned runner in %s isn't a sha256: %q", path, runner)
	}
	return runner, nil
}
