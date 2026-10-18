//go:build windows

package sysinfo

import "golang.org/x/sys/windows"

// DiskUsage returns total and free bytes of the volume containing path.
func DiskUsage(path string) (total, free int64, err error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	var freeAvail, tot, totFree uint64
	if err = windows.GetDiskFreeSpaceEx(p, &freeAvail, &tot, &totFree); err != nil {
		return 0, 0, err
	}
	return int64(tot), int64(freeAvail), nil
}
