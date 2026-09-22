//go:build !linux

package server

// diskUsage is only implemented where the service is deployed; elsewhere
// /status leaves the disk field out.
func diskUsage(string) (total, used, free uint64, ok bool) { return 0, 0, 0, false }
