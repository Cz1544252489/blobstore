//go:build linux

package server

import "syscall"

// diskUsage reports the filesystem holding path the way df does: its size,
// the bytes in use, and the bytes an unprivileged process may still write.
// Used plus free falls short of total by the blocks reserved for root.
func diskUsage(path string) (total, used, free uint64, ok bool) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, 0, 0, false
	}
	bsize := uint64(fs.Bsize)
	return fs.Blocks * bsize, (fs.Blocks - fs.Bfree) * bsize, fs.Bavail * bsize, true
}
