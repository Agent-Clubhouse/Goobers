//go:build linux

package proc

import (
	"os"
	"strings"
	"testing"
)

// procStatLine builds a /proc/[pid]/stat line whose comm contains the spaces
// and parens proc(5) permits, so every test here also exercises the
// last-")" split the naive strings.Fields parse gets wrong.
func procStatLine(state, ppid, session, startTicks string) []byte {
	fields := make([]string, 20)
	for i := range fields {
		fields[i] = "0"
	}
	// proc(5) field N, minus the two fields the last-")" split consumes.
	const (
		stateIndex     = 0
		ppidIndex      = 1
		sessionIndex   = 3
		startTimeIndex = 19
	)
	fields[stateIndex] = state
	fields[ppidIndex] = ppid
	fields[sessionIndex] = session
	fields[startTimeIndex] = startTicks
	return []byte("4242 (weird (name) here) " + strings.Join(fields, " ") + "\n")
}

func TestParseProcStatReadsStateParentSessionAndStartTime(t *testing.T) {
	stat, ok := parseProcStat(procStatLine("Z", "7", "91", "123456"))
	if !ok {
		t.Fatal("parseProcStat reported failure on a well-formed line")
	}
	if stat.state != 'Z' {
		t.Errorf("state = %q, want 'Z'", stat.state)
	}
	if stat.ppid != 7 {
		t.Errorf("ppid = %d, want 7", stat.ppid)
	}
	if stat.session != 91 {
		t.Errorf("session = %d, want 91", stat.session)
	}
	if stat.startTicks != 123456 {
		t.Errorf("startTicks = %d, want 123456", stat.startTicks)
	}
}

// TestParseProcStatRejectsUnparseableLines pins the fail-closed direction:
// every caller treats !ok as "unknown", and the reaper in particular must never
// turn a garbled read into a decision to wait for someone else's child.
func TestParseProcStatRejectsUnparseableLines(t *testing.T) {
	for name, line := range map[string]string{
		"empty":              "",
		"no comm parens":     "4242 sh Z 7 4242 91 0 -1 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0 0",
		"truncated fields":   "4242 (sh) Z 7 4242",
		"nothing after comm": "4242 (sh)",
		"non-numeric ppid":   string(procStatLine("Z", "notapid", "91", "123456")),
		"multi-char state":   string(procStatLine("ZZ", "7", "91", "123456")),
	} {
		if _, ok := parseProcStat([]byte(line)); ok {
			t.Errorf("parseProcStat(%s) reported success, want failure", name)
		}
	}
}

func TestReadProcStatReadsSelfAndRejectsNonexistentPIDs(t *testing.T) {
	stat, ok := readProcStat(os.Getpid())
	if !ok {
		t.Fatal("readProcStat(self) reported failure")
	}
	if stat.ppid != os.Getppid() {
		t.Errorf("ppid = %d, want %d", stat.ppid, os.Getppid())
	}
	if stat.state == 'Z' {
		t.Errorf("state = 'Z' for the running test process")
	}
	for _, pid := range []int{0, -1} {
		if _, ok := readProcStat(pid); ok {
			t.Errorf("readProcStat(%d) reported success, want failure", pid)
		}
	}
}
