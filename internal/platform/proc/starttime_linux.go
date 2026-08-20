//go:build linux

package proc

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// linuxClockTicksPerSec is sysconf(_SC_CLK_TCK), the unit /proc/[pid]/stat's
// starttime field is expressed in. It is not exposed by golang.org/x/sys/unix,
// but 100 (USER_HZ) has been the universal value across every mainstream
// Linux distribution and architecture for decades regardless of the kernel's
// internal timer frequency (CONFIG_HZ) — hardcoding it is standard practice
// (e.g. util-linux's ps/top do the same) rather than a real portability risk.
const linuxClockTicksPerSec = 100
const startTimeSupported = true

func startTime(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return time.Time{}, false
	}
	// Fields after comm (the process name, field 2) are what proc(5) actually
	// guarantees whitespace-delimited: comm itself is parenthesized and may
	// contain spaces or even parens, so the only safe split point is the
	// LAST ")" in the line — anything a process can name itself cannot
	// contain a ")" followed by " (" that looks like a later field boundary.
	text := string(data)
	closeParen := strings.LastIndexByte(text, ')')
	if closeParen < 0 || closeParen+2 > len(text) {
		return time.Time{}, false
	}
	fields := strings.Fields(text[closeParen+2:])
	// State is field 3 (index 0 in `fields`); starttime is field 22, i.e.
	// index 22-3 = 19.
	const starttimeIndex = 22 - 3
	if len(fields) <= starttimeIndex {
		return time.Time{}, false
	}
	ticks, err := strconv.ParseUint(fields[starttimeIndex], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	bootTime, ok := linuxBootTime()
	if !ok {
		return time.Time{}, false
	}
	return bootTime.Add(time.Duration(ticks) * time.Second / linuxClockTicksPerSec), true
}

func linuxBootTime() (time.Time, bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return time.Time{}, false
	}
	defer func() { _ = f.Close() }()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		sec, ok := strings.CutPrefix(line, "btime ")
		if !ok {
			continue
		}
		seconds, err := strconv.ParseInt(strings.TrimSpace(sec), 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		return time.Unix(seconds, 0), true
	}
	return time.Time{}, false
}
