package runner

import (
	"io/fs"
	"syscall"
)

// inodeAndChange is an entry's inode and its change time in nanoseconds, which no process can set back.
func inodeAndChange(info fs.FileInfo) (uint64, int64) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return stat.Ino, stat.Ctimespec.Nano()
}

// sharedWritesUnseen reports whether a write through a shared memory map can change a file on path's filesystem and
// leave its times as they were. On a Mac's filesystems it can't (a review, Oct 10: APFS took such a write's time).
func sharedWritesUnseen(path string) bool {
	return false
}
