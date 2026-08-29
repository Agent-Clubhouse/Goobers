//go:build linux

package proc

import (
	"os"
	"strconv"
	"strings"
)

// procStat is the subset of /proc/[pid]/stat (proc(5)) this package reads.
//
// Every field below survives the process's death: the kernel keeps the entry
// until someone wait()s for the pid, which is exactly what makes state usable
// as a liveness signal (a zombie is state 'Z') and what lets the orphan reaper
// classify a dead child before it consumes the exit status.
type procStat struct {
	// state is field 3 — 'R', 'S', 'D', 'Z', 'T', …
	state byte
	// ppid is field 4 — for an orphan the kernel reparented, the pid of the
	// subreaper (in a container, pid 1) rather than the original parent.
	ppid int
	// session is field 6 — inherited unchanged across fork/exec and changed
	// only by setsid(2), so it identifies which session-detached tree
	// (proc.Configure) a process descends from.
	session int
	// startTicks is field 22 — clock ticks since boot, the PID-reuse
	// discriminator StartTime is built on.
	startTicks uint64
}

// readProcStat reads and parses /proc/<pid>/stat. ok is false when the entry
// does not exist or cannot be parsed; callers must treat that as "unknown",
// never as evidence about the process.
func readProcStat(pid int) (procStat, bool) {
	if pid <= 0 {
		return procStat{}, false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return procStat{}, false
	}
	return parseProcStat(data)
}

// parseProcStat splits one /proc/[pid]/stat line.
//
// Fields after comm (the process name, field 2) are what proc(5) actually
// guarantees whitespace-delimited: comm itself is parenthesized and may contain
// spaces or even parens, so the only safe split point is the LAST ")" in the
// line — anything a process can name itself cannot contain a ")" followed by
// " (" that looks like a later field boundary.
func parseProcStat(data []byte) (procStat, bool) {
	text := string(data)
	closeParen := strings.LastIndexByte(text, ')')
	if closeParen < 0 || closeParen+2 > len(text) {
		return procStat{}, false
	}
	fields := strings.Fields(text[closeParen+2:])
	// proc(5) numbers fields from 1 and fields[0] here is field 3, so field N
	// lives at index N-3: state 3→0, ppid 4→1, session 6→3, starttime 22→19.
	const (
		stateIndex     = 0
		ppidIndex      = 1
		sessionIndex   = 3
		startTimeIndex = 19
	)
	// Linux has emitted all 52 fields for every task, zombies included, for as
	// long as the file has existed; a short line means the read raced something
	// unparseable rather than a kernel that stops at field 21.
	if len(fields) <= startTimeIndex || len(fields[stateIndex]) != 1 {
		return procStat{}, false
	}
	ppid, err := strconv.Atoi(fields[ppidIndex])
	if err != nil {
		return procStat{}, false
	}
	session, err := strconv.Atoi(fields[sessionIndex])
	if err != nil {
		return procStat{}, false
	}
	startTicks, err := strconv.ParseUint(fields[startTimeIndex], 10, 64)
	if err != nil {
		return procStat{}, false
	}
	return procStat{
		state:      fields[stateIndex][0],
		ppid:       ppid,
		session:    session,
		startTicks: startTicks,
	}, true
}
