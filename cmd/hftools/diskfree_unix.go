//go:build unix

package main

import "syscall"

// freeBytes reports the free space available to an unprivileged user at path.
func freeBytes(path string) (int64, bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, false
	}
	return int64(fs.Bavail * uint64(fs.Bsize)), true
}
