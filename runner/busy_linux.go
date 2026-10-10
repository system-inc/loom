package runner

import (
	"os"
	"strconv"
	"strings"
)

// cpuTimes is the machine's CPU time so far, from /proc/stat's first line: all of it and the idle part (idle and
// iowait), in clock ticks. ok is false when unreadable.
func cpuTimes() (total, idle uint64, ok bool) {
	content, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(content), "\n")
	fields := strings.Fields(line)
	if len(fields) < 6 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for index, field := range fields[1:] {
		value, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		// guest and guest_nice are already counted in user and nice.
		if index < 8 {
			total += value
		}
		if index == 3 || index == 4 {
			idle += value
		}
	}
	return total, idle, true
}
