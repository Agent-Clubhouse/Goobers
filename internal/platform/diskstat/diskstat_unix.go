//go:build linux || darwin

package diskstat

import "golang.org/x/sys/unix"

func read(path string) (Footprint, error) {
	var fs unix.Statfs_t
	if err := unix.Statfs(path, &fs); err != nil {
		return Footprint{}, err
	}
	// Bsize's width differs by platform and architecture (int32/int64/uint32);
	// the explicit uint64 conversion keeps this file identical on both. Blocks,
	// Bfree, and Bavail are already uint64 on both linux and darwin.
	blockSize := uint64(fs.Bsize)
	return Footprint{
		Path:           path,
		TotalBytes:     fs.Blocks * blockSize,
		FreeBytes:      fs.Bfree * blockSize,
		AvailableBytes: fs.Bavail * blockSize,
	}, nil
}
