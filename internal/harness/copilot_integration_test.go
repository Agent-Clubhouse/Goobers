//go:build integration && !windows

package harness

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goobers/goobers/internal/testdep"
)

func TestIntegrationExecProcessRunnerTimeout(t *testing.T) {
	testdep.Require(t, "sleep")

	runner := ExecProcessRunner{}
	_, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sleep", "5"},
		Timeout: 50 * time.Millisecond,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
}

// TestIntegrationExecProcessRunnerKillsProcessGroup is the regression test for
// #119: a background grandchild that stays in the same process group (the
// common case — job control off) must die with the timeout kill, not survive
// and keep the harness stage's stdout pipe open.
func TestIntegrationExecProcessRunnerKillsProcessGroup(t *testing.T) {
	testdep.Require(t, "sh", "sleep")

	runner := ExecProcessRunner{}
	start := time.Now()
	_, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sh", "-c", "sleep 30 & wait"},
		Timeout: 100 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("Run took %s, want well under the 30s sleep — process group was not killed", elapsed)
	}
}

// TestIntegrationExecProcessRunnerTimeoutGivesUpOnEscapedDescendant is the
// regression test for #119's WaitDelay gap: a grandchild that escapes the
// process group (via job control's own new-pgid-per-background-job behavior,
// the portable stand-in for setsid) survives the group kill and keeps the
// stdout pipe open, so cmd.Wait() would never return on its own. Run must still
// return within groupKillWaitDelay of the timeout rather than hanging for the
// escaped process's full lifetime.
func TestIntegrationExecProcessRunnerTimeoutGivesUpOnEscapedDescendant(t *testing.T) {
	testdep.Require(t, "bash", "sleep")

	runner := ExecProcessRunner{}
	start := time.Now()
	// `set -m` gives the backgrounded sleep its own process group — it
	// outlives bash's own near-immediate exit and is never reached by the
	// group kill (bash's group, not its own). 30s comfortably exceeds
	// groupKillWaitDelay (5s), so the test can only pass via the give-up
	// bound, not by the escaped process happening to exit on its own first.
	_, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"bash", "-c", "set -m; sleep 30 & sleep 0.1"},
		Timeout: 100 * time.Millisecond,
	})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, want ErrTimeout", err)
	}
	if elapsed > 8*time.Second {
		t.Fatalf("Run took %s, want under ~%s (timeout + groupKillWaitDelay) — the give-up bound did not engage", elapsed, 100*time.Millisecond+groupKillWaitDelay)
	}
}

// TestIntegrationExecProcessRunnerDistinguishesCancelFromTimeout is #122's
// low-priority defense-in-depth item: runCtx.Done() fires both when its own
// timeout elapses and when the caller's ctx is externally canceled, and the
// two must not be conflated — a canceled ctx should never come back as
// ErrTimeout. internal/runner's dispatch always uses context.WithoutCancel
// today, so this path is otherwise unreachable in production; the test drives
// it directly by canceling ctx itself rather than through the runner.
func TestIntegrationExecProcessRunnerDistinguishesCancelFromTimeout(t *testing.T) {
	testdep.Require(t, "sleep")

	runner := ExecProcessRunner{}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := runner.Run(ctx, ProcessRequest{
		Command: []string{"sleep", "5"},
		Timeout: 10 * time.Second, // comfortably longer than the external cancel
	})
	if !errors.Is(err, ErrCanceled) {
		t.Fatalf("error = %v, want ErrCanceled", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("error = %v, must not also be ErrTimeout", err)
	}
}

// TestIntegrationExecProcessRunnerDefaultsToEmptyEnv is the regression test
// for #122: os/exec treats a nil Cmd.Env as "inherit this process's
// environment" — ExecProcessRunner must not let that fail-open default through
// when a caller leaves ProcessRequest.Env unset.
func TestIntegrationExecProcessRunnerDefaultsToEmptyEnv(t *testing.T) {
	testdep.Require(t, "sh")

	const ambientSecretVar = "GOOBERS_AMBIENT_DAEMON_SECRET"
	t.Setenv(ambientSecretVar, "ambient-daemon-secret-never-declared")

	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sh", "-c", `test -z "$` + ambientSecretVar + `" && echo absent`},
		// Env deliberately left unset.
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Transcript), "absent") {
		t.Fatalf("ambient daemon env var leaked with Env unset: transcript = %q", res.Transcript)
	}
}

func TestIntegrationExecProcessRunnerCapturesTranscript(t *testing.T) {
	testdep.Require(t, "sh")

	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sh", "-c", "echo hello-stdout; echo hello-stderr 1>&2"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Transcript), "hello-stdout") || !strings.Contains(string(res.Transcript), "hello-stderr") {
		t.Fatalf("transcript = %q", res.Transcript)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
}

