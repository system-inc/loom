//go:build !linux

package builder

import "syscall"

// blockSize is the unit the BSDs and macOS count free blocks in, Bsize: their statfs has no separate fragment size.
func blockSize(stat *syscall.Statfs_t) uint64 {
	return uint64(stat.Bsize)
}
