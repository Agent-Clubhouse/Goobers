//go:build integration && !windows

package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/test/testsupport/testdep"
)

// TestIntegrationShellExecutor_TimeoutGivesUpOnEscapedDescendant is the
// regression test for #119's WaitDelay gap: a grandchild that escapes the
// process group (via job control's own new-pgid-per-background-job behavior,
// the portable stand-in for setsid) survives the group kill and keeps the
// stdout pipe open, so cmd.Wait() would never return on its own. Run must still
// return within groupKillWaitDelay of the timeout rather than hanging for the
// escaped process's full lifetime.
func TestIntegrationShellExecutor_TimeoutGivesUpOnEscapedDescendant(t *testing.T) {
	testdep.Require(t, "bash", "sleep")

	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "100ms"}

	start := time.Now()
	// `set -m` gives the backgrounded sleep its own process group — the
	// portable equivalent of setsid — it outlives bash's own near-immediate
	// exit and is never reached by the group kill (bash's group, not its
	// own). 30s comfortably exceeds groupKillWaitDelay (5s), so the test can
	// only pass via the give-up bound, not by the escaped process happening
	// to exit on its own first.
	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"bash", "-c", "set -m; sleep 30 & sleep 0.1"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// The escaped descendant ignores/outruns both signals, so Run gives up
	// after the timeout SIGQUIT grace AND the SIGKILL give-up bound:
	// ~timeout + timeoutDumpGrace + groupKillWaitDelay. Still bounded — it does
	// not hang for the escaped process's full 30s lifetime.
	if elapsed > timeoutDumpGrace+groupKillWaitDelay+3*time.Second {
		t.Fatalf("Run took %s, want under ~%s (timeout + timeoutDumpGrace + groupKillWaitDelay) — the give-up bound did not engage", elapsed, 100*time.Millisecond+timeoutDumpGrace+groupKillWaitDelay)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("error = %+v, want timeout", result.Error)
	}
}

