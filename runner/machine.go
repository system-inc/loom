package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// A machine is what a started event says about where the unit runs.
type machine struct {
	name            string
	cpus            int
	memoryMegabytes int
}

// describeMachine reports the hostname, the CPUs the unit can use (the cgroup's quota when it is lower
// than runtime.NumCPU) and the memory it can use (the cgroup's limit when lower than physical memory).
func describeMachine() machine {
	name, _ := os.Hostname()
	cpus := runtime.NumCPU()
	if quota := cgroupCpus(); quota > 0 && quota < cpus {
		cpus = quota
	}
	memory := physicalMemoryBytes()
	if limit := cgroupMemoryBytes(); limit > 0 && (memory == 0 || limit < memory) {
		memory = limit
	}
	return machine{name: name, cpus: cpus, memoryMegabytes: int(memory >> 20)}
}

// selfSha256 is the running binary's sha256, 64 lowercase hex, read once from /proc/self/exe (the executable's path
// where there is no /proc). A unit's key names its runner by this hash (tools.runner), so the started event carries it
// and the judge can refuse an attempt run by a runner its key wasn't made for (Loom, 01:52Z Oct 10). "" when unreadable.
var selfSha256 = sync.OnceValue(func() string {
	path := "/proc/self/exe"
	if _, err := os.Stat(path); err != nil {
		if path, err = os.Executable(); err != nil {
			return ""
		}
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
})

// parseCpuMax reads cgroup v2's cpu.max, "<quota> <period>" in microseconds or "max <period>" for no
// quota, and returns the quota in whole CPUs rounded up, or 0 for none.
func parseCpuMax(text string) int {
	fields := strings.Fields(text)
	if len(fields) != 2 || fields[0] == "max" {
		return 0
	}
	return cpusFromQuota(fields[0], fields[1])
}

// cpusFromQuota divides a CFS quota by its period, rounding up; a negative or unreadable quota is none.
func cpusFromQuota(quotaText string, periodText string) int {
	quota, err := strconv.ParseFloat(strings.TrimSpace(quotaText), 64)
	if err != nil || quota <= 0 {
		return 0
	}
	period, err := strconv.ParseFloat(strings.TrimSpace(periodText), 64)
	if err != nil || period <= 0 {
		return 0
	}
	return int(math.Ceil(quota / period))
}

// parseMemoryLimit reads a cgroup memory limit in bytes; "max" or an absurd v1 sentinel is none.
func parseMemoryLimit(text string) uint64 {
	value, err := strconv.ParseUint(strings.TrimSpace(text), 10, 64)
	if err != nil || value >= 1<<60 {
		return 0
	}
	return value
}

// parseMeminfoTotal reads MemTotal from /proc/meminfo, given in kB.
func parseMeminfoTotal(text string) uint64 {
	for _, line := range strings.Split(text, "\n") {
		if rest, found := strings.CutPrefix(line, "MemTotal:"); found {
			fields := strings.Fields(rest)
			if len(fields) == 0 {
				return 0
			}
			kilobytes, err := strconv.ParseUint(fields[0], 10, 64)
			if err != nil {
				return 0
			}
			return kilobytes << 10
		}
	}
	return 0
}
