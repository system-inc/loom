package planner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
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
