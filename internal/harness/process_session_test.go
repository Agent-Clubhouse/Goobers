//go:build unix

package harness

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
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
	fmt.Fprintln(os.Stderr, "SESSIONCHECK stderr")
	if os.Getenv("GOOBERS_TEST_FAIL") == "1" {
		fmt.Fprintln(os.Stderr, strings.Repeat("x", 100))
		os.Exit(7)
	}
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
	if !bytes.Contains(res.Stderr, []byte("SESSIONCHECK stderr")) {
		t.Fatalf("stderr = %q, want separately captured child stderr", res.Stderr)
	}
}

func TestExecProcessRunnerCapturesStdoutBeyondTranscriptLimit(t *testing.T) {
	var captured bytes.Buffer
	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command:            []string{os.Args[0], "-test.run=^TestHelperReportsSession$", "-test.v"},
		Timeout:            30 * time.Second,
		MaxTranscriptBytes: 16,
		StdoutCapture:      &captured,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !res.TranscriptTruncated {
		t.Fatal("transcript was not truncated")
	}
	if !harnessSessionMarkerRE.Match(captured.Bytes()) {
		t.Fatalf("stdout capture %q did not retain the complete marker", captured.Bytes())
	}
}

func TestExecProcessRunnerBoundsStderrOnFailure(t *testing.T) {
	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command:            []string{os.Args[0], "-test.run=^TestHelperReportsSession$", "-test.v"},
		Env:                []string{"GOOBERS_TEST_FAIL=1"},
		Timeout:            30 * time.Second,
		MaxTranscriptBytes: 32,
	})
	if err == nil {
		t.Fatal("Run error = nil, want non-zero exit")
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if !res.StderrTruncated || res.StderrDroppedBytes == 0 {
		t.Fatalf("stderr truncation = %v/%d, want truncated output", res.StderrTruncated, res.StderrDroppedBytes)
	}
	if !bytes.Contains(res.Stderr, []byte("[transcript truncated:")) {
		t.Fatalf("Stderr = %q, want truncation marker", res.Stderr)
	}
}
