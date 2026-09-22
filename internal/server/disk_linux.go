//go:build linux

package server

import "syscall"

// diskUsage reports the size of the filesystem holding path and the bytes an
// unprivileged process may still write to it.
func diskUsage(path string) (total, free uint64, ok bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, false
	}
	bsize := uint64(fs.Bsize)
	return fs.Blocks * bsize, fs.Bavail * bsize, true
}
