//go:build linux

package proc

import (
	"bufio"
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
	stat, ok := readProcStat(pid)
	if !ok {
		return time.Time{}, false
	}
	bootTime, ok := linuxBootTime()
	if !ok {
		return time.Time{}, false
	}
	return bootTime.Add(time.Duration(stat.startTicks) * time.Second / linuxClockTicksPerSec), true
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
