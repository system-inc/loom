package runner

import (
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
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
