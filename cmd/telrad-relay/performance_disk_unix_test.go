//go:build (linux || darwin) && !relay_container

package main

import "golang.org/x/sys/unix"

func performanceDiskAvailable(path string) (uint64, error) {
	var stat unix.Statfs_t
	err := unix.Statfs(path, &stat)
	return uint64(stat.Bavail) * uint64(stat.Bsize), err
}
