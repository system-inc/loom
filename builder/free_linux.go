package builder

import "syscall"

// blockSize is the unit Linux counts free blocks in: the fundamental block size, Frsize, which statfs's Bsize (the
// preferred transfer size) need not equal.
func blockSize(stat *syscall.Statfs_t) uint64 {
	return uint64(stat.Frsize)
}