// TestIntegrationExecProcessRunnerBoundsTranscript is #245's headline
// regression: an unbounded syncBuffer let a chatty/looping agentic session
// balloon daemon memory and write a multi-hundred-MB span into the journal. A
// command emitting well past MaxTranscriptBytes must yield a Transcript
// bounded at ~the cap (plus the short marker), not one proportional to actual
// output.
func TestIntegrationExecProcessRunnerBoundsTranscript(t *testing.T) {
	testdep.Require(t, "head", "sh", "yes")

	const capBytes = 1024
	const totalWritten = capBytes * 50 // comfortably past the cap

	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command:            []string{"sh", "-c", "yes x | head -c " + strconv.Itoa(totalWritten)},
		MaxTranscriptBytes: capBytes,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TranscriptTruncated {
		t.Fatal("TranscriptTruncated = false, want true")
	}
	if res.TranscriptDroppedBytes <= 0 {
		t.Fatalf("TranscriptDroppedBytes = %d, want > 0", res.TranscriptDroppedBytes)
	}
	if got := int64(totalWritten) - res.TranscriptDroppedBytes; got != capBytes {
		t.Fatalf("retained bytes = %d (total %d - dropped %d), want exactly the cap %d", got, totalWritten, res.TranscriptDroppedBytes, capBytes)
	}
	// Peak retained bytes: the marker adds a small, fixed overhead on top of
	// the cap, nowhere near proportional to totalWritten.
	if len(res.Transcript) > capBytes+128 {
		t.Fatalf("Transcript length = %d, want at most cap (%d) + a small marker overhead", len(res.Transcript), capBytes)
	}
	if !strings.Contains(string(res.Transcript), "transcript truncated") {
		t.Fatalf("Transcript missing truncation marker: %q", res.Transcript[max(0, len(res.Transcript)-80):])
	}
}

// TestIntegrationExecProcessRunnerUnboundedTranscriptStaysUntruncated is the
// negative control for TestIntegrationExecProcessRunnerBoundsTranscript:
// output comfortably under the cap must round-trip untouched, with no marker
// appended and TranscriptTruncated left false.
func TestIntegrationExecProcessRunnerUnboundedTranscriptStaysUntruncated(t *testing.T) {
	testdep.Require(t, "sh")

	runner := ExecProcessRunner{}
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command:            []string{"sh", "-c", "echo small-output"},
		MaxTranscriptBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.TranscriptTruncated {
		t.Fatal("TranscriptTruncated = true, want false for output well under the cap")
	}
	if res.TranscriptDroppedBytes != 0 {
		t.Fatalf("TranscriptDroppedBytes = %d, want 0", res.TranscriptDroppedBytes)
	}
	if strings.Contains(string(res.Transcript), "truncated") {
		t.Fatalf("Transcript unexpectedly carries a truncation marker: %q", res.Transcript)
	}
}

// TestIntegrationExecProcessRunnerDefaultTranscriptCap confirms a caller that
// never sets MaxTranscriptBytes still gets a bounded transcript
// (DefaultMaxTranscriptBytes), not the pre-#245 unbounded behavior.
func TestIntegrationExecProcessRunnerDefaultTranscriptCap(t *testing.T) {
	testdep.Require(t, "head", "sh", "yes")

	runner := ExecProcessRunner{}
	overCap := DefaultMaxTranscriptBytes + 1<<16
	res, err := runner.Run(context.Background(), ProcessRequest{
		Command: []string{"sh", "-c", "yes x | head -c " + strconv.Itoa(int(overCap))},
		// MaxTranscriptBytes deliberately left unset.
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.TranscriptTruncated {
		t.Fatal("TranscriptTruncated = false, want true under the default cap")
	}
}

// TestIntegrationCopilotAdapterRun_LargeTranscriptDoesNotAffectCompletionDetection
// is #245's fail-safe acceptance criterion: truncation must never eat the
// completion signal — the adapter keys success/failure off the completion file
// at CompletionPath, never off the transcript, so a session that floods
// stdout/stderr well past the cap must still round-trip its result payload
// correctly.
func TestIntegrationCopilotAdapterRun_LargeTranscriptDoesNotAffectCompletionDetection(t *testing.T) {
	testdep.Require(t, "dirname", "head", "mkdir", "sh", "yes")

	workspace := t.TempDir()
	adapter := &CopilotAdapter{
		Command: []string{"sh", "-c", `
			yes chatty-agent-output | head -c 65536
			mkdir -p "$(dirname "$1")"
			printf '%s' "$2" > "$1"
		`, "sh", filepath.Join(workspace, DefaultResultPath), `{"status":"success","summary":"done"}`},
	}
	out, err := adapter.Run(context.Background(), RunRequest{
		Workspace:          workspace,
		CompletionPath:     DefaultResultPath,
		MaxTranscriptBytes: 1024,
		Timeout:            5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !out.TranscriptTruncated {
		t.Fatal("TranscriptTruncated = false, want true")
	}
	if len(out.Payload) == 0 {
		t.Fatal("expected a non-empty result payload despite the truncated transcript")
	}
	if !strings.Contains(string(out.Payload), "success") {
		t.Fatalf("Payload = %q, want the completion file's actual content, unaffected by transcript truncation", out.Payload)
	}
}
