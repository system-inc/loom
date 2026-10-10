package runner

// cpuTimes is unread on a Mac, which runs one unit at a time.
func cpuTimes() (total, idle uint64, ok bool) {
	return 0, 0, false
}
