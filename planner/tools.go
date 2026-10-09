package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"strings"
)

// ProbeTools reads the box's tools part: the runner binary's sha256 and the toolchains' versions. A toolchain the box
// lacks reads as empty, which is part of the key: a box without clang keys apart from one with it.
func ProbeTools(runner string) (Tools, error) {
	tools := Tools{WasiSdk: os.Getenv("WASI_SDK_VERSION")}
	if runner != "" {
		content, err := os.ReadFile(runner)
		if err != nil {
			return Tools{}, err
		}
		sum := sha256.Sum256(content)
		tools.Runner = hex.EncodeToString(sum[:])
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
