package runner

import "os"

// The cgroup files read here are the ones at the root of the cgroup filesystem, which is the unit's own
// cgroup inside a container, where quotas matter. On a bare host they are the root cgroup's: no quota.

func cgroupCpus() int {
	if text, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		return parseCpuMax(string(text))
	}
	quota, quotaError := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	period, periodError := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if quotaError != nil || periodError != nil {
		return 0
	}
	return cpusFromQuota(string(quota), string(period))
}

func cgroupMemoryBytes() uint64 {
	if text, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		return parseMemoryLimit(string(text))
	}
	if text, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
		return parseMemoryLimit(string(text))
	}
	return 0
}

func physicalMemoryBytes() uint64 {
	text, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	return parseMeminfoTotal(string(text))
}
