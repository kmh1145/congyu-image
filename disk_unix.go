//go:build !windows

package main

import "syscall"

func diskCapacity(root string, used int64) (int64, int64, int64) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(root, &stat); err != nil {
		return 0, used, 0
	}
	total := int64(stat.Blocks) * int64(stat.Bsize)
	free := int64(stat.Bavail) * int64(stat.Bsize)
	return total, used, free
}
