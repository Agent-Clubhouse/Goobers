// Package cgroupfs is the shared reader for the container control-group files
// the daemon's diagnostics parse.
//
// Two packages read cgroups for two different limits — memstat for the memory
// limit the OOM killer enforces (#3949), cpustat for the CPU quota the CFS
// throttler enforces (#3963) — and both need the same three things: where the
// cgroup is mounted, how to read a file holding one integer, and how to read
// the "<key> <value>" line format both generations use for their stat files.
// Keeping one copy is the same discipline internal/procenv exists for (#248):
// two hand-kept-in-sync copies of a parser drift, and a diagnostic that
// silently disagrees with itself about which cgroup it read is worse than no
// diagnostic.
package cgroupfs

import (
	"bufio"
	"bytes"
	"os"
	"strconv"
)

// DefaultRoot is where both cgroup generations are mounted in a container.
// Tests read a fake root instead, so parsing is exercised on every platform
// rather than only where a real cgroup exists.
const DefaultRoot = "/sys/fs/cgroup"

// ReadUint reads a file holding a single decimal integer. A missing file, a
// non-numeric body ("max"), or a negative value all report not-ok, so a caller
// never has to distinguish "absent" from "unparseable" — neither yields a
// number it could report.
func ReadUint(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	return ParseUint(bytes.TrimSpace(data))
}

// ParseUint parses one decimal integer field, reporting not-ok for anything
// that is not one — the sentinel words ("max"), the negative sentinels ("-1"),
// and empty fields alike.
func ParseUint(field []byte) (uint64, bool) {
	value, err := strconv.ParseUint(string(bytes.TrimSpace(field)), 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

// ReadKeyed parses the "<key> <value>" line format both cgroup generations use
// for their stat files. Unparseable lines are skipped rather than failing the
// read: these files gain keys across kernel versions, and one unrecognized
// line must not cost the caller the keys it did understand.
func ReadKeyed(path string) map[string]uint64 {
	values, _ := ReadKeyedFile(path)
	return values
}

// ReadKeyedFile is ReadKeyed with the fact that the file could not be opened
// at all. Most callers do not need it, because for a stat file an absent key
// and a zero key mean the same thing. It matters where they do not: memstat's
// at-limit counter is only exported by cgroup v2, and a caller that treats
// "this kernel cannot tell you" as "this cgroup is idle" reports an all-clear
// it never measured. The map is always non-nil.
func ReadKeyedFile(path string) (map[string]uint64, bool) {
	values := make(map[string]uint64)
	data, err := os.ReadFile(path)
	if err != nil {
		return values, false
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		key, rest, found := bytes.Cut(bytes.TrimSpace(scanner.Bytes()), []byte(" "))
		if !found {
			continue
		}
		value, ok := ParseUint(rest)
		if !ok {
			continue
		}
		values[string(key)] = value
	}
	return values, true
}
