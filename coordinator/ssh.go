package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"

	"github.com/system-inc/loom/protocol"
)

// An SSHMachine runs units on one of our boxes over ssh. It shares the box with the fast gate the gate's own
// way, so the two can never share CPUs: it takes one of the box's gate slots by the same flock
// (~/fast-gate/lock, lock-2 ...) and runs the runner under taskset on that slot's CPUs, the same partition
// adamic's cloud/fast-gate.sh makes. Which slots Loom may take at all is the coordinator's allowance
// (~/.loom/slots), agreed with the gate's owner.
type SSHMachine struct {
	Box   string // the ssh host
	Class string // "B" for the box's area slot, "S" for a small one
	// Runner is the runner binary's path on the box relative to the login's home, where ssh starts, and
	// version-named: .loom/bin/loom-runner-<version>.
	Runner     string
	Version    string
	GoPlatform string // "linux/amd64", from Probe
	CoreCount  int    // the box's whole CPU count, from Probe
}

func (machine SSHMachine) Name() string          { return machine.Box }
func (machine SSHMachine) RunnerVersion() string { return machine.Version }
func (machine SSHMachine) Platform() string      { return machine.GoPlatform }
func (machine SSHMachine) Cores() int            { return machine.CoreCount }

// slotScript takes a free gate slot of the class, waiting for one, and runs the runner pinned to its CPUs
// with the unit on stdin. The lock is fd 9, which exec hands to the runner, so it holds while the unit runs.
// Keep the partition identical to adamic's cloud/fast-gate.sh.
const slotScript = `set -euo pipefail
class=$1 runner=$2
mkdir -p ~/fast-gate ~/loom-units
slot=""
while [ -z "${slot}" ]; do
  candidates=$([ "${class}" = S ] && echo "2 3" || echo "1")
  [ "${class}" = S ] && [ "$(nproc --all)" -ge 64 ] && candidates="2 3 4"
  [ "$(nproc --all)" -lt 32 ] && candidates=1
  for candidate in ${candidates}; do
    exec 9> ~/fast-gate/lock$([ "${candidate}" = 1 ] && echo "" || echo "-${candidate}")
    if flock -n 9; then slot=${candidate}; break; fi
    exec 9>&-
  done
  # A coordinator that went away (its ssh dropped or was killed) leaves this script orphaned; it must not
  # take a slot for a unit nobody is waiting on.
  [ -n "${slot}" ] || { kill -0 "${PPID}" 2>/dev/null || exit 3; sleep 1; }
done
cpus=$(nproc --all)
area=$((cpus * 3 / 8))
small=$((cpus * 3 / 16))
case ${slot} in
  1) first=0 share=${area}; [ "${cpus}" -lt 32 ] && share=${cpus} ;;
  2) first=${area} share=${small} ;;
  3) first=$((area + small)) share=${small} ;;
  *) first=$((area + 2 * small)) share=$((cpus - area - 2 * small)) ;;
esac
echo "loom: $(hostname) slot ${slot}, cpus ${first}-$((first + share - 1))" >&2
export LOOM_SLOT=${slot} LOOM_SLOT_CPUS=${first}-$((first + share - 1))
exec taskset -c "${LOOM_SLOT_CPUS}" "${runner}" run --workspace ~/loom-units -
`

func (machine SSHMachine) Run(runContext context.Context, unit protocol.Unit, events io.Writer) error {
	body, err := json.Marshal(unit)
	if err != nil {
		return err
	}
	var diagnostics bytes.Buffer
	command := exec.CommandContext(runContext, "ssh", "-o", "BatchMode=yes", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4",
		machine.Box, "bash -c "+shellQuote(slotScript)+" loom-slot "+shellQuote(machine.Class)+" "+shellQuote(machine.Runner))
	command.Stdin = bytes.NewReader(body)
	command.Stdout = events
	command.Stderr = &diagnostics
	err = command.Run()
	// The runner exits 1 for a failed unit and 2 for a broken one; that is in its events, not an error here.
	if exitError, ok := err.(*exec.ExitError); ok && (exitError.ExitCode() == 1 || exitError.ExitCode() == 2) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("ssh %s: %v: %s", machine.Box, err, lastLine(diagnostics.String()))
	}
	return nil
}

func lastLine(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	return lines[len(lines)-1]
}

// shellQuote quotes one word for a POSIX shell.
func shellQuote(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// Probe asks a box its platform, as Go spells it ("linux/amd64"), and its whole CPU count.
func Probe(probeContext context.Context, box string) (string, int, error) {
	output, err := exec.CommandContext(probeContext, "ssh", "-o", "BatchMode=yes", box, "uname -sm; nproc --all").Output()
	if err != nil {
		return "", 0, fmt.Errorf("ssh %s uname: %w", box, err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 3 {
		return "", 0, fmt.Errorf("%s says it is %q", box, output)
	}
	cores, err := strconv.Atoi(fields[2])
	if err != nil {
		return "", 0, fmt.Errorf("%s says it has %q CPUs", box, fields[2])
	}
	system := strings.ToLower(fields[0])
	architecture := map[string]string{"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}[fields[1]]
	if architecture == "" {
		return "", 0, fmt.Errorf("%s has an architecture Loom doesn't build for: %s", box, fields[1])
	}
	return system + "/" + architecture, cores, nil
}

// Install puts a runner binary on a box at remotePath (relative to the login's home) unless it is already
// there. The path is version-named, so an installed version is never changed underneath a running unit.
func Install(installContext context.Context, box string, localBinary string, remotePath string) (installed bool, err error) {
	check := exec.CommandContext(installContext, "ssh", "-o", "BatchMode=yes", box, "test -x "+remotePath)
	if check.Run() == nil {
		return false, nil
	}
	directory := remotePath[:strings.LastIndex(remotePath, "/")]
	if output, err := exec.CommandContext(installContext, "ssh", "-o", "BatchMode=yes", box, "mkdir -p "+directory).CombinedOutput(); err != nil {
		return false, fmt.Errorf("ssh %s mkdir: %v: %s", box, err, output)
	}
	temporary := remotePath + ".partial"
	if output, err := exec.CommandContext(installContext, "scp", "-q", "-o", "BatchMode=yes", localBinary, box+":"+temporary).CombinedOutput(); err != nil {
		return false, fmt.Errorf("scp to %s: %v: %s", box, err, output)
	}
	if output, err := exec.CommandContext(installContext, "ssh", "-o", "BatchMode=yes", box, "chmod 755 "+temporary+" && mv "+temporary+" "+remotePath).CombinedOutput(); err != nil {
		return false, fmt.Errorf("ssh %s install: %v: %s", box, err, output)
	}
	return true, nil
}
