//go:build windows

package diskstat

import "golang.org/x/sys/windows"

func read(path string) (Footprint, error) {
	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return Footprint{}, err
	}
	var freeAvailable, total, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &freeAvailable, &total, &totalFree); err != nil {
		return Footprint{}, err
	}
	return Footprint{
		Path:           path,
		TotalBytes:     total,
		FreeBytes:      totalFree,
		AvailableBytes: freeAvailable,
	}, nil
}
