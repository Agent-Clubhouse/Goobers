//go:build unix

package harness

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// harnessSessionMarkerRE parses the `SESSIONCHECK sid=<n> pid=<n>` line the
// re-exec'd helper below prints.
var harnessSessionMarkerRE = regexp.MustCompile(`SESSIONCHECK sid=(-?\d+) pid=(\d+)`)

// TestHelperReportsSession is not a real test — it is the child program the
// spawn-detachment guard below re-execs (this test binary, -test.run filtered
// to just this function). It prints its own session id (syscall.Getsid) and
// pid so the parent can assert the child became a session leader. It runs
// harmlessly in the normal suite too: it only reports.
//
// unix.Getsid, not syscall.Getsid: the latter is Darwin-only in the standard
// library (absent on Linux), so the portable x/sys/unix wrapper is required
// for this to build on CI.
func TestHelperReportsSession(t *testing.T) {
	sid, err := unix.Getsid(0)
	if err != nil {
		t.Fatalf("getsid: %v", err)
	}
	fmt.Printf("SESSIONCHECK sid=%d pid=%d\n", sid, os.Getpid())
}

// TestExecProcessRunnerSpawnsChildInNewSession is the H1 regression guard for
// the #845 "local-ci hang", covering the harness/copilot spawn path (the twin
// of the executor's stage spawn). ExecProcessRunner MUST spawn its child into
// its own session (SysProcAttr.Setsid), detached from the daemon's controlling
// terminal. A child spawned with the pre-fix Setpgid is a background process
// group that still shares the daemon's session and controlling terminal, so
// the kernel STOPs it (SIGTTOU/SIGTTIN, state T) the moment it touches
// terminal state.
//
// The assertion is session leadership: Setsid makes the spawned child its own
// session leader, so its session id equals its pid; a Setpgid child inherits
// the spawner's session, so sid != pid. This is tty-independent, so the guard
// holds in CI too. Revert Setsid→Setpgid in process.go and sid != pid fails.
func TestExecProcessRunnerSpawnsChildInNewSession(t *testing.T) {
	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{os.Args[0], "-test.run=^TestHelperReportsSession$", "-test.v"},
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := harnessSessionMarkerRE.FindStringSubmatch(string(res.Transcript))
	if m == nil {
		t.Fatalf("transcript %q did not contain a parseable SESSIONCHECK line", res.Transcript)
	}
	sid, _ := strconv.Atoi(m[1])
	pid, _ := strconv.Atoi(m[2])
	if sid != pid {
		t.Fatalf("spawned harness child sid=%d != pid=%d — it is not a session leader, so it was spawned with Setpgid, not Setsid; terminal job control can freeze it (#845 regression)", sid, pid)
	}
}
