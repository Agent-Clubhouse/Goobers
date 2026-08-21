//go:build linux

package proc

import (
	"os"
	"strconv"
	"strings"
)

// isZombie reports whether pid names a zombie process: it has exited but its
// parent has not yet wait()ed on it, so it still occupies a process-table
// slot and a signal-0 probe keeps succeeding for it (#3395). probeAlive
// treats a zombie as gone — in a container whose pid 1 is not a reaping
// init, KillTree's targets die and reparent to pid 1, nobody reaps them, and
// a zombie-blind probe reads them as alive forever even though the kill path
// worked. The parsing mirrors starttime_linux.go: state is the field right
// after the parenthesized comm, found via the last ")" since comm itself may
// contain spaces or parens.
func isZombie(pid int) bool {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	text := string(data)
	closeParen := strings.LastIndexByte(text, ')')
	if closeParen < 0 || closeParen+2 > len(text) {
		return false
	}
	fields := strings.Fields(text[closeParen+2:])
	if len(fields) == 0 {
		return false
	}
	return fields[0] == "Z"
}
