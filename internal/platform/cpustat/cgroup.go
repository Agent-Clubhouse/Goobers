package cpustat

import (
	"bytes"
	"os"
	"path/filepath"

	"github.com/goobers/goobers/internal/platform/cgroupfs"
)

// defaultCgroupRoot is where both cgroup generations are mounted in a
// container. Tests read a fake root instead, so this package's parsing is
// exercised on every platform rather than only where a real cgroup exists.
const defaultCgroupRoot = cgroupfs.DefaultRoot

// nanosPerMicro converts cgroup v1's throttled_time (nanoseconds) to the
// microseconds v2 reports, so Cgroup carries one unit regardless of generation.
const nanosPerMicro = 1_000

// readCgroup returns the process's CPU cgroup reading, or nil if none can be
// read. Every failure path is nil rather than an error, for the same reason
// memstat's is: a caller sampling this per stage launch and per heartbeat must
// not have to decide what to do about a missing cgroup, and "not in a
// container" is the common case on a developer machine.
func readCgroup(root string) *Cgroup {
	if root == "" {
		return nil
	}
	if cgroup := readCgroupV2(root); cgroup != nil {
		return cgroup
	}
	// v1 splits controllers into subdirectories, but a container runtime may
	// bind-mount the cpu controller straight onto the root. Try both, in the
	// same order and for the same reason memstat tries them for memory.
	if cgroup := readCgroupV1(filepath.Join(root, "cpu")); cgroup != nil {
		return cgroup
	}
	// A v1 host that mounts cpu and cpuacct as one comounted hierarchy names
	// the directory "cpu,cpuacct". It carries the identical files.
	if cgroup := readCgroupV1(filepath.Join(root, "cpu,cpuacct")); cgroup != nil {
		return cgroup
	}
	return readCgroupV1(root)
}

// readCgroupV2 parses the unified hierarchy: cpu.max holds "<quota> <period>"
// on one line, with the literal "max" in the quota field when unlimited.
func readCgroupV2(dir string) *Cgroup {
	data, err := os.ReadFile(filepath.Join(dir, "cpu.max"))
	if err != nil {
		return nil
	}
	quotaField, periodField, found := bytes.Cut(bytes.TrimSpace(data), []byte(" "))
	if !found {
		return nil
	}
	period, ok := cgroupfs.ParseUint(periodField)
	if !ok || period == 0 {
		// A zero or unparseable period would make the quota meaningless — and,
		// as a divisor, dangerous. Report no reading rather than a wrong one.
		return nil
	}
	cgroup := &Cgroup{PeriodUSec: period}
	// "max" means unlimited, which ParseUint rejects — leaving QuotaUSec zero,
	// which is exactly what unlimited means here.
	if quota, ok := cgroupfs.ParseUint(quotaField); ok {
		cgroup.QuotaUSec = quota
	}
	readThrottling(cgroup, filepath.Join(dir, "cpu.stat"))
	return cgroup
}

// readCgroupV1 parses the legacy hierarchy, where the same two quantities live
// in separate files and an unlimited quota is the sentinel -1 (which ParseUint
// rejects, leaving QuotaUSec zero).
func readCgroupV1(dir string) *Cgroup {
	period, ok := cgroupfs.ReadUint(filepath.Join(dir, "cpu.cfs_period_us"))
	if !ok || period == 0 {
		return nil
	}
	cgroup := &Cgroup{PeriodUSec: period}
	if quota, ok := cgroupfs.ReadUint(filepath.Join(dir, "cpu.cfs_quota_us")); ok {
		cgroup.QuotaUSec = quota
	}
	readThrottling(cgroup, filepath.Join(dir, "cpu.stat"))
	return cgroup
}

// readThrottling fills in the CFS throttling counters. Both generations name
// the period counters identically and differ only in the accumulated-time key
// and its unit, so one reader covers both and Cgroup carries microseconds
// either way.
func readThrottling(cgroup *Cgroup, path string) {
	stat := cgroupfs.ReadKeyed(path)
	cgroup.Periods = stat["nr_periods"]
	cgroup.ThrottledPeriods = stat["nr_throttled"]
	if usec, ok := stat["throttled_usec"]; ok {
		cgroup.ThrottledUSec = usec
		return
	}
	// v1's throttled_time is nanoseconds. Divide rather than multiply so the
	// v2 branch above stays exact and this one merely loses sub-microsecond
	// precision no operator reads.
	if nsec, ok := stat["throttled_time"]; ok {
		cgroup.ThrottledUSec = nsec / nanosPerMicro
	}
}