// TestIntegrationShellExecutor_DistinguishesCancelFromTimeout is #122's
// low-priority defense-in-depth item: runCtx.Done() fires both when its own
// timeout elapses and when the caller's ctx is externally canceled, and the
// two must not be conflated — a canceled ctx should never come back as the
// "timeout" error code. internal/runner's dispatch always uses
// context.WithoutCancel today, so this path is otherwise unreachable in
// production; the test drives it directly by canceling ctx itself rather than
// through the runner.
func TestIntegrationShellExecutor_DistinguishesCancelFromTimeout(t *testing.T) {
	testdep.Require(t, "sleep")

	shellExec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "10s"} // comfortably longer than the external cancel

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond) // Intentional delayed cancellation exercises shell process termination.
		cancel()
	}()

	result, err := shellExec.Run(ctx, env, apiv1.DeterministicRun{Command: []string{"sleep", "5"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "canceled" || result.Error.Retryable {
		t.Fatalf("error = %+v, want canceled, non-retryable", result.Error)
	}
}

func TestIntegrationShellExecutor_TimeoutRemovesOriginalProcessGroup(t *testing.T) {
	testdep.Require(t, "ps", "sh", "sleep")

	e, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputTimeout: "200ms"}

	result, err := e.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `
echo $$ > "$PIDDIR/stage.pid"
trap '' QUIT
sleep 30 >/dev/null 2>&1 &
echo $! > "$PIDDIR/child.pid"
trap 'exit 0' QUIT
while :; do :; done
`},
		Env: map[string]string{"PIDDIR": env.Workspace},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure || result.Error == nil || result.Error.Code != "timeout" {
		t.Fatalf("status=%v error=%+v, want timeout failure", result.Status, result.Error)
	}

	stagePID := readProcessPID(t, filepath.Join(env.Workspace, "stage.pid"))
	childPID := readProcessPID(t, filepath.Join(env.Workspace, "child.pid"))
	t.Cleanup(func() {
		process, err := os.FindProcess(childPID)
		if err == nil {
			_ = process.Kill()
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for {
		alive, err := processGroupExists(stagePID)
		if err != nil {
			t.Fatalf("probe process group %d: %v", stagePID, err)
		}
		if !alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process with timed-out stage's original process-group id %d remains alive", stagePID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func processGroupExists(pgid int) (bool, error) {
	out, err := exec.Command("ps", "-axo", "pgid=").Output()
	if err != nil {
		return false, err
	}
	want := strconv.Itoa(pgid)
	for _, field := range strings.Fields(string(out)) {
		if field == want {
			return true, nil
		}
	}
	return false, nil
}

func readProcessPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read process pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse process pid %q: %v", data, err)
	}
	return pid
}

func TestIntegrationShellExecutorFailureEvidence(t *testing.T) {
	testdep.Require(t, "sh")
	for _, finding := range []string{
		"main.go:4:1: exported method needs comment (revive)",
		"main.go:4: run: cyclomatic complexity grew from 20 to 21",
		"Unfamiliar analyser requires a target in the manifest",
	} {
		t.Run(finding, func(t *testing.T) {
			executor, recorder := newTestExecutor(t, nil)
			env := baseEnvelope(t)
			stdout := strings.Repeat("successful earlier check\n", 2000) + "==> custom-check\n" + finding + "\n<== custom-check (elapsed 1s)\n"
			result, err := executor.Run(context.Background(), env, apiv1.DeterministicRun{
				Command: []string{"sh", "-c", `printf '%s' "$TEST_FAILURE_STDOUT"; printf '%s' "$TEST_FAILURE_STDERR" >&2; exit 1`},
				Env:     map[string]string{"TEST_FAILURE_STDOUT": stdout, "TEST_FAILURE_STDERR": "ci: custom-check: exit status 1\nmake: *** [ci] Error 1\n"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != apiv1.ResultFailure || result.Error.Code != "nonzero_exit" {
				t.Fatalf("result = %+v", result)
			}
			digest, _ := result.Outputs[outputFailureDigest].(string)
			if !strings.Contains(digest, finding) || len(digest) > maxFailureDigestBytes {
				t.Fatalf("digest = %q", digest)
			}
			if count, ok := result.Outputs[outputFailureCount].(float64); !ok || count < 1 {
				t.Fatalf("missing count: %+v", result.Outputs)
			}
			path, _ := result.Outputs[outputFailureArtifact].(string)
			data := recorder.recorded[path]
			start, end := int(result.Outputs[outputFailureStartByte].(float64)), int(result.Outputs[outputFailureEndByte].(float64))
			if start < 0 || end > len(data) || !strings.Contains(string(data[start:end]), finding) {
				t.Fatalf("artifact %q bytes %d-%d miss finding", path, start, end)
			}
		})
	}
}

func TestIntegrationShellExecutorFailureEvidenceOnStderrWithStdoutFraming(t *testing.T) {
	testdep.Require(t, "sh")
	executor, recorder := newTestExecutor(t, nil)
	finding := "Widget configuration rejected: add a target to the manifest"
	result, err := executor.Run(context.Background(), baseEnvelope(t), apiv1.DeterministicRun{
		Command: []string{"sh", "-c", `printf '%s\n' '==> custom-check' 'Analysing configuration...' '<== custom-check (elapsed 1s)'; printf '%s\n' "$TEST_DIAGNOSIS" 'ci: custom-check: exit status 1' 'make: *** [ci] Error 1' >&2; exit 1`},
		Env:     map[string]string{"TEST_DIAGNOSIS": finding},
	})
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := result.Outputs[outputFailureDigest].(string)
	if !strings.Contains(digest, finding) || !strings.Contains(result.Summary, finding) {
		t.Fatalf("lost stderr finding: %+v", result)
	}
	path, _ := result.Outputs[outputFailureArtifact].(string)
	start, end := int(result.Outputs[outputFailureStartByte].(float64)), int(result.Outputs[outputFailureEndByte].(float64))
	if !strings.Contains(string(recorder.recorded[path][start:end]), finding) {
		t.Fatal("artifact pointer lost stderr diagnostic")
	}
}
