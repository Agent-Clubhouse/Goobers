// Package diskstat reports free space on the filesystem containing a given
// path, for the low-disk protection gate (#4873). Like memstat, a reading
// that cannot be taken is not an error the caller must plumb through — the
// gate this backs is required to fail open, so an unmeasurable filesystem
// must look like "no reading yet" rather than propagate a hard failure that
// could take the daemon down over a diagnostic.
package diskstat

import "fmt"

// Footprint is one point-in-time free-space reading for the filesystem that
// contains Path.
type Footprint struct {
	// Path is the directory the reading was taken for (the instance root),
	// not necessarily the filesystem's mount point.
	Path string
	// TotalBytes is the filesystem's total size.
	TotalBytes uint64
	// FreeBytes is space free on the filesystem, including any portion
	// reserved for privileged processes.
	FreeBytes uint64
	// AvailableBytes is space available to the daemon's own user — usually
	// at or below FreeBytes once a filesystem's superuser reserve is
	// excluded (ext4's default 5%, for instance). This is the number low-disk
	// protection should gate on: it is what the daemon could actually still
	// write, not what a privileged process could.
	AvailableBytes uint64
}

// Read samples free space for the filesystem containing path. An error means
// the measurement could not be taken on this platform or for this path
// (unsupported OS, a missing or unreadable path) — callers must treat that as
// "unknown", not "zero free space", matching memstat.Read's fail-open
// contract for a missing cgroup.
func Read(path string) (Footprint, error) {
	return read(path)
}

// FormatBytes renders a byte count in binary units, matching
// memstat.FormatBytes so a disk reading and a memory reading in the same
// operator-facing message read consistently.
func FormatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	value := float64(n)
	suffixes := []string{"Ki", "Mi", "Gi", "Ti", "Pi"}
	var suffix string
	for _, candidate := range suffixes {
		value /= unit
		suffix = candidate
		if value < unit {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f%s", value, suffix)
	}
	return fmt.Sprintf("%.0f%s", value, suffix)
}
