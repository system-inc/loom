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
	return stat.Ino, stat.Ctim.Nano()
}
