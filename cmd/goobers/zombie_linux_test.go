//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

// isZombie reports whether pid names a zombie process: it has exited but its
// parent has not yet wait()ed on it, so it still occupies a process-table
// slot and a signal-0 probe keeps succeeding for it (#3395). It mirrors
// internal/platform/proc's zombie_linux_test.go helper; duplicated locally
// because it is test-only and package-private on both sides. The parsing
// mirrors starttime_linux.go: state is the field right after the
// parenthesized comm, found via the last ")" since comm itself may contain
// spaces or parens.
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
