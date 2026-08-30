package memstat

import (
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/cgroupfs"
)

// defaultCgroupRoot is where both cgroup generations are mounted in a
// container. Tests read a fake root instead, so this package's parsing is
// exercised on every platform rather than only where a real cgroup exists.
// internal/platform/cpustat reads the CPU quota from the same root through the
// same shared parsers, so the two halves of one incident can never disagree
// about which cgroup they described.
const defaultCgroupRoot = cgroupfs.DefaultRoot

// v1UnlimitedFloor is the floor above which a cgroup v1 memory.limit_in_bytes
// means "unlimited". v1 has no sentinel word: it stores page count × page size,
// so the value is the largest page-aligned number that fits — which differs by
// page size (9223372036854771712 at 4 KiB, and larger alignments on kernels
// with bigger pages). Comparing against a floor rather than one exact constant
// keeps every such variant classified as unlimited.
const v1UnlimitedFloor = uint64(1) << 62

// readCgroup returns the process's memory cgroup reading, or nil if none can be
// read. Every failure path is nil rather than an error: a caller sampling this
// on a timer must not have to decide what to do about a missing cgroup, and
// "not in a container" is the common case on a developer machine.
func readCgroup(root string) *Cgroup {
	if root == "" {
		return nil
	}
	if cgroup := readCgroupV2(root); cgroup != nil {
		return cgroup
	}
	// v1 splits controllers into subdirectories, but a container runtime may
	// bind-mount the memory controller straight onto the root. Try both.
	if cgroup := readCgroupV1(filepath.Join(root, "memory")); cgroup != nil {
		return cgroup
	}
	return readCgroupV1(root)
}

func readCgroupV2(dir string) *Cgroup {
	current, ok := cgroupfs.ReadUint(filepath.Join(dir, "memory.current"))
	if !ok {
		return nil
	}
	cgroup := &Cgroup{Current: current}
	// v2 writes the literal "max" when unlimited, which cgroupfs.ReadUint rejects —
	// leaving Limit zero, which is exactly what unlimited means here.
	if limit, ok := cgroupfs.ReadUint(filepath.Join(dir, "memory.max")); ok {
		cgroup.Limit = limit
	}
	stat := cgroupfs.ReadKeyed(filepath.Join(dir, "memory.stat"))
	cgroup.Anon = stat["anon"]
	cgroup.File = stat["file"]
	// memory.events uses the same "<key> <value>" format as memory.stat.
	// Absent keys read as zero, which is the correct reading for a kernel
	// that does not export them.
	events := cgroupfs.ReadKeyed(filepath.Join(dir, "memory.events"))
	cgroup.AtLimit = events["max"]
	cgroup.OOMKills = events["oom_kill"]
	return cgroup
}

func readCgroupV1(dir string) *Cgroup {
	current, ok := cgroupfs.ReadUint(filepath.Join(dir, "memory.usage_in_bytes"))
	if !ok {
		return nil
	}
	cgroup := &Cgroup{Current: current}
	if limit, ok := cgroupfs.ReadUint(filepath.Join(dir, "memory.limit_in_bytes")); ok && limit < v1UnlimitedFloor {
		cgroup.Limit = limit
	}
	stat := cgroupfs.ReadKeyed(filepath.Join(dir, "memory.stat"))
	// v1 names the same two quantities "rss" and "cache". Mapping them onto
	// Anon and File keeps the reading generation-agnostic for every caller.
	cgroup.Anon = stat["rss"]
	cgroup.File = stat["cache"]
	// v1 has no "max" equivalent, so AtLimit stays zero. It does report
	// kills, under a different filename and alongside non-numeric lines
	// cgroupfs.ReadKeyed skips.
	cgroup.OOMKills = cgroupfs.ReadKeyed(filepath.Join(dir, "memory.oom_control"))["oom_kill"]
	return cgroup
}
