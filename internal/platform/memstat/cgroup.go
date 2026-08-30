package memstat

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// defaultCgroupRoot is where both cgroup generations are mounted in a
// container. Tests read a fake root instead, so this package's parsing is
// exercised on every platform rather than only where a real cgroup exists.
const defaultCgroupRoot = "/sys/fs/cgroup"

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
	current, ok := readUint(filepath.Join(dir, "memory.current"))
	if !ok {
		return nil
	}
	cgroup := &Cgroup{Current: current}
	// v2 writes the literal "max" when unlimited, which readUint rejects —
	// leaving Limit zero, which is exactly what unlimited means here.
	if limit, ok := readUint(filepath.Join(dir, "memory.max")); ok {
		cgroup.Limit = limit
	}
	stat := readKeyedFile(filepath.Join(dir, "memory.stat"))
	cgroup.Anon = stat["anon"]
	cgroup.File = stat["file"]
	return cgroup
}

func readCgroupV1(dir string) *Cgroup {
	current, ok := readUint(filepath.Join(dir, "memory.usage_in_bytes"))
	if !ok {
		return nil
	}
	cgroup := &Cgroup{Current: current}
	if limit, ok := readUint(filepath.Join(dir, "memory.limit_in_bytes")); ok && limit < v1UnlimitedFloor {
		cgroup.Limit = limit
	}
	stat := readKeyedFile(filepath.Join(dir, "memory.stat"))
	// v1 names the same two quantities "rss" and "cache". Mapping them onto
	// Anon and File keeps the reading generation-agnostic for every caller.
	cgroup.Anon = stat["rss"]
	cgroup.File = stat["cache"]
	return cgroup
}

// readUint reads a file holding a single decimal integer. A missing file, a
// non-numeric body ("max"), or a negative value all report not-ok, so a caller
// never has to distinguish "absent" from "unparseable" — neither yields a
// number it could report.
func readUint(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	value, err := strconv.ParseUint(string(bytes.TrimSpace(data)), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// readKeyedFile parses the "<key> <value>" line format both cgroup generations
// use for memory.stat. Unparseable lines are skipped rather than failing the
// read: memory.stat gains keys across kernel versions, and one unrecognized
// line must not cost the caller the keys it did understand.
func readKeyedFile(path string) map[string]uint64 {
	values := make(map[string]uint64)
	data, err := os.ReadFile(path)
	if err != nil {
		return values
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, rest, found := bytes.Cut(bytes.TrimSpace(scanner.Bytes()), []byte(" "))
		if !found {
			continue
		}
		value, err := strconv.ParseUint(string(bytes.TrimSpace(rest)), 10, 64)
		if err != nil {
			continue
		}
		values[string(key)] = value
	}
	return values
}
