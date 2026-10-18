//go:build !windows

package sysinfo

import "syscall"

// DiskUsage returns total and free bytes of the filesystem containing path.
func DiskUsage(path string) (total, free int64, err error) {
	var st syscall.Statfs_t
	if err = syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize), nil
}
