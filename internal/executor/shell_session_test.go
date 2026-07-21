//go:build unix

package executor

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"testing"

	"golang.org/x/sys/unix"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// sessionMarkerRE parses the `SESSIONCHECK sid=<n> pid=<n>` line the
// re-exec'd helper below prints.
var sessionMarkerRE = regexp.MustCompile(`SESSIONCHECK sid=(-?\d+) pid=(\d+)`)

// TestHelperReportsSession is not a real test — it is the child program the
// spawn-detachment guard below re-execs (via this test binary with
// -test.run filtering it to just this function). It prints its own session id
// (syscall.Getsid) and pid so the parent can assert the child became a session
// leader. It runs harmlessly as part of the normal suite too: it only reports,
// asserting nothing about its own (un-spawned) session.
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

// TestShellExecutor_SpawnsStageInNewSession is the H1 regression guard for the
// #845 "local-ci hang". The executor MUST spawn every deterministic stage into
// its own session (SysProcAttr.Setsid), detached from the daemon's controlling
// terminal. A stage spawned with the pre-fix Setpgid is a background process
// group that still shares the daemon's session and controlling terminal, and
// the kernel STOPs it (SIGTTOU/SIGTTIN, state T, zero CPU) the moment it
// touches terminal state — the outage this whole incident was about.
//
// The assertion is session leadership: syscall.Setsid makes the spawned child
// its own session leader, so its session id equals its pid; a Setpgid child
// instead inherits the spawner's session, so sid != pid. This holds whether or
// not the test process itself has a controlling terminal, so the guard is real
// under CI (no tty) as well as interactively. Revert Setsid→Setpgid in
// shell.go and sid != pid makes this fail.
func TestShellExecutor_SpawnsStageInNewSession(t *testing.T) {
	e, rec := newTestExecutor(t, nil)
	env := baseEnvelope(t)

	// Re-exec this very test binary, filtered to the reporter helper, through
	// the real stage-spawn path. The executor does not pass os.Environ
	// through (SEC-045) and there is no TestMain, so this runs the helper and
	// nothing else.
	result, err := e.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{os.Args[0], "-test.run=^TestHelperReportsSession$", "-test.v"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultSuccess {
		t.Fatalf("status = %v, want success (result: %+v)", result.Status, result)
	}

	sid, pid := parseSessionMarker(t, string(rec.recorded["task-1/stdout.log"]))
	if sid != pid {
		t.Fatalf("spawned stage sid=%d != pid=%d — it is not a session leader, so it was spawned with Setpgid, not Setsid; terminal job control can freeze it (#845 regression)", sid, pid)
	}
}

// parseSessionMarker extracts the sid and pid from a captured transcript that
// contains the helper's SESSIONCHECK line.
func parseSessionMarker(t *testing.T, out string) (sid, pid int) {
	t.Helper()
	m := sessionMarkerRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("output %q did not contain a parseable SESSIONCHECK line", out)
	}
	sid, _ = strconv.Atoi(m[1])
	pid, _ = strconv.Atoi(m[2])
	return sid, pid
}
