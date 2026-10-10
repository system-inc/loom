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

// tmpfsMagic is tmpfs's filesystem type, as statfs says it.
const tmpfsMagic = 0x01021994

// sharedWritesUnseen reports whether a write through a shared memory map can change a file on path's filesystem and
// leave its change and modification times as they were: on tmpfs it can, msync or not (a review, Oct 10), so a tree
// kept there can't be checked unchanged. A filesystem statfs can't read is taken for one.
func sharedWritesUnseen(path string) bool {
	var stat syscall.Statfs_t
	if syscall.Statfs(path, &stat) != nil {
		return true
	}
	return stat.Type == tmpfsMagic
}
