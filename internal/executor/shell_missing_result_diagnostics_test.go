package executor

import (
	"context"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestShellExecutor_MissingResultFileIncludesExitCode is #711's core
// acceptance for the residual missing_result_file path: a command that
// exits nonzero and never produces its declared result file must carry the
// actual exit code in the message, not the bare "was not produced" that
// used to give an operator nothing to work with.
func TestShellExecutor_MissingResultFileIncludesExitCode(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "never-written.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "exit 7"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "missing_result_file" {
		t.Fatalf("error = %+v, want missing_result_file", result.Error)
	}
	if !strings.Contains(result.Error.Message, "exit code 7") {
		t.Fatalf("message = %q, want it to contain the actual exit code 7", result.Error.Message)
	}
}

// TestShellExecutor_MissingResultFileDistinguishesSignalFromExit is #711's
// "exited 0 but wrote no file must be distinguishable from died to signal N"
// requirement: a process killed by a signal (never got the chance to write
// its declared result file, or exit cleanly at all) must report the signal,
// not a misleading "exit code -1" (Go's exec.ExitError sentinel for a signal
// death, which reads like a normal — if odd — exit code to an operator).
func TestShellExecutor_MissingResultFileDistinguishesSignalFromExit(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "never-written.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "kill -9 $$"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Status != apiv1.ResultFailure {
		t.Fatalf("status = %v, want failure", result.Status)
	}
	if result.Error == nil || result.Error.Code != "missing_result_file" {
		t.Fatalf("error = %+v, want missing_result_file", result.Error)
	}
	if !strings.Contains(result.Error.Message, "killed by signal") {
		t.Fatalf("message = %q, want it to name the signal, not a bare exit code", result.Error.Message)
	}
	if strings.Contains(result.Error.Message, "exit code -1") {
		t.Fatalf("message = %q, must not report the raw -1 exit-code sentinel for a signal death", result.Error.Message)
	}
}

// TestShellExecutor_MissingResultFileIncludesStderrExcerpt proves the
// stderr-excerpt half of #711's diagnostic: whatever the command printed to
// stderr before failing to produce its result file (its real error, a stack
// trace, a "command not found") must be visible in the journaled message,
// not archived only in a separate stderr.log artifact an operator has to go
// dig up.
func TestShellExecutor_MissingResultFileIncludesStderrExcerpt(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "never-written.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{
		Command: []string{"sh", "-c", "echo 'boom: something specific went wrong' >&2; exit 1"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Error == nil || result.Error.Code != "missing_result_file" {
		t.Fatalf("error = %+v, want missing_result_file", result.Error)
	}
	if !strings.Contains(result.Error.Message, "boom: something specific went wrong") {
		t.Fatalf("message = %q, want the stderr excerpt inline", result.Error.Message)
	}
}

// TestShellExecutor_MissingResultFileOmitsEmptyStderrClause proves
// missingResultFileError doesn't append a dangling "; stderr: " when the
// command produced no stderr at all — a command that silently forgot to
// write its file with no error output shouldn't grow a misleading empty
// clause.
func TestShellExecutor_MissingResultFileOmitsEmptyStderrClause(t *testing.T) {
	exec, _ := newTestExecutor(t, nil)
	env := baseEnvelope(t)
	env.Inputs = map[string]interface{}{InputResultFile: "never-written.json"}

	result, err := exec.Run(context.Background(), env, apiv1.DeterministicRun{Command: []string{"sh", "-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Error == nil || result.Error.Code != "missing_result_file" {
		t.Fatalf("error = %+v, want missing_result_file", result.Error)
	}
	if strings.Contains(result.Error.Message, "; stderr: ") {
		t.Fatalf("message = %q, must not carry an empty stderr clause when there was no stderr output", result.Error.Message)
	}
	if !strings.Contains(result.Error.Message, "exit code 0") {
		t.Fatalf("message = %q, want it to show the clean exit 0 that nonetheless produced no file", result.Error.Message)
	}
}

func TestStderrExcerpt_TruncatesAndMarksTruncation(t *testing.T) {
	long := strings.Repeat("x", missingResultFileStderrExcerptBytes+100)
	got := stderrExcerpt([]byte(long))
	if len(got) <= missingResultFileStderrExcerptBytes {
		t.Fatalf("excerpt len = %d, want it truncated at %d plus the marker", len(got), missingResultFileStderrExcerptBytes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("excerpt = %q, want a truncation marker suffix", got)
	}
}

func TestStderrExcerpt_EmptyInputIsEmptyString(t *testing.T) {
	if got := stderrExcerpt(nil); got != "" {
		t.Fatalf("stderrExcerpt(nil) = %q, want empty", got)
	}
}
